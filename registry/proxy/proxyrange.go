package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/distribution/distribution/v3"
)

type rangeOpener interface {
	OpenRange(ctx context.Context, dgst digest.Digest, start, end int64) (io.ReadCloser, error)
}

type remoteReader struct {
	ctx    context.Context
	remote distribution.BlobService
	dgst   digest.Digest
	size   int64
	limit  int64
	offset int64

	body    io.ReadCloser
	bodyEnd int64
}

func newRemoteReader(ctx context.Context, remote distribution.BlobService, dgst digest.Digest, size, limit int64) *remoteReader {
	return &remoteReader{ctx: ctx, remote: remote, dgst: dgst, size: size, limit: limit}
}

func (rr *remoteReader) Read(p []byte) (int, error) {
	if rr.offset >= rr.size {
		return 0, io.EOF
	}
	if rr.body == nil {
		if err := rr.open(); err != nil {
			return 0, err
		}
	}

	n, err := rr.body.Read(p)
	rr.offset += int64(n)
	proxyMetrics.BlobPull(uint64(n))
	if err != io.EOF {
		return n, err
	}
	if rr.offset < rr.bodyEnd {
		return n, io.ErrUnexpectedEOF
	}
	rr.closeBody()
	if rr.offset >= rr.size {
		return n, io.EOF
	}
	if n == 0 {
		return rr.Read(p)
	}
	return n, nil
}

func (rr *remoteReader) open() error {
	end := rr.size
	if rr.limit > rr.offset {
		end = min(rr.limit, rr.size)
	}

	var err error
	switch opener, ok := rr.remote.(rangeOpener); {
	case rr.offset == 0 && end == rr.size:
		rr.body, err = rr.remote.Open(rr.ctx, rr.dgst)
	case ok:
		rr.body, err = opener.OpenRange(rr.ctx, rr.dgst, rr.offset, end)
	default:
		rr.body, err = rr.openSeek(end)
	}
	if err != nil {
		return err
	}
	rr.bodyEnd = end
	return nil
}

func (rr *remoteReader) openSeek(end int64) (io.ReadCloser, error) {
	rsc, err := rr.remote.Open(rr.ctx, rr.dgst)
	if err != nil {
		return nil, err
	}
	if _, err := rsc.Seek(rr.offset, io.SeekStart); err != nil {
		rsc.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rsc, end-rr.offset), rsc}, nil
}

// Seek stays local: http.ServeContent seeks to the end to learn the size, which costs a full GET on an HTTP reader.
func (rr *remoteReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += rr.offset
	case io.SeekEnd:
		offset += rr.size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	if offset != rr.offset {
		rr.closeBody()
	}
	rr.offset = offset
	return offset, nil
}

func (rr *remoteReader) Close() error {
	rr.closeBody()
	return nil
}

func (rr *remoteReader) closeBody() {
	if rr.body != nil {
		rr.body.Close()
		rr.body = nil
	}
}

func singleRange(r *http.Request, size int64) (start, end int64, ok bool) {
	// ServeContent ignores Range when If-Range does not match, so a bound could truncate a full response.
	if r.Header.Get("If-Range") != "" {
		return 0, 0, false
	}
	spec, found := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Range")), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if last == "" {
		return start, size, true
	}
	lastByte, err := strconv.ParseInt(last, 10, 64)
	if err != nil || lastByte < start {
		return 0, 0, false
	}
	return start, min(lastByte+1, size), true
}
