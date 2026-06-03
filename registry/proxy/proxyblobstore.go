package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/distribution/v3/registry/proxy/scheduler"
	"github.com/distribution/reference"
)

type proxyBlobStore struct {
	localStore        distribution.BlobStore
	remoteStore       distribution.BlobService
	scheduler         *scheduler.TTLExpirationScheduler
	ttl               *time.Duration
	cacheWriteTimeout time.Duration
	repositoryName    reference.Named
	authChallenger    authChallenger
}

var _ distribution.BlobStore = &proxyBlobStore{}

var errRangeNotSatisfiable = errors.New("range not satisfiable")

// inflight tracks currently downloading blobs
var inflight = make(map[digest.Digest]struct{})

// mu protects inflight
var mu sync.Mutex

func setResponseHeaders(h http.Header, length int64, mediaType string, digest digest.Digest) {
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("Content-Type", mediaType)
	h.Set("Docker-Content-Digest", digest.String())
	h.Set("Etag", digest.String())
	h.Set("Accept-Ranges", "bytes")
}

func (pbs *proxyBlobStore) copyRemoteContent(ctx context.Context, dgst digest.Digest, writer io.Writer, desc v1.Descriptor) error {
	remoteReader, err := pbs.remoteStore.Open(ctx, dgst)
	if err != nil {
		return err
	}
	defer remoteReader.Close()

	_, err = io.CopyN(writer, remoteReader, desc.Size)
	return err
}

func (pbs *proxyBlobStore) copyContent(ctx context.Context, dgst digest.Digest, writer io.Writer, h http.Header) (v1.Descriptor, error) {
	desc, err := pbs.remoteStore.Stat(ctx, dgst)
	if err != nil {
		return v1.Descriptor{}, err
	}

	setResponseHeaders(h, desc.Size, desc.MediaType, dgst)

	if err := pbs.copyRemoteContent(ctx, dgst, writer, desc); err != nil {
		return v1.Descriptor{}, err
	}

	proxyMetrics.BlobPull(uint64(desc.Size))
	proxyMetrics.BlobPush(uint64(desc.Size), false)

	return desc, nil
}

type byteRange struct {
	start  int64
	length int64
}

func (br byteRange) end() int64 {
	return br.start + br.length - 1
}

func (br byteRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", br.start, br.end(), size)
}

func parseRangeHeader(rangeHeader string, size int64) ([]byteRange, bool, error) {
	rangeHeader = strings.TrimSpace(rangeHeader)
	if rangeHeader == "" {
		return nil, false, nil
	}

	unit, rangeSet, ok := strings.Cut(rangeHeader, "=")
	if !ok || strings.TrimSpace(unit) != "bytes" {
		return nil, false, nil
	}
	if size <= 0 {
		return nil, true, errRangeNotSatisfiable
	}

	var ranges []byteRange
	for _, rangeSpec := range strings.Split(rangeSet, ",") {
		rangeSpec = strings.TrimSpace(rangeSpec)
		startSpec, endSpec, ok := strings.Cut(rangeSpec, "-")
		if !ok {
			return nil, true, errRangeNotSatisfiable
		}

		if startSpec == "" {
			suffixLength, err := strconv.ParseInt(endSpec, 10, 64)
			if err != nil || suffixLength <= 0 {
				return nil, true, errRangeNotSatisfiable
			}
			if suffixLength > size {
				suffixLength = size
			}
			ranges = append(ranges, byteRange{start: size - suffixLength, length: suffixLength})
			continue
		}

		start, err := strconv.ParseInt(startSpec, 10, 64)
		if err != nil || start < 0 {
			return nil, true, errRangeNotSatisfiable
		}
		if start >= size {
			continue
		}

		end := size - 1
		if endSpec != "" {
			end, err = strconv.ParseInt(endSpec, 10, 64)
			if err != nil || end < start {
				return nil, true, errRangeNotSatisfiable
			}
			if end >= size {
				end = size - 1
			}
		}

		ranges = append(ranges, byteRange{start: start, length: end - start + 1})
	}

	if len(ranges) == 0 {
		return nil, true, errRangeNotSatisfiable
	}

	return ranges, true, nil
}

func rangeContentLength(ranges []byteRange) int64 {
	var length int64
	for _, br := range ranges {
		length += br.length
	}
	return length
}

func serveRangeNotSatisfiable(w http.ResponseWriter, size int64) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	http.Error(w, http.StatusText(http.StatusRequestedRangeNotSatisfiable), http.StatusRequestedRangeNotSatisfiable)
}

func (pbs *proxyBlobStore) serveRemoteRanges(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest, desc v1.Descriptor, ranges []byteRange, recordPull bool) error {
	setResponseHeaders(w.Header(), desc.Size, desc.MediaType, dgst)

	contentLength := rangeContentLength(ranges)
	if len(ranges) == 1 {
		br := ranges[0]
		w.Header().Set("Content-Length", strconv.FormatInt(br.length, 10))
		w.Header().Set("Content-Range", br.contentRange(desc.Size))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusPartialContent)
			proxyMetrics.BlobPush(0, false)
			return nil
		}

		remoteReader, err := pbs.remoteStore.Open(ctx, dgst)
		if err != nil {
			return err
		}
		defer remoteReader.Close()

		if _, err := remoteReader.Seek(br.start, io.SeekStart); err != nil {
			return err
		}

		w.WriteHeader(http.StatusPartialContent)
		if _, err := io.CopyN(w, remoteReader, br.length); err != nil {
			return err
		}

		if recordPull {
			proxyMetrics.BlobPull(uint64(contentLength))
		} else {
			proxyMetrics.BlobPullBytes(uint64(contentLength))
		}
		proxyMetrics.BlobPush(uint64(contentLength), false)
		return nil
	}

	mw := multipart.NewWriter(w)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusPartialContent)
		proxyMetrics.BlobPush(0, false)
		return nil
	}

	remoteReader, err := pbs.remoteStore.Open(ctx, dgst)
	if err != nil {
		return err
	}
	defer remoteReader.Close()

	w.WriteHeader(http.StatusPartialContent)
	for _, br := range ranges {
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Range", br.contentRange(desc.Size))
		partHeader.Set("Content-Type", desc.MediaType)

		partWriter, err := mw.CreatePart(partHeader)
		if err != nil {
			return err
		}
		if _, err := remoteReader.Seek(br.start, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(partWriter, remoteReader, br.length); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}

	if recordPull {
		proxyMetrics.BlobPull(uint64(contentLength))
	} else {
		proxyMetrics.BlobPullBytes(uint64(contentLength))
	}
	proxyMetrics.BlobPush(uint64(contentLength), false)
	return nil
}

func (pbs *proxyBlobStore) cacheRemoteBlob(ctx context.Context, writerCtx context.Context, bw distribution.BlobWriter, dgst digest.Digest, desc v1.Descriptor) error {
	committed := false
	defer func() {
		if !committed {
			if err := bw.Cancel(writerCtx); err != nil {
				dcontext.GetLogger(ctx).WithError(err).Errorf("Error canceling blob writer")
			}
		}
	}()

	if err := pbs.copyRemoteContent(writerCtx, dgst, bw, desc); err != nil {
		return err
	}
	proxyMetrics.BlobPull(uint64(desc.Size))

	if _, err := bw.Commit(writerCtx, desc); err != nil {
		return err
	}
	committed = true

	return pbs.scheduleBlob(ctx, dgst)
}

func (pbs *proxyBlobStore) scheduleBlob(ctx context.Context, dgst digest.Digest) error {
	blobRef, err := reference.WithDigest(pbs.repositoryName, dgst)
	if err != nil {
		dcontext.GetLogger(ctx).Errorf("Error creating reference: %s", err)
		return err
	}

	if pbs.scheduler != nil && pbs.ttl != nil {
		if err := pbs.scheduler.AddBlob(blobRef, *pbs.ttl); err != nil {
			dcontext.GetLogger(ctx).Errorf("Error adding blob: %s", err)
			return err
		}
	}

	return nil
}

func (pbs *proxyBlobStore) serveLocal(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) (bool, error) {
	localDesc, err := pbs.localStore.Stat(ctx, dgst)
	if err != nil {
		// Stat can report a zero sized file here if it's checked between creation
		// and population.  Return nil error, and continue
		return false, nil
	}

	proxyMetrics.BlobPush(uint64(localDesc.Size), true)
	return true, pbs.localStore.ServeBlob(ctx, w, r, dgst)
}

func (pbs *proxyBlobStore) ServeBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) error {
	served, err := pbs.serveLocal(ctx, w, r, dgst)
	if err != nil {
		dcontext.GetLogger(ctx).Errorf("Error serving blob from local storage: %s", err.Error())
		return err
	}

	if served {
		return nil
	}

	if err := pbs.authChallenger.tryEstablishChallenges(ctx); err != nil {
		return err
	}

	mu.Lock()
	_, ok := inflight[dgst]
	if ok {
		// If the blob has been serving in other requests.
		// Will return the blob from the remote store directly.
		// TODO Maybe we could reuse the these blobs are serving remotely and caching locally.
		mu.Unlock()
		if strings.TrimSpace(r.Header.Get("Range")) != "" {
			desc, err := pbs.remoteStore.Stat(ctx, dgst)
			if err != nil {
				return err
			}
			ranges, requested, err := parseRangeHeader(r.Header.Get("Range"), desc.Size)
			if requested {
				if err != nil {
					setResponseHeaders(w.Header(), desc.Size, desc.MediaType, dgst)
					serveRangeNotSatisfiable(w, desc.Size)
					proxyMetrics.BlobPush(0, false)
					return nil
				}
				return pbs.serveRemoteRanges(ctx, w, r, dgst, desc, ranges, true)
			}
		}
		_, err := pbs.copyContent(ctx, dgst, w, w.Header())
		return err
	}
	inflight[dgst] = struct{}{}
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(inflight, dgst)
		mu.Unlock()
	}()

	if strings.TrimSpace(r.Header.Get("Range")) != "" {
		desc, err := pbs.remoteStore.Stat(ctx, dgst)
		if err != nil {
			return err
		}

		ranges, requested, err := parseRangeHeader(r.Header.Get("Range"), desc.Size)
		if requested {
			if err != nil {
				setResponseHeaders(w.Header(), desc.Size, desc.MediaType, dgst)
				serveRangeNotSatisfiable(w, desc.Size)
				proxyMetrics.BlobPush(0, false)
				return nil
			}

			writerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pbs.cacheWriteTimeout)
			defer cancel()

			bw, err := pbs.localStore.Create(writerCtx)
			if err != nil {
				return err
			}

			cacheErrCh := make(chan error, 1)
			go func() {
				cacheErrCh <- pbs.cacheRemoteBlob(ctx, writerCtx, bw, dgst, desc)
			}()

			serveErr := pbs.serveRemoteRanges(ctx, w, r, dgst, desc, ranges, false)
			cacheErr := <-cacheErrCh
			if serveErr != nil {
				return serveErr
			}
			return cacheErr
		}
	}

	// Create a detached context for the blob writer that won't be canceled
	// when the HTTP request context is canceled. This allows the cache write
	// to complete even if the client disconnects.
	// Use the configured timeout to prevent hanging operations.
	writerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pbs.cacheWriteTimeout)
	defer cancel()

	bw, err := pbs.localStore.Create(writerCtx)
	if err != nil {
		return err
	}

	committed := false
	// Ensure the writer is canceled if we return early with an error
	defer func() {
		if !committed {
			if err := bw.Cancel(writerCtx); err != nil {
				dcontext.GetLogger(ctx).WithError(err).Errorf("Error canceling blob writer")
			}
		}
	}()

	// Serving client and storing locally over same fetching request.
	// This can prevent a redundant blob fetching.
	multiWriter := io.MultiWriter(w, bw)
	desc, err := pbs.copyContent(ctx, dgst, multiWriter, w.Header())
	if err != nil {
		return err
	}

	_, err = bw.Commit(writerCtx, desc)
	if err != nil {
		return err
	}

	committed = true

	return pbs.scheduleBlob(ctx, dgst)
}

func (pbs *proxyBlobStore) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	desc, err := pbs.localStore.Stat(ctx, dgst)
	if err == nil {
		return desc, err
	}

	if err != distribution.ErrBlobUnknown {
		return v1.Descriptor{}, err
	}

	if err := pbs.authChallenger.tryEstablishChallenges(ctx); err != nil {
		return v1.Descriptor{}, err
	}

	return pbs.remoteStore.Stat(ctx, dgst)
}

func (pbs *proxyBlobStore) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	blob, err := pbs.localStore.Get(ctx, dgst)
	if err == nil {
		return blob, nil
	}

	if err := pbs.authChallenger.tryEstablishChallenges(ctx); err != nil {
		return []byte{}, err
	}

	blob, err = pbs.remoteStore.Get(ctx, dgst)
	if err != nil {
		return []byte{}, err
	}

	_, err = pbs.localStore.Put(ctx, "", blob)
	if err != nil {
		return []byte{}, err
	}
	return blob, nil
}

// Unsupported functions
func (pbs *proxyBlobStore) Put(ctx context.Context, mediaType string, p []byte) (v1.Descriptor, error) {
	return v1.Descriptor{}, distribution.ErrUnsupported
}

func (pbs *proxyBlobStore) Create(ctx context.Context, options ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	return nil, distribution.ErrUnsupported
}

func (pbs *proxyBlobStore) Resume(ctx context.Context, id string) (distribution.BlobWriter, error) {
	return nil, distribution.ErrUnsupported
}

func (pbs *proxyBlobStore) Mount(ctx context.Context, sourceRepo reference.Named, dgst digest.Digest) (v1.Descriptor, error) {
	return v1.Descriptor{}, distribution.ErrUnsupported
}

func (pbs *proxyBlobStore) Open(ctx context.Context, dgst digest.Digest) (io.ReadSeekCloser, error) {
	return nil, distribution.ErrUnsupported
}

func (pbs *proxyBlobStore) Delete(ctx context.Context, dgst digest.Digest) error {
	return distribution.ErrUnsupported
}
