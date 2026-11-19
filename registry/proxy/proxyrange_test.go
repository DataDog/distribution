package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/client"
	"github.com/distribution/reference"
)

type fakeUpstream struct {
	t    *testing.T
	blob []byte
	desc v1.Descriptor

	mu     sync.Mutex
	ranges []string
}

func newFakeUpstream(t *testing.T, size int) *fakeUpstream {
	t.Helper()
	blob := makeBlob(size)
	return &fakeUpstream{
		t:    t,
		blob: blob,
		desc: v1.Descriptor{
			MediaType: "application/octet-stream",
			Size:      int64(size),
			Digest:    digest.FromBytes(blob),
		},
	}
}

func (fu *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodHead:
		w.Header().Set("Content-Type", fu.desc.MediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(fu.desc.Size, 10))
		return
	case http.MethodGet:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	fu.mu.Lock()
	fu.ranges = append(fu.ranges, r.Header.Get("Range"))
	fu.mu.Unlock()

	w.Header().Set("Content-Type", fu.desc.MediaType)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(fu.blob))
}

func (fu *fakeUpstream) gets() []string {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return slices.Clone(fu.ranges)
}

func (fu *fakeUpstream) remoteStore(ctx context.Context) distribution.BlobStore {
	fu.t.Helper()
	server := httptest.NewServer(fu)
	fu.t.Cleanup(server.Close)

	name, err := reference.WithName("foo/bar")
	if err != nil {
		fu.t.Fatal(err)
	}
	repo, err := client.NewRepository(name, server.URL, http.DefaultTransport)
	if err != nil {
		fu.t.Fatal(err)
	}
	return repo.Blobs(ctx)
}

func TestServeRemoteSendsOneBoundedRequest(t *testing.T) {
	const size = 1 << 20
	tests := []struct {
		name       string
		rangeValue string
		wantStatus int
		wantGets   []string
		wantStart  int
		wantEnd    int
	}{
		{"full", "", http.StatusOK, []string{""}, 0, size},
		{"head of blob", "bytes=0-1023", http.StatusPartialContent, []string{"bytes=0-1023"}, 0, 1024},
		{"middle", "bytes=4096-8191", http.StatusPartialContent, []string{"bytes=4096-8191"}, 4096, 8192},
		{"open-ended", "bytes=1000000-", http.StatusPartialContent, []string{"bytes=1000000-1048575"}, 1000000, size},
		{"suffix", "bytes=-1024", http.StatusPartialContent, []string{"bytes=1047552-1048575"}, size - 1024, size},
		{"unsatisfiable", "bytes=2000000-", http.StatusRequestedRangeNotSatisfiable, nil, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newFakeUpstream(t, size)
			pbs := &proxyBlobStore{remoteStore: upstream.remoteStore(t.Context())}

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.rangeValue != "" {
				r.Header.Set("Range", tt.rangeValue)
			}
			if err := pbs.serveRemote(t.Context(), w, r, upstream.desc.Digest, upstream.desc); err != nil {
				t.Fatal(err)
			}

			if w.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", w.Code, tt.wantStatus)
			}
			if got := upstream.gets(); !slices.Equal(got, tt.wantGets) {
				t.Errorf("upstream GET ranges: got %q, want %q", got, tt.wantGets)
			}
			if tt.wantStatus == http.StatusRequestedRangeNotSatisfiable {
				return
			}
			if !bytes.Equal(w.Body.Bytes(), upstream.blob[tt.wantStart:tt.wantEnd]) {
				t.Errorf("body: got %d bytes, want blob[%d:%d]", w.Body.Len(), tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestServeRemoteMultiRange(t *testing.T) {
	upstream := newFakeUpstream(t, 4096)
	pbs := &proxyBlobStore{remoteStore: upstream.remoteStore(t.Context())}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Range", "bytes=0-9,100-109")
	if err := pbs.serveRemote(t.Context(), w, r, upstream.desc.Digest, upstream.desc); err != nil {
		t.Fatal(err)
	}

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusPartialContent)
	}
	body := w.Body.Bytes()
	if !bytes.Contains(body, upstream.blob[0:10]) || !bytes.Contains(body, upstream.blob[100:110]) {
		t.Errorf("multipart body is missing a requested part")
	}
}

func TestSingleRange(t *testing.T) {
	const size = 100
	tests := []struct {
		rangeValue string
		ifRange    string
		wantStart  int64
		wantEnd    int64
		wantOK     bool
	}{
		{"", "", 0, 0, false},
		{"bytes=0-9", "", 0, 10, true},
		{"bytes=90-", "", 90, 100, true},
		{"bytes=90-1000", "", 90, 100, true},
		{"bytes=-10", "", 90, 100, true},
		{"bytes=-1000", "", 0, 100, true},
		{"bytes=0-9", "sha256:abc", 0, 0, false},
		{"bytes=0-9,20-29", "", 0, 0, false},
		{"bytes=100-", "", 0, 0, false},
		{"bytes=9-0", "", 0, 0, false},
		{"bytes=-0", "", 0, 0, false},
		{"items=0-9", "", 0, 0, false},
		{"bytes=a-b", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.rangeValue+"/"+tt.ifRange, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Range", tt.rangeValue)
			if tt.ifRange != "" {
				r.Header.Set("If-Range", tt.ifRange)
			}
			start, end, ok := singleRange(r, size)
			if ok != tt.wantOK || start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("got (%d, %d, %v), want (%d, %d, %v)", start, end, ok, tt.wantStart, tt.wantEnd, tt.wantOK)
			}
		})
	}
}
