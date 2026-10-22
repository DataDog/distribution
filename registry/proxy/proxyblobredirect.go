package proxy

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// redirectOpener is a remote blob store that can report an upstream redirect
// instead of following it. It is implemented by the internal/client blob store.
type redirectOpener interface {
	// OpenOrRedirect sends a GET with rangeHeader when it is not empty. It
	// returns a 200, 206 or 416 response, or a 307 to another host with an
	// absolute Location. The caller must close the body.
	OpenOrRedirect(ctx context.Context, dgst digest.Digest, rangeHeader string) (*http.Response, error)
}

// redirectOpener returns remoteBlobs as a redirectOpener when the proxy
// forwards upstream redirects, and nil otherwise.
func (pr *proxyingRegistry) redirectOpener(remoteBlobs distribution.BlobService) redirectOpener {
	if !pr.forwardRedirects {
		return nil
	}
	return remoteBlobs.(redirectOpener)
}

// forwardsRedirects reports whether an upstream redirect for r may be sent to
// the client. Presigned storage URLs are signed for one method, and the remote
// signs the redirect for our GET, so a HEAD client cannot follow it.
func (pbs *proxyBlobStore) forwardsRedirects(r *http.Request) bool {
	return pbs.redirects != nil && r.Method == http.MethodGet
}

// serveRedirectable serves a cache miss with one upstream GET that carries the
// client Range and does not follow a cross-host 307. The response (200, 206,
// 416 or 307) goes to the client as-is. When no other request fills the cache,
// a second upstream GET fills it in the background.
func (pbs *proxyBlobStore) serveRedirectable(
	ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest,
) error {
	desc, err := pbs.remoteStore.Stat(ctx, dgst)
	if err != nil {
		return err
	}
	if claimFill(dgst) {
		go pbs.fillInBackground(ctx, desc)
	}

	resp, err := pbs.redirects.OpenOrRedirect(ctx, dgst, r.Header.Get("Range"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return relayResponse(w, r, desc, resp)
}

// relayResponse sends the upstream response resp to the client.
func relayResponse(w http.ResponseWriter, r *http.Request, desc v1.Descriptor, resp *http.Response) error {
	if resp.StatusCode == http.StatusTemporaryRedirect {
		http.Redirect(w, r, resp.Header.Get("Location"), http.StatusTemporaryRedirect)
		proxyMetrics.BlobPull(0)
		proxyMetrics.BlobPush(0, false)
		return nil
	}

	h := w.Header()
	for _, key := range []string{"Content-Length", "Content-Range", "Content-Type"} {
		if v := resp.Header.Get(key); v != "" {
			h.Set(key, v)
		}
	}
	h.Set("Docker-Content-Digest", desc.Digest.String())
	h.Set("Etag", desc.Digest.String())
	w.WriteHeader(resp.StatusCode)

	n, err := io.Copy(w, resp.Body)
	proxyMetrics.BlobPull(uint64(n))
	proxyMetrics.BlobPush(uint64(n), false)
	return err
}

// claimFill marks dgst as inflight and reports whether the caller owns the
// cache fill. It shares the inflight map with ServeBlob.
func claimFill(dgst digest.Digest) bool {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := inflight[dgst]; ok {
		return false
	}
	inflight[dgst] = struct{}{}
	return true
}

func releaseFill(dgst digest.Digest) {
	mu.Lock()
	delete(inflight, dgst)
	mu.Unlock()
}

// fillInBackground copies the full blob from the remote store to the local
// store and then releases the inflight claim. It runs on a context detached
// from the client request, so a client disconnect does not abort it.
func (pbs *proxyBlobStore) fillInBackground(ctx context.Context, desc v1.Descriptor) {
	defer releaseFill(desc.Digest)
	fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pbs.cacheWriteTimeout)
	defer cancel()

	if err := pbs.fillCache(fillCtx, desc); err != nil {
		dcontext.GetLoggerWithField(fillCtx, "digest", desc.Digest).WithError(err).Error("Error filling blob cache")
	}
}

func (pbs *proxyBlobStore) fillCache(ctx context.Context, desc v1.Descriptor) error {
	remoteReader, err := pbs.remoteStore.Open(ctx, desc.Digest)
	if err != nil {
		return err
	}
	defer remoteReader.Close()

	bw, err := pbs.localStore.Create(ctx)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(bw, remoteReader, desc.Size); err != nil {
		if cancelErr := bw.Cancel(ctx); cancelErr != nil {
			dcontext.GetLogger(ctx).WithError(cancelErr).Error("Error canceling blob writer")
		}
		return err
	}
	proxyMetrics.blobFillPull(uint64(desc.Size))

	if _, err := bw.Commit(ctx, desc); err != nil {
		return err
	}
	return pbs.scheduleExpiry(desc.Digest)
}

func (pbs *proxyBlobStore) scheduleExpiry(dgst digest.Digest) error {
	if pbs.scheduler == nil || pbs.ttl == nil {
		return nil
	}
	blobRef, err := reference.WithDigest(pbs.repositoryName, dgst)
	if err != nil {
		return err
	}
	return pbs.scheduler.AddBlob(blobRef, *pbs.ttl)
}

// blobFillPull counts bytes pulled by a background cache fill. Unlike
// BlobPull, it does not count a miss: the client request that started the
// fill already counted it.
func (pmc *proxyMetricsCollector) blobFillPull(bytesPulled uint64) {
	atomic.AddUint64(&pmc.blobMetrics.BytesPulled, bytesPulled)
	pulledBytes.WithValues("blob").Inc(float64(bytesPulled))
}
