package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// fakeUpstream is a remote registry that serves one blob, or answers blob GETs
// with a 307 to a storage server when redirect is set. It counts GETs.
type fakeUpstream struct {
	blob        []byte
	redirect    bool
	ignoreRange bool

	registry    *httptest.Server
	storage     *httptest.Server
	registryGET atomic.Int32
	storageGET  atomic.Int32
}

func newFakeUpstream(t *testing.T, blob []byte, redirect, ignoreRange bool) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{blob: blob, redirect: redirect, ignoreRange: ignoreRange}
	u.storage = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.storageGET.Add(1)
		u.serveBlob(w, r)
	}))
	t.Cleanup(u.storage.Close)
	u.registry = httptest.NewServer(http.HandlerFunc(u.serveRegistry))
	t.Cleanup(u.registry.Close)
	return u
}

func (u *fakeUpstream) storageURL() string {
	return u.storage.URL + "/presigned"
}

func (u *fakeUpstream) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/" {
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(u.blob)))
		w.Header().Set("Content-Type", "application/octet-stream")
		return
	}
	u.registryGET.Add(1)
	if u.redirect {
		http.Redirect(w, r, u.storageURL(), http.StatusTemporaryRedirect)
		return
	}
	u.serveBlob(w, r)
}

func (u *fakeUpstream) serveBlob(w http.ResponseWriter, r *http.Request) {
	if u.ignoreRange {
		r.Header.Del("Range")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(u.blob))
}

func newRedirectTestStore(t *testing.T, remoteURL string, forward bool) (*proxyBlobStore, distribution.BlobStore) {
	t.Helper()
	ctx := context.Background()
	driver := inmemory.New()
	local, err := storage.NewRegistry(ctx, driver)
	if err != nil {
		t.Fatal(err)
	}
	name, err := reference.WithName("foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	noTTL := time.Duration(0)
	pr, err := NewRegistryPullThroughCache(ctx, local, driver, configuration.Proxy{
		RemoteURL:        remoteURL,
		TTL:              &noTTL,
		ForwardRedirects: forward,
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := pr.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	localRepo, err := local.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return repo.Blobs(ctx).(*proxyBlobStore), localRepo.Blobs(ctx)
}

func waitForFillRelease(t *testing.T, dgst digest.Digest) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, ok := inflight[dgst]
		mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cache fill did not end")
}

func TestProxyStoreServeForwardRedirects(t *testing.T) {
	blob := makeBlob(100)
	dgst := digest.FromBytes(blob)

	tests := []struct {
		name             string
		forward          bool
		method           string
		rangeHeader      string
		redirect         bool
		ignoreRange      bool
		inflight         bool
		wantStatus       int
		wantBody         []byte
		wantContentRange string
		wantRedirect     bool
		wantRegistryGETs int32
		wantStorageGETs  int32
	}{
		{
			name:             "cold GET with 307 forwards it and fills from storage",
			forward:          true,
			redirect:         true,
			wantStatus:       http.StatusTemporaryRedirect,
			wantRedirect:     true,
			wantRegistryGETs: 2,
			wantStorageGETs:  1,
		},
		{
			name:             "cold ranged GET with 307 forwards it",
			forward:          true,
			rangeHeader:      "bytes=10-19",
			redirect:         true,
			wantStatus:       http.StatusTemporaryRedirect,
			wantRedirect:     true,
			wantRegistryGETs: 2,
			wantStorageGETs:  1,
		},
		{
			name:             "cold GET with 200 streams it and fills with a second GET",
			forward:          true,
			wantStatus:       http.StatusOK,
			wantBody:         blob,
			wantRegistryGETs: 2,
		},
		{
			name:             "cold ranged GET with 206 relays it",
			forward:          true,
			rangeHeader:      "bytes=10-19",
			wantStatus:       http.StatusPartialContent,
			wantBody:         blob[10:20],
			wantContentRange: "bytes 10-19/100",
			wantRegistryGETs: 2,
		},
		{
			name:             "inflight GET with 307 forwards it with one GET",
			forward:          true,
			redirect:         true,
			inflight:         true,
			wantStatus:       http.StatusTemporaryRedirect,
			wantRedirect:     true,
			wantRegistryGETs: 1,
		},
		{
			name:             "inflight ranged GET with 307 forwards it with one GET",
			forward:          true,
			rangeHeader:      "bytes=10-19",
			redirect:         true,
			inflight:         true,
			wantStatus:       http.StatusTemporaryRedirect,
			wantRedirect:     true,
			wantRegistryGETs: 1,
		},
		{
			name:             "inflight GET with 200 streams it with one GET",
			forward:          true,
			inflight:         true,
			wantStatus:       http.StatusOK,
			wantBody:         blob,
			wantRegistryGETs: 1,
		},
		{
			name:             "inflight ranged GET with 206 relays it with one GET",
			forward:          true,
			rangeHeader:      "bytes=10-19",
			inflight:         true,
			wantStatus:       http.StatusPartialContent,
			wantBody:         blob[10:20],
			wantContentRange: "bytes 10-19/100",
			wantRegistryGETs: 1,
		},
		{
			name:             "inflight unsatisfiable range relays the 416",
			forward:          true,
			rangeHeader:      "bytes=500-",
			inflight:         true,
			wantStatus:       http.StatusRequestedRangeNotSatisfiable,
			wantContentRange: "bytes */100",
			wantRegistryGETs: 1,
		},
		{
			name:             "inflight range ignored upstream relays the 200",
			forward:          true,
			rangeHeader:      "bytes=10-19",
			ignoreRange:      true,
			inflight:         true,
			wantStatus:       http.StatusOK,
			wantBody:         blob,
			wantRegistryGETs: 1,
		},
		{
			name:             "cold HEAD with 307 is not forwarded",
			forward:          true,
			method:           http.MethodHead,
			redirect:         true,
			wantStatus:       http.StatusOK,
			wantRegistryGETs: 1,
			wantStorageGETs:  1,
		},
		{
			name:             "disabled: cold GET with 307 is followed",
			redirect:         true,
			wantStatus:       http.StatusOK,
			wantBody:         blob,
			wantRegistryGETs: 1,
			wantStorageGETs:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newFakeUpstream(t, blob, tt.redirect, tt.ignoreRange)
			pbs, localBlobs := newRedirectTestStore(t, upstream.registry.URL, tt.forward)
			if tt.inflight {
				if !claimFill(dgst) {
					t.Fatal("blob already inflight")
				}
				t.Cleanup(func() { releaseFill(dgst) })
			}

			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			r := httptest.NewRequest(method, "/", nil)
			if tt.rangeHeader != "" {
				r.Header.Set("Range", tt.rangeHeader)
			}

			proxyMetrics = &proxyMetricsCollector{}
			w := httptest.NewRecorder()
			if err := pbs.ServeBlob(context.Background(), w, r, dgst); err != nil {
				t.Fatal(err)
			}
			if !tt.inflight {
				waitForFillRelease(t, dgst)
			}

			if w.Code != tt.wantStatus {
				t.Errorf("unexpected status: got %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.wantBody != nil && !bytes.Equal(w.Body.Bytes(), tt.wantBody) {
				t.Errorf("unexpected body: got %d bytes, want %d", w.Body.Len(), len(tt.wantBody))
			}
			if got := w.Header().Get("Content-Range"); got != tt.wantContentRange {
				t.Errorf("unexpected Content-Range: got %q, want %q", got, tt.wantContentRange)
			}
			wantLocation := ""
			if tt.wantRedirect {
				wantLocation = upstream.storageURL()
			}
			if got := w.Header().Get("Location"); got != wantLocation {
				t.Errorf("unexpected Location: got %q, want %q", got, wantLocation)
			}
			if got := upstream.registryGET.Load(); got != tt.wantRegistryGETs {
				t.Errorf("unexpected registry GETs: got %d, want %d", got, tt.wantRegistryGETs)
			}
			if got := upstream.storageGET.Load(); got != tt.wantStorageGETs {
				t.Errorf("unexpected storage GETs: got %d, want %d", got, tt.wantStorageGETs)
			}
			_, err := localBlobs.Stat(context.Background(), dgst)
			if tt.inflight && !errors.Is(err, distribution.ErrBlobUnknown) {
				t.Errorf("unexpected local stat for an inflight blob: got %v, want %v", err, distribution.ErrBlobUnknown)
			}
			if !tt.inflight && err != nil {
				t.Errorf("blob not cached: %v", err)
			}
			if got := proxyMetrics.blobMetrics; got.Requests != 1 || got.Misses != 1 {
				t.Errorf("unexpected metrics: got %+v, want 1 request and 1 miss", got)
			}
		})
	}
}

func TestProxyStoreServeForwardRedirectsCachedHit(t *testing.T) {
	blob := makeBlob(100)
	dgst := digest.FromBytes(blob)
	upstream := newFakeUpstream(t, blob, true, false)
	pbs, _ := newRedirectTestStore(t, upstream.registry.URL, true)

	for i, wantStatus := range []int{http.StatusTemporaryRedirect, http.StatusOK} {
		w := httptest.NewRecorder()
		if err := pbs.ServeBlob(context.Background(), w, httptest.NewRequest(http.MethodGet, "/", nil), dgst); err != nil {
			t.Fatal(err)
		}
		waitForFillRelease(t, dgst)
		if w.Code != wantStatus {
			t.Errorf("request %d: unexpected status: got %d, want %d", i, w.Code, wantStatus)
		}
	}
	if got := upstream.registryGET.Load(); got != 2 {
		t.Errorf("unexpected registry GETs: got %d, want 2", got)
	}
}
