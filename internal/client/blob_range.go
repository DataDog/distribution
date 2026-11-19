package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/client/transport"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// OpenRange returns bytes [start, end) of the blob from a single ranged GET.
// It fails unless the server answers with exactly that range.
func (bs *blobs) OpenRange(ctx context.Context, dgst digest.Digest, start, end int64) (io.ReadCloser, error) {
	if start < 0 || end <= start {
		return nil, fmt.Errorf("invalid blob range [%d, %d)", start, end)
	}
	ref, err := reference.WithDigest(bs.name, dgst)
	if err != nil {
		return nil, err
	}
	blobURL, err := bs.ub.BuildBlobURL(ref)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))

	resp, err := bs.client.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkRangeResponse(resp, start, end); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

func checkRangeResponse(resp *http.Response, start, end int64) error {
	if resp.StatusCode == http.StatusNotFound {
		return distribution.ErrBlobUnknown
	}
	if err := HandleHTTPResponseError(resp); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusPartialContent {
		return transport.ErrWrongCodeForByteRange
	}
	want := fmt.Sprintf("bytes %d-%d/", start, end-1)
	if got := resp.Header.Get("Content-Range"); !strings.HasPrefix(got, want) {
		return fmt.Errorf("unexpected Content-Range %q for requested range [%d, %d)", got, start, end)
	}
	return nil
}
