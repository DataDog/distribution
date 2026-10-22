package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

func TestBlobOpenOrRedirect(t *testing.T) {
	dgst, blob := newRandomBlob(1024)
	serveBlob := func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}

	storage := httptest.NewServer(http.HandlerFunc(serveBlob))
	defer storage.Close()
	storageURL := storage.URL + "/presigned"

	tests := []struct {
		name         string
		upstream     func(w http.ResponseWriter, r *http.Request)
		rangeHeader  string
		wantStatus   int
		wantLocation string
		wantBody     []byte
		wantErr      error
	}{
		{
			name: "307 to another host is returned",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, storageURL, http.StatusTemporaryRedirect)
			},
			wantStatus:   http.StatusTemporaryRedirect,
			wantLocation: storageURL,
		},
		{
			name: "307 to the same host is followed with the range",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/moved" {
					http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
					return
				}
				serveBlob(w, r)
			},
			rangeHeader: "bytes=10-19",
			wantStatus:  http.StatusPartialContent,
			wantBody:    blob[10:20],
		},
		{
			name: "302 to another host is followed",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, storageURL, http.StatusFound)
			},
			wantStatus: http.StatusOK,
			wantBody:   blob,
		},
		{
			name:       "200 returns the body",
			upstream:   serveBlob,
			wantStatus: http.StatusOK,
			wantBody:   blob,
		},
		{
			name:        "206 returns the range",
			upstream:    serveBlob,
			rangeHeader: "bytes=10-19",
			wantStatus:  http.StatusPartialContent,
			wantBody:    blob[10:20],
		},
		{
			name:        "416 is returned",
			upstream:    serveBlob,
			rangeHeader: "bytes=5000-",
			wantStatus:  http.StatusRequestedRangeNotSatisfiable,
		},
		{
			name:     "404 is an unknown blob",
			upstream: http.NotFound,
			wantErr:  distribution.ErrBlobUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(tt.upstream))
			defer upstream.Close()

			ctx := dcontext.Background()
			repo, _ := reference.WithName("test.example.com/repo1")
			r, err := NewRepository(repo, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			opener := r.Blobs(ctx).(interface {
				OpenOrRedirect(context.Context, digest.Digest, string) (*http.Response, error)
			})

			resp, err := opener.OpenOrRedirect(ctx, dgst, tt.rangeHeader)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("unexpected error: got %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("unexpected status: got %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get("Location"); got != tt.wantLocation {
				t.Errorf("unexpected location: got %q, want %q", got, tt.wantLocation)
			}
			if tt.wantBody == nil {
				return
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.wantBody) {
				t.Errorf("unexpected body: got %d bytes, want %d", len(got), len(tt.wantBody))
			}
		})
	}
}
