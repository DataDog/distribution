package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
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
	fills             *fillRegistry
}

var _ distribution.BlobStore = &proxyBlobStore{}

func setResponseHeaders(h http.Header, length int64, mediaType string, digest digest.Digest) {
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("Content-Type", mediaType)
	h.Set("Docker-Content-Digest", digest.String())
	h.Set("Etag", digest.String())
}

// serveRemote streams the blob from the remote store to the client, honoring
// any Range header via http.ServeContent. It does not populate the local cache.
func (pbs *proxyBlobStore) serveRemote(ctx context.Context, w http.ResponseWriter, r *http.Request, desc v1.Descriptor) {
	_, end, _ := singleRange(r, desc.Size)
	remoteReader := newRemoteReader(ctx, pbs.remoteStore, desc.Digest, desc.Size, end)
	defer remoteReader.Close()
	serveContent(w, r, desc, remoteReader)
}

func serveContent(w http.ResponseWriter, r *http.Request, desc v1.Descriptor, content io.ReadSeeker) {
	setResponseHeaders(w.Header(), desc.Size, desc.MediaType, desc.Digest)
	http.ServeContent(w, r, desc.Digest.String(), time.Time{}, content)

	// ServeContent rewrites Content-Length to the bytes actually served (the
	// range size for a 206, or the full size for a 200), so read it back for
	// the push metric. It is empty/zero for a multipart multi-range response.
	pushed, _ := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
	proxyMetrics.BlobPush(uint64(pushed), false)
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

	if r.Method == http.MethodHead {
		desc, err := pbs.remoteStore.Stat(ctx, dgst)
		if err != nil {
			return err
		}

		setResponseHeaders(w.Header(), desc.Size, desc.MediaType, desc.Digest)
		w.WriteHeader(http.StatusOK)
		return nil
	}

	f, leader, err := pbs.joinFill(ctx, dgst)
	if err != nil {
		return err
	}
	defer pbs.fills.release(f)

	pbs.serveFill(ctx, w, r, f)
	if !leader {
		return nil
	}
	// The request that started the fill returns the cache result.
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
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
		pbs.serveRemote(ctx, w, r, f.desc)
		return
	}
	serveContent(w, r, f.desc, &spoolReader{ctx: ctx, f: f})
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
