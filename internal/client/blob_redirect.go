package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// OpenOrRedirect sends a GET for the blob dgst, with rangeHeader as the Range
// header when it is not empty. It returns the response when its status is 200,
// 206, 416, or 307 to another host (such as a presigned storage URL). A
// cross-host 307 is returned with an absolute Location instead of being
// followed. Redirects to the registry host are followed, because they can need
// the registry credentials. The body is not verified (see Open). The caller
// must close it.
func (bs *blobs) OpenOrRedirect(ctx context.Context, dgst digest.Digest, rangeHeader string) (*http.Response, error) {
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
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	client := *bs.client
	client.CheckRedirect = stopAtCrossHostTemporaryRedirect
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return checkOpenOrRedirectResponse(resp)
}

// checkOpenOrRedirectResponse returns resp when OpenOrRedirect passes it to the
// caller, and otherwise closes its body and returns an error.
func checkOpenOrRedirectResponse(resp *http.Response) (*http.Response, error) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
		return resp, nil
	case http.StatusTemporaryRedirect:
		location, err := resp.Location()
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Header.Set("Location", location.String())
		return resp, nil
	}

	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, distribution.ErrBlobUnknown
	}
	if err := HandleHTTPResponseError(resp); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unexpected status fetching blob: %s", resp.Status)
}

// stopAtCrossHostTemporaryRedirect stops at a 307 to a host other than the
// host of the first request, and follows every other redirect.
func stopAtCrossHostTemporaryRedirect(req *http.Request, via []*http.Request) error {
	if req.Response != nil && req.Response.StatusCode == http.StatusTemporaryRedirect &&
		req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return checkHTTPRedirect(req, via)
}
