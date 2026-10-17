package traedis

import (
	"bytes"
	"net/http"
	"strings"
	"time"
)

// Range requests (RFC 9110 §14). The cache only stores whole responses
// (§3.3): it answers a request for some of their bytes with those, as the
// backend would. Without a stored response, the request reaches the backend as
// it is, and a 206 is not stored.

// maxRanges bounds the ranges a request may ask for: with more, it gets the
// whole response, as §14.2 allows. Each range is a part of the body with its
// own header fields: many small ones (bytes=0-0,2-2,…) would make the answer
// many times larger than the request, and than the body, from a response that
// the backend made once. http.ServeContent has a bound of its own since Go
// 1.26.9, which depends on how Traefik is built: this one does not.
const maxRanges = 16

// wantsRange reports whether a GET with header req asks for byte ranges of
// the stored 200 response with header resp. Another range unit is ignored
// (§14.2), and so are ranges that the backend said it does not serve, and more
// than maxRanges of them: they are counted by their commas, as they are not
// parsed here.
func wantsRange(req, resp http.Header) bool {
	ranges := req["Range"]
	return len(ranges) > 0 && strings.HasPrefix(ranges[0], "bytes=") && strings.Count(ranges[0], ",") < maxRanges &&
		!strings.EqualFold(strings.TrimSpace(resp.Get("Accept-Ranges")), "none")
}

// serveRange answers a GET that wantsRange from e, whose header is in w: a
// 206 with the bytes asked for, a 416 when there are none, or the whole
// response when If-Range does not name it (§13.1.5).
//
// The standard library does it all, on a request that only has these two
// fields: the other conditions were evaluated before (§13.2.2, see serve).
func serveRange(w http.ResponseWriter, r *http.Request, e *entry) {
	ranged := &http.Request{Method: http.MethodGet, Header: http.Header{"Range": r.Header["Range"]}}
	if condition := r.Header["If-Range"]; len(condition) > 0 {
		ranged.Header["If-Range"] = condition
	}
	// If-Range is compared with the entity tag in the header of w, or with
	// this date.
	var modified time.Time
	if t, err := http.ParseTime(e.header.Get("Last-Modified")); err == nil {
		modified = t
	}
	http.ServeContent(w, ranged, "", modified, bytes.NewReader(e.body))
}
