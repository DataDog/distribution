package proxy

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/distribution/distribution/v3/configuration"
)

// newFillRegistry returns nil when shared fetches are disabled.
func newFillRegistry(config configuration.SharedFetch) (*fillRegistry, error) {
	if !config.Enabled {
		return nil, nil
	}
	dir := cmp.Or(config.SpoolDirectory, os.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create proxy spool directory: %w", err)
	}
	return &fillRegistry{spool: &spoolBudget{dir: dir, maxSize: config.SpoolMaxSize}}, nil
}

func (pbs *proxyBlobStore) serveSharedFetch(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) error {
	f, leader, err := pbs.joinFill(ctx, dgst)
	if err != nil {
		return err
	}
	defer pbs.fills.release(f)

	pbs.serveFill(ctx, w, r, f)
	if !leader {
		return nil
	}
	// A client that leaves while the fill commits is not a server error: the fill continues without it.
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return nil
	}
}

func (pbs *proxyBlobStore) joinFill(ctx context.Context, dgst digest.Digest) (f *blobFill, leader bool, err error) {
	key := fillKey{repository: pbs.repositoryName.Name(), dgst: dgst}
	if f := pbs.fills.lookup(key); f != nil {
		return f, false, nil
	}
	desc, err := pbs.remoteStore.Stat(ctx, dgst)
	if err != nil {
		return nil, false, err
	}
	f, leader = pbs.fills.start(ctx, key, desc)
	if leader {
		fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pbs.cacheWriteTimeout)
		go func() {
			defer cancel()
			pbs.fills.run(fillCtx, pbs, key, f)
		}()
	}
	return f, leader, nil
}

func (pbs *proxyBlobStore) serveFill(ctx context.Context, w http.ResponseWriter, r *http.Request, f *blobFill) {
	start, _, _ := singleRange(r, f.desc.Size)
	if !f.canServe(start) {
		pbs.serveRemoteRange(ctx, w, r, f.desc)
		return
	}
	serveContent(w, r, f.desc, &spoolReader{ctx: ctx, f: f})
}

func (pbs *proxyBlobStore) serveRemoteRange(ctx context.Context, w http.ResponseWriter, r *http.Request, desc v1.Descriptor) {
	_, end, _ := singleRange(r, desc.Size)
	reader := newRemoteReader(ctx, pbs.remoteStore, desc.Digest, desc.Size, end)
	defer reader.Close()
	serveContent(w, r, desc, reader)
}

func serveContent(w http.ResponseWriter, r *http.Request, desc v1.Descriptor, content io.ReadSeeker) {
	setResponseHeaders(w.Header(), desc.Size, desc.MediaType, desc.Digest)
	http.ServeContent(w, r, desc.Digest.String(), time.Time{}, content)

	// ServeContent rewrites Content-Length to the bytes it served. It is empty for a multipart response.
	pushed, _ := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
	proxyMetrics.BlobPush(uint64(pushed), false)
}
