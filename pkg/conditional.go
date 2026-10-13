package traedis

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Conditional requests (RFC 9110 §13, RFC 9111 §4.3). The cache only evaluates
// the conditions of a client against a response it has stored (§4.3.2):
// without one, they reach the backend as they are (RFC 9110 §13.2.1). When it
// has a stored response to validate, it sends the backend its validators
// instead (§4.3.1).

// conditions lists the request header fields the cache evaluates. If-Match and
// If-Unmodified-Since are for the backend, and If-Range goes with Range (see
// serveRange).
var conditions = []string{"If-None-Match", "If-Modified-Since"}

// conditional reports whether a request has conditions that the cache
// evaluates: its answer may be a 304.
func conditional(h http.Header) bool {
	for _, name := range conditions {
		if len(h.Values(name)) > 0 {
			return true
		}
	}
	return false
}

// notModified reports whether a stored 200 response with header resp, received
// at the given time, is the one the client says it has: the answer to the
// request header req is then a 304 (RFC 9110 §13.1, RFC 9111 §4.3.2).
// If-None-Match alone decides when it is there; If-Modified-Since is compared
// with Last-Modified, or with when the response was generated or received, and
// ignored unless it is one valid date (RFC 9110 §13.1.3).
func notModified(req, resp http.Header, received time.Time) bool {
	if tags := req.Values("If-None-Match"); len(tags) > 0 {
		return etagListed(tags, resp.Get("ETag"))
	}
	dates := req.Values("If-Modified-Since")
	if len(dates) != 1 {
		return false
	}
	since, err := http.ParseTime(dates[0])
	if err != nil {
		return false
	}
	modified := dateValue(resp, received.Truncate(time.Second))
	if t, err := http.ParseTime(resp.Get("Last-Modified")); err == nil {
		modified = t
	}
	return !modified.After(since)
}

// etagListed reports whether etag is one of the entity tags of If-None-Match
// field lines, by weak comparison (RFC 9110 §8.8.3.2). "*" is any response.
func etagListed(lines []string, etag string) bool {
	for _, line := range lines {
		for _, tag := range splitList(line) {
			if tag == "*" || (etag != "" && sameETag(tag, etag)) {
				return true
			}
		}
	}
	return false
}

// sameETag compares two entity tags weakly: whether they are weak or not.
func sameETag(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// representationMetadata lists the fields describing a representation
// (RFC 9110 §8) that a 304 does not carry: Content-Location and the validators
// are the ones it keeps.
var representationMetadata = []string{
	"Content-Type", "Content-Encoding", "Content-Language", "Content-Length", "Content-Range",
}

// notModifiedHeader makes h, the header of a stored response, the header of
// the 304 sent in its place (RFC 9110 §15.4.5): without representation
// metadata, but for Last-Modified when it is the only validator.
func notModifiedHeader(h http.Header) {
	for _, name := range representationMetadata {
		h.Del(name)
	}
	if h.Get("ETag") != "" {
		h.Del("Last-Modified")
	}
}

// backendRequest returns the request the backend gets for r when e is the
// stored response that could not be served: the validators of e in place of
// the conditions of the client (§4.3.1), which the cache answers from e once
// it is validated. validating tells whether that is so: a 304 is then about e.
// Otherwise it is r itself: without a stored response of its own to validate
// (none, no validator, a HEAD), the cache leaves the conditions to the backend
// (§4.3.2). r is never modified.
func backendRequest(r *http.Request, e *entry) (*http.Request, bool) {
	etag, modified := "", ""
	if e != nil && r.Method == http.MethodGet {
		etag = e.header.Get("ETag")
		modified = e.header.Get("Last-Modified")
	}
	if etag == "" && modified == "" {
		return r, false
	}
	out := r.Clone(r.Context())
	for _, name := range conditions {
		out.Header.Del(name)
	}
	if etag != "" {
		out.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		out.Header.Set("If-Modified-Since", modified)
	}
	return out, true
}

// validates tells what a 304 with header h says of e, the stored response that
// was validated (§4.3.4). current: e is the response the 304 is about, and may
// be reused. update: e gets the fields of the 304, which takes a validator
// that identifies it:
//   - a strong entity tag has to be the one of e;
//   - otherwise, a weak entity tag and Last-Modified have to correspond to
//     those of e;
//   - a 304 without validator only updates a response without validator: e
//     has some, it is reused as it is.
//
// A validator that is not the one of e is about another response: e is not
// current.
func validates(e *entry, h http.Header) (bool, bool) {
	etag := h.Get("ETag")
	modified := h.Get("Last-Modified")
	stored := e.header.Get("ETag")
	if etag != "" && !strings.HasPrefix(etag, "W/") {
		same := etag == stored
		return same, same
	}
	if etag == "" && modified == "" {
		return true, false
	}
	if etag != "" && (stored == "" || !sameETag(etag, stored)) {
		return false, false
	}
	if modified != "" && modified != e.header.Get("Last-Modified") {
		return false, false
	}
	return true, true
}

// headMatches reports whether the 200 response to a HEAD with header h is the
// stored response e (§4.3.5): the validators it has are those of e, and its
// Content-Length is the length of the body of e.
func headMatches(e *entry, h http.Header) bool {
	if etag := h.Get("ETag"); etag != "" && etag != e.header.Get("ETag") {
		return false
	}
	if modified := h.Get("Last-Modified"); modified != "" && modified != e.header.Get("Last-Modified") {
		return false
	}
	if length := h.Get("Content-Length"); length != "" && length != strconv.FormatInt(e.size, 10) {
		return false
	}
	return true
}

// kept lists the fields of a stored response that a 304 does not update
// (§3.2): its body and its place in the store depend on them.
var kept = []string{"Content-Encoding", "Content-Range", "Vary"}

// refreshed returns the stored response e updated by a 304, or by the response
// to a HEAD, with header h (§3.2, §4.3.4, §4.3.5): the same body, the fields of
// that response in place of its own, and the times of the exchange.
func refreshed(e *entry, h http.Header, requestTime, responseTime time.Time) *entry {
	header := e.header.Clone()
	// These belong to an exchange, not to the response: gone unless h has
	// them.
	header.Del("Age")
	header.Del("Date")
	update := endToEnd(h)
	for _, name := range kept {
		update.Del(name)
	}
	for name, values := range update {
		// A field set to no value is one the response does not have: a server
		// does that to the Content-Type of a 304, to keep net/http from guessing.
		if len(values) > 0 {
			header[name] = values
		}
	}
	// What the updated response forbids to store (§3.1) is not part of it.
	cc := parseCacheControl(header.Values("Cache-Control"))
	for _, name := range cc.unstored {
		header.Del(name)
	}
	out := &entry{
		status:       e.status,
		header:       header,
		body:         e.body,
		size:         e.size,
		requestTime:  requestTime,
		responseTime: responseTime,
		vary:         e.vary,
		raw:          e.raw,
	}
	setFreshness(out, cc)
	return out
}

// ownFields returns the fields of a 304 with header h that are for the client
// that caused the validation of e, and nobody else: Set-Cookie, and the fields
// that the 304 or e forbid to store. The stored response does not have them.
func ownFields(e *entry, h http.Header) http.Header {
	names := []string{"Set-Cookie"}
	names = append(names, parseCacheControl(h.Values("Cache-Control")).unstored...)
	names = append(names, parseCacheControl(e.header.Values("Cache-Control")).unstored...)
	own := http.Header{}
	for _, name := range names {
		if values := h.Values(name); len(values) > 0 {
			own[name] = append([]string(nil), values...)
		}
	}
	return own
}
