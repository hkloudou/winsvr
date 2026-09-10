package update

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// HTTPValidator detects changes from standard HEAD response headers, preferring
// a strong ETag, then Last-Modified, then Content-Length. It requires no server
// support beyond serving a static file correctly.
type HTTPValidator struct {
	URL    string
	Client *http.Client
}

func (h *HTTPValidator) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (h *HTTPValidator) Check(ctx context.Context, cur State) (bool, State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, h.URL, nil)
	if err != nil {
		return false, cur, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return false, cur, err
	}
	defer resp.Body.Close()

	next := State{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		next.Size, _ = strconv.ParseInt(cl, 10, 64)
	}

	switch {
	case next.ETag != "" || cur.ETag != "":
		return next.ETag != cur.ETag, next, nil
	case next.LastModified != "" || cur.LastModified != "":
		return next.LastModified != cur.LastModified, next, nil
	default:
		return next.Size != cur.Size, next, nil
	}
}

// Verify is a no-op: header-based validation carries no content checksum.
func (h *HTTPValidator) Verify(path string, next State) error { return nil }
