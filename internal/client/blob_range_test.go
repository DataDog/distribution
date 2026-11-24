package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/distribution/reference"
)

func TestBlobOpenRange(t *testing.T) {
	dgst, content := newRandomBlob(1024)
	tests := []struct {
		name         string
		contentRange string
		status       int
		wantErr      bool
	}{
		{"partial content", "bytes 100-199/1024", http.StatusPartialContent, false},
		{"range ignored", "", http.StatusOK, true},
		{"wrong range", "bytes 0-99/1024", http.StatusPartialContent, true},
		{"not found", "", http.StatusNotFound, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotRange string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRange = r.Header.Get("Range")
				if tt.contentRange != "" {
					w.Header().Set("Content-Range", tt.contentRange)
				}
				w.WriteHeader(tt.status)
				w.Write(content[100:200])
			}))
			defer server.Close()

			name, _ := reference.WithName("test.example.com/repo1")
			repo, err := NewRepository(name, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			blobs := repo.Blobs(t.Context()).(*blobs)

			body, err := blobs.OpenRange(t.Context(), dgst, 100, 200)
			if gotRange != "bytes=100-199" {
				t.Errorf("Range header: got %q, want %q", gotRange, "bytes=100-199")
			}
			if tt.wantErr {
				if err == nil {
					body.Close()
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content[100:200]) {
				t.Errorf("body: got %d bytes, want content[100:200]", len(got))
			}
		})
	}
}
