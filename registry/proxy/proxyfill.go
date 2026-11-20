package proxy

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/reference"
)

const defaultSpoolLookahead = 20 << 20

var errShortFill = errors.New("remote blob ended before its size")

// Fills are keyed by repository too: the remote authorizes each repository, and the cache links blobs per repository.
type fillKey struct {
	repository string
	dgst       digest.Digest
}

type fillRegistry struct {
	spool     *spoolBudget
	lookahead int64

	mu    sync.Mutex
	fills map[fillKey]*blobFill
}

type spoolBudget struct {
	dir     string
	maxSize int64
	used    atomic.Int64
}

var errSpoolFull = errors.New("spool budget exceeded")

func (sb *spoolBudget) create(size int64) (*os.File, error) {
	if used := sb.add(size); sb.maxSize > 0 && used > sb.maxSize {
		sb.add(-size)
		return nil, errSpoolFull
	}
	file, err := os.CreateTemp(sb.dir, "blob-")
	if err != nil {
		sb.add(-size)
		return nil, err
	}
	// Unlink now so the kernel frees the space when the last reader closes the file, even after a crash.
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		sb.add(-size)
		return nil, err
	}
	return file, nil
}

func (sb *spoolBudget) add(size int64) int64 {
	used := sb.used.Add(size)
	spoolBytes.Set(float64(used))
	return used
}

type blobFill struct {
	desc v1.Descriptor
	// Requests read the blob from spool because the cache upload is not readable before its commit.
	spool *os.File
	done  chan struct{}

	refs int // guarded by fillRegistry.mu

	mu       sync.Mutex
	written  int64
	finished bool
	err      error
	progress chan struct{}
}

func (fr *fillRegistry) lookup(key fillKey) *blobFill {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	f := fr.fills[key]
	if f != nil {
		f.refs++
	}
	return f
}

func (fr *fillRegistry) start(ctx context.Context, key fillKey, desc v1.Descriptor) (f *blobFill, started bool) {
	spool := fr.createSpool(ctx, desc.Size)

	fr.mu.Lock()
	if existing := fr.fills[key]; existing != nil {
		existing.refs++
		fr.mu.Unlock()
		fr.closeSpool(spool, desc.Size)
		return existing, false
	}
	f = &blobFill{
		desc:     desc,
		spool:    spool,
		done:     make(chan struct{}),
		refs:     2, // the caller and the fetch goroutine
		progress: make(chan struct{}),
	}
	if fr.fills == nil {
		fr.fills = make(map[fillKey]*blobFill)
	}
	fr.fills[key] = f
	fr.mu.Unlock()
	return f, true
}

func (fr *fillRegistry) createSpool(ctx context.Context, size int64) *os.File {
	if fr.spool == nil {
		return nil
	}
	spool, err := fr.spool.create(size)
	switch {
	case errors.Is(err, errSpoolFull):
		spoolFallbacks.WithValues("budget").Inc(1)
	case err != nil:
		spoolFallbacks.WithValues("error").Inc(1)
		dcontext.GetLogger(ctx).WithError(err).Errorf("Error creating blob spool")
	}
	return spool
}

func (fr *fillRegistry) remove(key fillKey) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	delete(fr.fills, key)
}

func (fr *fillRegistry) release(f *blobFill) {
	fr.mu.Lock()
	f.refs--
	last := f.refs == 0
	fr.mu.Unlock()
	if last {
		fr.closeSpool(f.spool, f.desc.Size)
	}
}

func (fr *fillRegistry) closeSpool(spool *os.File, size int64) {
	if spool == nil {
		return
	}
	spool.Close()
	fr.spool.add(-size)
}

func (fr *fillRegistry) run(ctx context.Context, pbs *proxyBlobStore, key fillKey, f *blobFill) {
	defer fr.release(f)
	defer close(f.done)
	defer fr.remove(key)

	err := pbs.fetch(ctx, f)
	if err != nil {
		dcontext.GetLogger(ctx).WithError(err).Errorf("Error caching blob %s", f.desc.Digest)
	}
	f.finish(err)
}

func (pbs *proxyBlobStore) fetch(ctx context.Context, f *blobFill) error {
	bw, err := pbs.localStore.Create(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if err := bw.Cancel(ctx); err != nil {
				dcontext.GetLogger(ctx).WithError(err).Errorf("Error canceling blob writer")
			}
		}
	}()

	remoteReader, err := pbs.remoteStore.Open(ctx, f.desc.Digest)
	if err != nil {
		return err
	}
	defer remoteReader.Close()

	var dst io.Writer = bw
	if f.spool != nil {
		dst = io.MultiWriter(bw, spoolWriter{f})
	}
	if _, err := io.CopyN(dst, remoteReader, f.desc.Size); err != nil {
		if err == io.EOF {
			return errShortFill
		}
		return err
	}
	proxyMetrics.BlobPull(uint64(f.desc.Size))

	if _, err := bw.Commit(ctx, f.desc); err != nil {
		return err
	}
	committed = true

	blobRef, err := reference.WithDigest(pbs.repositoryName, f.desc.Digest)
	if err != nil {
		return err
	}
	if pbs.scheduler != nil && pbs.ttl != nil {
		return pbs.scheduler.AddBlob(blobRef, *pbs.ttl)
	}
	return nil
}

func (f *blobFill) finish(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = true
	f.err = err
	close(f.progress)
}

func (f *blobFill) canServe(start, lookahead int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spool != nil && f.err == nil && start <= f.written+lookahead
}

func (f *blobFill) waitFor(ctx context.Context, end int64) (int64, error) {
	for {
		f.mu.Lock()
		written, finished, err, progress := f.written, f.finished, f.err, f.progress
		f.mu.Unlock()

		switch {
		case written >= end:
			return written, nil
		case finished:
			return written, cmp.Or(err, errShortFill)
		}
		select {
		case <-progress:
		case <-ctx.Done():
			return written, ctx.Err()
		}
	}
}

type spoolWriter struct {
	f *blobFill
}

func (sw spoolWriter) Write(p []byte) (int, error) {
	n, err := sw.f.spool.Write(p)

	sw.f.mu.Lock()
	sw.f.written += int64(n)
	close(sw.f.progress)
	sw.f.progress = make(chan struct{})
	sw.f.mu.Unlock()
	return n, err
}

type spoolReader struct {
	ctx    context.Context
	f      *blobFill
	offset int64
}

func (sr *spoolReader) Read(p []byte) (int, error) {
	size := sr.f.desc.Size
	if sr.offset >= size {
		return 0, io.EOF
	}
	if int64(len(p)) > size-sr.offset {
		p = p[:size-sr.offset]
	}
	written, err := sr.f.waitFor(sr.ctx, sr.offset+1)
	if err != nil {
		return 0, err
	}
	if int64(len(p)) > written-sr.offset {
		p = p[:written-sr.offset]
	}
	n, err := sr.f.spool.ReadAt(p, sr.offset)
	sr.offset += int64(n)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

func (sr *spoolReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += sr.offset
	case io.SeekEnd:
		offset += sr.f.desc.Size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	sr.offset = offset
	return offset, nil
}
