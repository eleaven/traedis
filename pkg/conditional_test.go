package traedis

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var lastModified = t0.Format(http.TimeFormat)

func TestNotModified(t *testing.T) {
	before := t0.Add(-time.Hour).Format(http.TimeFormat)
	after := t0.Add(time.Hour).Format(http.TimeFormat)
	tests := []struct {
		name string
		req  http.Header
		resp http.Header
		want bool
	}{
		{name: "no condition", req: http.Header{}, resp: http.Header{"Etag": {`"v1"`}}},
		{name: "RFC 9110 §13.1.2 same entity tag", req: http.Header{"If-None-Match": {`"v1"`}}, resp: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "RFC 9110 §13.1.2 another entity tag", req: http.Header{"If-None-Match": {`"v0"`}}, resp: http.Header{"Etag": {`"v1"`}}},
		{name: "RFC 9110 §13.1.2 one of a list", req: http.Header{"If-None-Match": {`"v0", "v1"`}}, resp: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "RFC 9110 §13.1.2 one of several lines", req: http.Header{"If-None-Match": {`"v0"`, `"v1"`}}, resp: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "RFC 9110 §13.1.2 * is any response", req: http.Header{"If-None-Match": {"*"}}, resp: http.Header{}, want: true},
		{name: "RFC 9110 §8.8.3.2 weak comparison: weak request tag", req: http.Header{"If-None-Match": {`W/"v1"`}}, resp: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "RFC 9110 §8.8.3.2 weak comparison: weak response tag", req: http.Header{"If-None-Match": {`"v1"`}}, resp: http.Header{"Etag": {`W/"v1"`}}, want: true},
		{name: "a comma inside an entity tag", req: http.Header{"If-None-Match": {`"a,b", "c"`}}, resp: http.Header{"Etag": {`"a"`}}},
		{name: "an entity tag with a comma", req: http.Header{"If-None-Match": {`"a,b", "c"`}}, resp: http.Header{"Etag": {`"a,b"`}}, want: true},
		{name: "response without entity tag", req: http.Header{"If-None-Match": {`"v1"`}}, resp: http.Header{}},
		{name: "RFC 9110 §13.1.3 If-Modified-Since is ignored with If-None-Match", req: http.Header{"If-None-Match": {`"v0"`}, "If-Modified-Since": {after}}, resp: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}}},
		{name: "RFC 9110 §13.1.3 not modified since", req: http.Header{"If-Modified-Since": {lastModified}}, resp: http.Header{"Last-Modified": {lastModified}}, want: true},
		{name: "RFC 9110 §13.1.3 a later date", req: http.Header{"If-Modified-Since": {after}}, resp: http.Header{"Last-Modified": {lastModified}}, want: true},
		{name: "RFC 9110 §13.1.3 modified since", req: http.Header{"If-Modified-Since": {before}}, resp: http.Header{"Last-Modified": {lastModified}}},
		{name: "RFC 9110 §13.1.3 invalid date", req: http.Header{"If-Modified-Since": {"yesterday"}}, resp: http.Header{"Last-Modified": {lastModified}}},
		{name: "RFC 9110 §13.1.3 several dates", req: http.Header{"If-Modified-Since": {after, after}}, resp: http.Header{"Last-Modified": {lastModified}}},
		{name: "§4.3.2 Date when there is no Last-Modified", req: http.Header{"If-Modified-Since": {lastModified}}, resp: http.Header{"Date": {lastModified}}, want: true},
		{name: "§4.3.2 generated after the date", req: http.Header{"If-Modified-Since": {before}}, resp: http.Header{"Date": {lastModified}}},
		{name: "§4.3.2 received time when there is no Date", req: http.Header{"If-Modified-Since": {lastModified}}, resp: http.Header{}, want: true},
		{name: "§4.3.2 received after the date", req: http.Header{"If-Modified-Since": {before}}, resp: http.Header{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Received within the second of t0: the dates of HTTP have no fraction.
			if got := notModified(tt.req, tt.resp, t0.Add(500*time.Millisecond)); got != tt.want {
				t.Errorf("notModified() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNotModifiedHeader(t *testing.T) {
	h := http.Header{
		"Content-Type": {"text/plain"}, "Content-Length": {"5"}, "Content-Encoding": {"gzip"},
		"Content-Language": {"fr"}, "Content-Range": {"bytes 0-4/5"}, "Content-Location": {"/a.fr"},
		"Etag": {`"v1"`}, "Last-Modified": {lastModified}, "Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"},
		"Expires": {lastModified}, "Date": {lastModified}, "X-Other": {"kept"},
	}
	notModifiedHeader(h)
	want := http.Header{
		"Content-Location": {"/a.fr"}, "Etag": {`"v1"`}, "Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"},
		"Expires": {lastModified}, "Date": {lastModified}, "X-Other": {"kept"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("RFC 9110 §15.4.5 header = %v, want %v", h, want)
	}

	h = http.Header{"Last-Modified": {lastModified}, "Content-Type": {"text/plain"}}
	notModifiedHeader(h)
	if h.Get("Last-Modified") != lastModified || h.Get("Content-Type") != "" {
		t.Errorf("Last-Modified is the validator of a response without ETag: %v", h)
	}
}

func TestBackendRequest(t *testing.T) {
	validators := &entry{header: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}}}
	conditions := http.Header{"If-None-Match": {`"v0"`}, "If-Modified-Since": {lastModified}}
	tests := []struct {
		name       string
		method     string
		reqHdr     http.Header
		stored     *entry
		validating bool
		wantINM    string
		wantIMS    string
	}{
		{name: "§4.3.2 no stored response: the request as it is", reqHdr: conditions, wantINM: `"v0"`, wantIMS: lastModified},
		{name: "§4.3.2 stored response without validators: the request as it is", reqHdr: conditions, stored: &entry{header: http.Header{}}, wantINM: `"v0"`, wantIMS: lastModified},
		{name: "RFC 9110 §13.2.1 no conditional HEAD: the request as it is", method: http.MethodHead, reqHdr: conditions, stored: validators, wantINM: `"v0"`, wantIMS: lastModified},
		{name: "§4.3.1 validators of the stored response", stored: validators, validating: true, wantINM: `"v1"`, wantIMS: lastModified},
		{name: "§4.3.1 in place of the conditions of the client", reqHdr: http.Header{"If-None-Match": {`"v0"`}}, stored: validators, validating: true, wantINM: `"v1"`, wantIMS: lastModified},
		{name: "§4.3.1 entity tag only", reqHdr: conditions, stored: &entry{header: http.Header{"Etag": {`W/"v1"`}}}, validating: true, wantINM: `W/"v1"`},
		{name: "§4.3.1 Last-Modified only", reqHdr: http.Header{"If-None-Match": {`"v0"`}}, stored: &entry{header: http.Header{"Last-Modified": {lastModified}}}, validating: true, wantIMS: lastModified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, testURL, nil)
			req = req.WithContext(contextWithSpan(req))
			for k, v := range tt.reqHdr {
				req.Header[k] = v
			}
			req.Header.Set("If-Range", `"v0"`)
			req.Header.Set("If-Match", `"v0"`)
			req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			sent := req.Header.Clone()

			out, validating := backendRequest(req, tt.stored)
			if validating != tt.validating || (out == req) == tt.validating {
				t.Errorf("backendRequest(): validating = %v, same request = %v", validating, out == req)
			}
			if got := out.Header.Get("If-None-Match"); got != tt.wantINM {
				t.Errorf("If-None-Match = %q, want %q", got, tt.wantINM)
			}
			if got := out.Header.Get("If-Modified-Since"); got != tt.wantIMS {
				t.Errorf("If-Modified-Since = %q, want %q", got, tt.wantIMS)
			}
			if out.Header.Get("If-Range") == "" || out.Header.Get("If-Match") == "" || out.Header.Get("Traceparent") == "" {
				t.Errorf("the other header fields must be sent: %v", out.Header)
			}
			if out.Context().Value(ctxKey{}) != "span" || out.Method != method {
				t.Error("the context and the method must be those of the request")
			}
			if !reflect.DeepEqual(req.Header, sent) {
				t.Errorf("the request of the client must not be modified: %v", req.Header)
			}
		})
	}
}

func TestValidates(t *testing.T) {
	earlier := t0.Add(-time.Hour).Format(http.TimeFormat)
	tests := []struct {
		name        string
		stored      http.Header
		h           http.Header // of the 304
		wantCurrent bool
		wantUpdate  bool
	}{
		{name: "§4.3.4 same strong validator", stored: http.Header{"Etag": {`"v1"`}}, h: http.Header{"Etag": {`"v1"`}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 another strong validator", stored: http.Header{"Etag": {`"v1"`}}, h: http.Header{"Etag": {`"v2"`}}},
		{name: "§4.3.4 a strong validator is not a weak one", stored: http.Header{"Etag": {`W/"v1"`}}, h: http.Header{"Etag": {`"v1"`}}},
		{name: "§4.3.4 strong validator for a response stored without one", stored: http.Header{"Last-Modified": {lastModified}}, h: http.Header{"Etag": {`"v1"`}}},
		{name: "§4.3.4 Last-Modified does not matter with a strong validator", stored: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}}, h: http.Header{"Etag": {`"v1"`}, "Last-Modified": {earlier}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 corresponding weak validator", stored: http.Header{"Etag": {`W/"v1"`}}, h: http.Header{"Etag": {`W/"v1"`}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 weak validator of a strong stored one", stored: http.Header{"Etag": {`"v1"`}}, h: http.Header{"Etag": {`W/"v1"`}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 another weak validator", stored: http.Header{"Etag": {`W/"v1"`}}, h: http.Header{"Etag": {`W/"v2"`}}},
		{name: "§4.3.4 weak validator for a response stored without one", stored: http.Header{"Last-Modified": {lastModified}}, h: http.Header{"Etag": {`W/"v1"`}}},
		{name: "§4.3.4 corresponding Last-Modified", stored: http.Header{"Last-Modified": {lastModified}}, h: http.Header{"Last-Modified": {lastModified}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 Last-Modified of a response with an entity tag", stored: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}}, h: http.Header{"Last-Modified": {lastModified}}, wantCurrent: true, wantUpdate: true},
		{name: "§4.3.4 another Last-Modified", stored: http.Header{"Last-Modified": {lastModified}}, h: http.Header{"Last-Modified": {earlier}}},
		{name: "§4.3.4 weak validators must all correspond", stored: http.Header{"Etag": {`W/"v1"`}, "Last-Modified": {lastModified}}, h: http.Header{"Etag": {`W/"v1"`}, "Last-Modified": {earlier}}},
		{name: "§4.3.4 no validator: reused, not updated", stored: http.Header{"Etag": {`"v1"`}}, h: http.Header{"Cache-Control": {"max-age=60"}}, wantCurrent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current, update := validates(&entry{header: tt.stored}, tt.h)
			if current != tt.wantCurrent || update != tt.wantUpdate {
				t.Errorf("validates() = %v, %v; want %v, %v", current, update, tt.wantCurrent, tt.wantUpdate)
			}
		})
	}
}

func TestHeadMatches(t *testing.T) {
	stored := &entry{header: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}}, size: 5}
	tests := []struct {
		name string
		h    http.Header
		want bool
	}{
		{name: "§4.3.5 same validators and length", h: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lastModified}, "Content-Length": {"5"}}, want: true},
		{name: "§4.3.5 only the fields it has are compared", h: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "§4.3.5 no validator nor length", h: http.Header{}, want: true},
		{name: "§4.3.5 another entity tag", h: http.Header{"Etag": {`"v2"`}, "Content-Length": {"5"}}},
		{name: "§4.3.5 a weak entity tag is another value", h: http.Header{"Etag": {`W/"v1"`}}},
		{name: "§4.3.5 another Last-Modified", h: http.Header{"Etag": {`"v1"`}, "Last-Modified": {t0.Add(time.Hour).Format(http.TimeFormat)}}},
		{name: "§4.3.5 another length", h: http.Header{"Etag": {`"v1"`}, "Content-Length": {"6"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := headMatches(stored, tt.h); got != tt.want {
				t.Errorf("headMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefreshed(t *testing.T) {
	old := t0.Add(-time.Hour)
	e := &entry{
		status: 200,
		header: http.Header{
			"Cache-Control": {"max-age=60"}, "Etag": {`"v1"`}, "Content-Type": {"text/plain"}, "Content-Encoding": {"gzip"},
			"Vary": {"Accept-Encoding"}, "Date": {old.Format(http.TimeFormat)}, "Age": {"30"}, "X-Kept": {"stored"}, "Expires": {"0"},
		},
		body:        []byte("body"),
		vary:        http.Header{"Accept-Encoding": {"gzip"}},
		requestTime: old, responseTime: old,
	}
	stored := e.header.Clone()
	h := http.Header{
		"Cache-Control": {"max-age=120"}, "Etag": {`"v1"`}, "X-New": {"added"},
		// Not updated: the stored body and its field depend on them.
		"Content-Encoding": {"br"}, "Vary": {"Accept-Language"}, "Content-Range": {"bytes 0-1/4"},
		// Never stored.
		"Content-Length": {"0"}, "Connection": {"X-Hop"}, "X-Hop": {"1"},
		// §3.1: stored like any other field, unless Cache-Control excludes it.
		"Set-Cookie": {"sid=1"},
		// Regression: Traefik sends its 304 with a Content-Type set to no value,
		// which removed the stored one.
		"Content-Type": nil,
	}
	got := refreshed(e, h, t0, t0.Add(time.Second))

	want := http.Header{
		"Cache-Control": {"max-age=120"}, "Etag": {`"v1"`}, "Content-Type": {"text/plain"}, "Content-Encoding": {"gzip"},
		"Vary": {"Accept-Encoding"}, "X-Kept": {"stored"}, "Expires": {"0"}, "X-New": {"added"}, "Set-Cookie": {"sid=1"},
	}
	if !reflect.DeepEqual(got.header, want) {
		t.Errorf("§3.2 header = %v, want %v", got.header, want)
	}
	if got.status != 200 || string(got.body) != "body" || !reflect.DeepEqual(got.vary, e.vary) {
		t.Errorf("§4.3.4 the response is the stored one: %d %q %v", got.status, got.body, got.vary)
	}
	if !got.requestTime.Equal(t0) || !got.responseTime.Equal(t0.Add(time.Second)) {
		t.Errorf("§4.2.3 the age is counted from the validation: %v, %v", got.requestTime, got.responseTime)
	}
	if !reflect.DeepEqual(e.header, stored) {
		t.Error("the stored entry must not be modified")
	}

	// §3.1: the fields that the updated response forbids to store, whether the
	// 304 or the stored response says so.
	h = http.Header{"Cache-Control": {`max-age=120, private="Set-Cookie"`}, "Set-Cookie": {"sid=1"}, "X-New": {"added"}}
	got = refreshed(e, h, t0, t0)
	if len(got.header.Values("Set-Cookie")) != 0 || got.header.Get("X-New") != "added" {
		t.Errorf("§3.1 excluded by the 304: %v", got.header)
	}
	excluding := &entry{status: 200, header: http.Header{"Cache-Control": {`max-age=60, no-cache="Set-Cookie"`}}, requestTime: old, responseTime: old}
	got = refreshed(excluding, http.Header{"Set-Cookie": {"sid=1"}, "X-New": {"added"}}, t0, t0)
	if len(got.header.Values("Set-Cookie")) != 0 || got.header.Get("X-New") != "added" {
		t.Errorf("§3.1 excluded by the stored response: %v", got.header)
	}

	// The 304 has its own Date and Age.
	h = http.Header{"Date": {t0.Format(http.TimeFormat)}, "Age": {"5"}}
	got = refreshed(e, h, t0, t0)
	if got.header.Get("Date") != t0.Format(http.TimeFormat) || got.header.Get("Age") != "5" {
		t.Errorf("Date = %q, Age = %q", got.header.Get("Date"), got.header.Get("Age"))
	}
}

// origin is a backend with validators: it answers the conditions matching its
// current response with a 304.
type origin struct {
	etag         string
	lastModified string
	cacheControl string
	body         string
	status       int
	extra        http.Header // other response fields, of its 304 too
	etagOf304    string      // when it is not etag
	bare304      bool        // its 304 has no validator
	calls        int
	validations  int // 304 sent
	lastReq      *http.Request
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls++
	o.lastReq = r
	h := w.Header()
	for name, values := range o.extra {
		for _, v := range values {
			h.Add(name, v)
		}
	}
	if o.cacheControl != "" {
		h.Set("Cache-Control", o.cacheControl)
	}
	if o.etag != "" {
		h.Set("ETag", o.etag)
	}
	if o.lastModified != "" {
		h.Set("Last-Modified", o.lastModified)
	}
	matched := false
	if tag := r.Header.Get("If-None-Match"); tag != "" {
		matched = tag == o.etag
	} else if since := r.Header.Get("If-Modified-Since"); since != "" {
		matched = since == o.lastModified
	}
	if matched {
		o.validations++
		if o.etagOf304 != "" {
			h.Set("ETag", o.etagOf304)
		}
		if o.bare304 {
			h.Del("ETag")
			h.Del("Last-Modified")
		}
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/plain")
	h.Set("Content-Length", strconv.Itoa(len(o.body)))
	if o.status != 0 {
		w.WriteHeader(o.status)
	}
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, o.body)
	}
}

func newOrigin() *origin {
	return &origin{etag: `"v1"`, lastModified: lastModified, cacheControl: "max-age=60", body: "hello"}
}

func TestClientConditionsOnHit(t *testing.T) {
	later := t0.Add(time.Hour).Format(http.TimeFormat)
	earlier := t0.Add(-time.Hour).Format(http.TimeFormat)
	tests := []struct {
		name   string
		method string
		reqHdr http.Header
		want   int
	}{
		{name: "no condition", want: 200},
		{name: "§4.3.2 If-None-Match with the stored entity tag", reqHdr: http.Header{"If-None-Match": {`"v1"`}}, want: 304},
		{name: "§4.3.2 weak comparison", reqHdr: http.Header{"If-None-Match": {`W/"v1"`}}, want: 304},
		{name: "§4.3.2 list of entity tags", reqHdr: http.Header{"If-None-Match": {`"v0", "v1"`}}, want: 304},
		{name: "§4.3.2 If-None-Match: *", reqHdr: http.Header{"If-None-Match": {"*"}}, want: 304},
		{name: "§4.3.2 another entity tag", reqHdr: http.Header{"If-None-Match": {`"v0"`}}, want: 200},
		{name: "§4.3.2 If-Modified-Since with Last-Modified", reqHdr: http.Header{"If-Modified-Since": {lastModified}}, want: 304},
		{name: "§4.3.2 If-Modified-Since later", reqHdr: http.Header{"If-Modified-Since": {later}}, want: 304},
		{name: "§4.3.2 If-Modified-Since earlier", reqHdr: http.Header{"If-Modified-Since": {earlier}}, want: 200},
		{name: "§4.3.2 If-None-Match wins over If-Modified-Since", reqHdr: http.Header{"If-None-Match": {`"v0"`}, "If-Modified-Since": {later}}, want: 200},
		{name: "§4.3.2 HEAD", method: http.MethodHead, reqHdr: http.Header{"If-None-Match": {`"v1"`}}, want: 304},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			c, _ := newTestCache(t, o, nil)
			doRequest(c, http.MethodGet, testURL, nil)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := doRequest(c, method, testURL, tt.reqHdr)

			if o.calls != 1 || !isHit(lastCacheStatus(rec)) {
				t.Fatalf("expected a hit: %q (backend calls %d)", lastCacheStatus(rec), o.calls)
			}
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			h := rec.Header()
			if h.Get("ETag") != `"v1"` || h.Get("Cache-Control") != "max-age=60" || h.Get("Age") != "0" {
				t.Errorf("RFC 9110 §15.4.5 the response must have its ETag, Cache-Control and Age: %v", h)
			}
			if tt.want == 200 {
				if h.Get("Content-Length") != "5" || h.Get("Content-Type") != "text/plain" || h.Get("Last-Modified") != lastModified {
					t.Errorf("header = %v", h)
				}
				return
			}
			if rec.Body.Len() != 0 || h.Get("Content-Length") != "" || h.Get("Content-Type") != "" || h.Get("Last-Modified") != "" {
				t.Errorf("RFC 9110 §15.4.5 a 304 has no body nor the fields describing it: %q %v", rec.Body.String(), h)
			}
		})
	}
}

func TestClientConditionsOnHitWithoutValidators(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, nil)
	first := doRequest(c, http.MethodGet, testURL, nil)

	// §4.3.2: without Last-Modified, the response is as old as it was received.
	since := time.Now().Add(time.Second).UTC().Format(http.TimeFormat)
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"If-Modified-Since": {since}})
	if rec.Code != 304 || rec.Body.Len() != 0 || b.calls != 1 {
		t.Errorf("got %d %q (backend calls %d)", rec.Code, rec.Body.String(), b.calls)
	}
	rec = doRequest(c, http.MethodGet, testURL, http.Header{"If-Modified-Since": {lastModified}})
	if rec.Code != 200 || rec.Body.String() != first.Body.String() {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
	rec = doRequest(c, http.MethodGet, testURL, http.Header{"If-None-Match": {`"v1"`}})
	if rec.Code != 200 {
		t.Errorf("a response without entity tag matches none: got %d", rec.Code)
	}
}

// Without a stored response, the conditions of the client are for the backend
// (§4.3.2, RFC 9110 §13.2.1): the cache does not evaluate them.
func TestClientConditionsWithoutStoredResponse(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		reqHdr     http.Header
		want       int
		wantStatus string
		wantStored bool
	}{
		{name: "§4.3.2 the 304 of the backend goes to the client", reqHdr: http.Header{"If-None-Match": {`"v1"`}}, want: 304, wantStatus: "traedis; fwd=uri-miss; fwd-status=304"},
		{name: "§4.3.2 If-Modified-Since", reqHdr: http.Header{"If-Modified-Since": {lastModified}}, want: 304, wantStatus: "traedis; fwd=uri-miss; fwd-status=304"},
		{name: "§4.3.2 a whole response is stored", reqHdr: http.Header{"If-None-Match": {`"v0"`}}, want: 200, wantStatus: "traedis; fwd=uri-miss; fwd-status=200", wantStored: true},
		{name: "§4.3.2 HEAD", method: http.MethodHead, reqHdr: http.Header{"If-None-Match": {`"v1"`}}, want: 304, wantStatus: "traedis; fwd=uri-miss; fwd-status=304"},
		{name: "§5.2.1.4 request no-cache", reqHdr: http.Header{"If-None-Match": {`"v1"`}, "Cache-Control": {"no-cache"}}, want: 304, wantStatus: "traedis; fwd=request; fwd-status=304"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			c, st := newTestCache(t, o, nil)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, testURL, nil)
			for k, v := range tt.reqHdr {
				req.Header[k] = v
			}
			rec := httptest.NewRecorder()
			c.ServeHTTP(rec, req)

			if o.lastReq != req {
				t.Error("the backend must get the request of the client itself")
			}
			if rec.Code != tt.want || lastCacheStatus(rec) != tt.wantStatus {
				t.Errorf("got %d %q, want %d %q", rec.Code, lastCacheStatus(rec), tt.want, tt.wantStatus)
			}
			if tt.want == 304 && (o.validations != 1 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"v1"`) {
				t.Errorf("the 304 must be the one of the backend: %v (304 sent %d)", rec.Header(), o.validations)
			}
			if _, stored := st.lookup(cacheKey(cacheURI(req, false)), ""); stored != tt.wantStored {
				t.Errorf("stored = %v, want %v", stored, tt.wantStored)
			}
		})
	}
}

func TestOtherPreconditionsReachTheBackend(t *testing.T) {
	o := newOrigin()
	c, st := newTestCache(t, o, nil)
	storeValidated(t, st, "max-age=60", 2*time.Minute)
	hdr := http.Header{"If-Match": {`"v1"`}, "If-Unmodified-Since": {lastModified}, "If-Range": {`"v1"`}, "Range": {"bytes=0-1"}, "If-None-Match": {`"v0"`}}
	doRequest(c, http.MethodGet, staleURL, hdr)
	for _, name := range []string{"If-Match", "If-Unmodified-Since", "If-Range", "Range"} {
		if o.lastReq.Header.Get(name) != hdr.Get(name) {
			t.Errorf("§4.3.2 %s is for the backend, even in a validation: got %q", name, o.lastReq.Header.Get(name))
		}
	}
	if o.lastReq.Header.Get("If-None-Match") != `"v1"` {
		t.Errorf("If-None-Match = %q, want the stored entity tag", o.lastReq.Header.Get("If-None-Match"))
	}
}

// A cache that is bypassed stores nothing: the request is left as it is.
func TestClientConditionsWhenBypassed(t *testing.T) {
	o := newOrigin()
	c, st := newTestCache(t, o, nil)
	st.err = errors.New("redis down")
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"If-None-Match": {`"v1"`}})
	if o.lastReq.Header.Get("If-None-Match") != `"v1"` || o.validations != 1 {
		t.Errorf("the backend must get the conditions of the client: %v", o.lastReq.Header)
	}
	if rec.Code != 304 || lastCacheStatus(rec) != "traedis; fwd=bypass; fwd-status=304; detail=redis" {
		t.Errorf("got %d %q", rec.Code, lastCacheStatus(rec))
	}
}

// storeValidated stores at staleURL a response of newOrigin received age ago.
func storeValidated(t *testing.T, st *memStore, cacheControl string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	storeEntry(t, st, staleURL, &entry{
		status: 200,
		header: http.Header{
			"Cache-Control": {cacheControl}, "Content-Type": {"text/plain"}, "Etag": {`"v1"`}, "Last-Modified": {lastModified},
			"Date": {at.UTC().Format(http.TimeFormat)}, "X-Kept": {"stored"},
		},
		body:        []byte("old"),
		requestTime: at, responseTime: at,
	})
}

func storedAt(t *testing.T, c *cache, st *memStore, uri, field string) *entry {
	t.Helper()
	if _, ok := st.lookup(cacheKey(uri), field); !ok {
		return nil
	}
	e := st.entry(cacheKey(uri), field)
	if e == nil {
		t.Fatal("the field must hold an entry")
	}
	return e
}

const validated = "traedis; fwd=stale; fwd-status=304; ttl="

func TestValidation(t *testing.T) {
	o := newOrigin()
	o.cacheControl = "max-age=120"
	o.extra = http.Header{"X-New": {"added"}, "Cache-Status": {"origin; hit"}}
	c, st := newTestCache(t, o, nil)
	storeValidated(t, st, "max-age=60", 2*time.Minute)

	req := httptest.NewRequest(http.MethodGet, staleURL, nil)
	req = req.WithContext(contextWithSpan(req))
	req.Header.Set("Range", "bytes=0-1")
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Before", "kept") // set by a middleware before the cache
	c.ServeHTTP(rec, req)

	got := o.lastReq
	if o.calls != 1 || got.Header.Get("If-None-Match") != `"v1"` || got.Header.Get("If-Modified-Since") != lastModified {
		t.Fatalf("§4.3.1 the backend must get the validators of the stored response: %v", got.Header)
	}
	if got.Context().Value(ctxKey{}) != "span" || got.Header.Get("Range") != "bytes=0-1" {
		t.Error("the context and the other fields of the request must reach the backend")
	}
	if req.Header.Get("If-None-Match") != "" {
		t.Error("the request of the client must not be modified")
	}
	if rec.Code != 200 || rec.Body.String() != "old" || rec.Header().Get("Content-Length") != "3" {
		t.Fatalf("§4.3.3 the stored response must be served: got %d %q", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Cache-Control") != "max-age=120" || h.Get("X-New") != "added" || h.Get("X-Kept") != "stored" || h.Get("Content-Type") != "text/plain" {
		t.Errorf("§4.3.4 the stored fields are updated by those of the 304: %v", h)
	}
	if h.Get("Age") != "0" || h.Get("X-Before") != "kept" {
		t.Errorf("Age = %q, X-Before = %q", h.Get("Age"), h.Get("X-Before"))
	}
	status := h.Values("Cache-Status")
	if len(status) != 2 || status[0] != "origin; hit" || (status[1] != validated+"120" && status[1] != validated+"119") {
		t.Errorf("Cache-Status = %q, want %q120", status, validated)
	}

	e := storedAt(t, c, st, staleURL, "")
	if e == nil || st.sets != 2 || st.dels != 0 {
		t.Fatalf("the entry must be stored again (sets %d, dels %d)", st.sets, st.dels)
	}
	if string(e.body) != "old" || e.header.Get("Cache-Control") != "max-age=120" || e.header.Get("X-New") != "added" || time.Since(e.responseTime) > time.Minute {
		t.Errorf("§4.3.4 stored entry: %q %v, received %v", e.body, e.header, e.responseTime)
	}
	if v, _ := st.lookup(cacheKey(staleURL), ""); !closeTo(v.ttl, 2*time.Minute+time.Hour) {
		t.Errorf("stored ttl = %v", v.ttl)
	}
	if v, _ := st.lookup(cacheKey(staleURL), bodyField("")); !closeTo(v.ttl, 2*time.Minute+time.Hour) {
		t.Errorf("ttl of the body = %v: it must be kept for as long as its entry", v.ttl)
	}
	if len(e.header.Values("Cache-Status")) != 1 {
		t.Errorf("our Cache-Status must not be stored: %q", e.header.Values("Cache-Status"))
	}

	next := doRequest(c, http.MethodGet, staleURL, nil)
	if o.calls != 1 || !isHit(lastCacheStatus(next)) || next.Body.String() != "old" {
		t.Errorf("the entry is fresh again: %q (backend calls %d)", lastCacheStatus(next), o.calls)
	}
}

func TestValidationOutcome(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string // of the stored response, 2 minutes old
		method       string
		reqHdr       http.Header
		origin       func(o *origin)
		wantCode     int
		wantBody     string
		wantStatus   string // prefix of Cache-Status
		wantCalls    int
		wantSent     string // If-None-Match the backend got last
		wantStored   string // body of the entry afterwards; "": deleted
		wantSets     int
	}{
		{
			name:     "§4.3.3 304: the stored response is served and refreshed",
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 2,
		},
		{
			name: "§4.3.3 a whole response replaces the stored one", origin: func(o *origin) { o.etag = `"v2"` },
			wantCode: 200, wantBody: "hello", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1, wantSent: `"v1"`, wantStored: "hello", wantSets: 2,
		},
		{
			name: "§4.3.2 the client has the validated response", reqHdr: http.Header{"If-None-Match": {`"v1"`}},
			wantCode: 304, wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 2,
		},
		{
			name: "§4.3.2 the client has another response", reqHdr: http.Header{"If-None-Match": {`"v0"`}},
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 2,
		},
		{
			name: "§4.3.3 a whole response is sent as it is, whatever the client has", reqHdr: http.Header{"If-None-Match": {`"v2"`}}, origin: func(o *origin) { o.etag = `"v2"` },
			wantCode: 200, wantBody: "hello", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1, wantSent: `"v1"`, wantStored: "hello", wantSets: 2,
		},
		{
			name: "§5.2.1.1 request max-age=0 on a fresh response", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}},
			wantCode: 200, wantBody: "old", wantStatus: "traedis; fwd=request; fwd-status=304; ttl=", wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 2,
		},
		{
			name: "§5.2.2.2 must-revalidate", cacheControl: "max-age=60, must-revalidate",
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 2,
		},
		{
			name: "§5.2.1.5 request no-store: served, not stored again", reqHdr: http.Header{"Cache-Control": {"no-store"}},
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 1,
		},
		{
			name: "§5.2.1.4 request no-cache: the cache is not read", reqHdr: http.Header{"Cache-Control": {"no-cache"}},
			wantCode: 200, wantBody: "hello", wantStatus: "traedis; fwd=request; fwd-status=200", wantCalls: 1, wantStored: "hello", wantSets: 2,
		},
		{
			name: "RFC 9110 §13.2.1 a HEAD is not conditional", method: http.MethodHead,
			wantCode: 200, wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1, wantStored: "old", wantSets: 1,
		},
		{
			name: "§4.3.4 304 without validator: the stored response is served, not updated", origin: func(o *origin) { o.bare304 = true; o.cacheControl = "max-age=120" },
			wantCode: 200, wantBody: "old", wantStatus: "traedis; fwd=stale; fwd-status=304; ttl=-", wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 1,
		},
		{
			name: "§4.3.4 304 about another response: the backend is asked again", origin: func(o *origin) { o.etagOf304 = `"v2"` },
			wantCode: 200, wantBody: "hello", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 2, wantStored: "hello", wantSets: 2,
		},
		{
			name: "§5.2.2.5 304 with no-store: served, then deleted", origin: func(o *origin) { o.cacheControl = "no-store" },
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantSets: 1,
		},
		{
			name: "304 that leaves the response stale: served, then deleted", origin: func(o *origin) { o.cacheControl = "max-age=0" },
			wantCode: 200, wantBody: "old", wantStatus: validated, wantCalls: 1, wantSent: `"v1"`, wantSets: 1,
		},
		{
			name: "RFC 5861 §4 backend error", cacheControl: "max-age=60, stale-if-error=300", origin: func(o *origin) { o.status = 503; o.etag = `"v2"`; o.body = "error" },
			wantCode: 200, wantBody: "old", wantStatus: "traedis; fwd=stale; fwd-status=503; ttl=-", wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 1,
		},
		{
			name: "backend error without stale-if-error", origin: func(o *origin) { o.status = 503; o.etag = `"v2"`; o.body = "error" },
			wantCode: 503, wantBody: "error", wantStatus: "traedis; fwd=stale; fwd-status=503", wantCalls: 1, wantSent: `"v1"`, wantStored: "old", wantSets: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			if tt.origin != nil {
				tt.origin(o)
			}
			c, st := newTestCache(t, o, nil)
			cacheControl := tt.cacheControl
			if cacheControl == "" {
				cacheControl = "max-age=60"
			}
			storeValidated(t, st, cacheControl, 2*time.Minute)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := doRequest(c, method, staleURL, tt.reqHdr)

			if rec.Code != tt.wantCode || rec.Body.String() != tt.wantBody {
				t.Errorf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if got := lastCacheStatus(rec); !strings.HasPrefix(got, tt.wantStatus) {
				t.Errorf("Cache-Status = %q, want %q…", got, tt.wantStatus)
			}
			if o.calls != tt.wantCalls {
				t.Errorf("backend calls = %d, want %d", o.calls, tt.wantCalls)
			}
			if got := o.lastReq.Header.Get("If-None-Match"); got != tt.wantSent {
				t.Errorf("If-None-Match sent = %q, want %q", got, tt.wantSent)
			}
			body := ""
			if e := storedAt(t, c, st, staleURL, ""); e != nil {
				body = string(e.body)
			}
			if body != tt.wantStored || st.sets != tt.wantSets {
				t.Errorf("stored body = %q, sets = %d; want %q, %d", body, st.sets, tt.wantStored, tt.wantSets)
			}
		})
	}
}

func TestValidationWithoutValidators(t *testing.T) {
	o := newOrigin()
	c, st := newTestCache(t, o, nil)
	storeAged(t, st, "max-age=60", 2*time.Minute)
	rec := doRequest(c, http.MethodGet, staleURL, http.Header{"If-None-Match": {`"v0"`}})
	if o.lastReq.Header.Get("If-None-Match") != `"v0"` || o.lastReq.Header.Get("If-Modified-Since") != "" {
		t.Errorf("a stored response without validators is not validated: the request goes as it is: %v", o.lastReq.Header)
	}
	if rec.Code != 200 || rec.Body.String() != "hello" || lastCacheStatus(rec) != "traedis; fwd=stale; fwd-status=200" {
		t.Errorf("got %d %q %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
	}
}

func TestValidationByLastModified(t *testing.T) {
	o := newOrigin()
	o.etag = ""
	c, st := newTestCache(t, o, nil)
	at := time.Now().Add(-2 * time.Minute)
	storeEntry(t, st, staleURL, &entry{
		status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Last-Modified": {lastModified}}, body: []byte("old"),
		requestTime: at, responseTime: at,
	})
	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if o.lastReq.Header.Get("If-Modified-Since") != lastModified || o.lastReq.Header.Get("If-None-Match") != "" {
		t.Errorf("§4.3.1 If-Modified-Since must hold the stored Last-Modified: %v", o.lastReq.Header)
	}
	if rec.Body.String() != "old" || !strings.HasPrefix(lastCacheStatus(rec), validated) {
		t.Errorf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
}

// The Set-Cookie of a 304 is for the client that caused it, as on a miss.
func TestValidationWithSetCookie(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string // of the 304
		wantStored   bool
	}{
		{name: `§5.2.2.7 private="Set-Cookie": the entry is refreshed without the cookie`, cacheControl: `max-age=60, private="Set-Cookie"`, wantStored: true},
		{name: `§5.2.2.4 no-cache="Set-Cookie": the entry is refreshed without the cookie`, cacheControl: `max-age=60, no-cache="Set-Cookie"`, wantStored: true},
		{name: "§3.1 a response with a cookie that is not excluded may not be stored", cacheControl: "max-age=60"},
		{name: "public does not make a cookie shareable", cacheControl: "public, max-age=60"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			o.cacheControl = tt.cacheControl
			o.extra = http.Header{"Set-Cookie": {"sid=1", "theme=dark"}}
			c, st := newTestCache(t, o, nil)
			storeValidated(t, st, "max-age=60", 2*time.Minute)

			rec := httptest.NewRecorder()
			rec.Header().Add("Set-Cookie", "before=1") // set by a middleware before the cache
			c.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, staleURL, nil))
			if rec.Body.String() != "old" || !strings.HasPrefix(lastCacheStatus(rec), validated) {
				t.Fatalf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
			}
			if got := rec.Header().Values("Set-Cookie"); !reflect.DeepEqual(got, []string{"before=1", "sid=1", "theme=dark"}) {
				t.Errorf("Set-Cookie = %q", got)
			}
			e := storedAt(t, c, st, staleURL, "")
			if (e != nil) != tt.wantStored {
				t.Fatalf("stored = %v, want %v", e != nil, tt.wantStored)
			}
			if e == nil {
				return
			}
			if len(e.header.Values("Set-Cookie")) != 0 || time.Since(e.responseTime) > time.Minute {
				t.Errorf("stored entry: %v, received %v", e.header, e.responseTime)
			}
			if next := doRequest(c, http.MethodGet, staleURL, nil); next.Header().Get("Set-Cookie") != "" || !isHit(lastCacheStatus(next)) {
				t.Errorf("a hit must not replay Set-Cookie: %q %q", next.Header().Values("Set-Cookie"), lastCacheStatus(next))
			}
		})
	}
}

// A validated variant is stored again in its field, as it was selected.
func TestValidationOfVariant(t *testing.T) {
	o := newOrigin()
	o.extra = http.Header{"Vary": {"Accept-Language, Accept-Encoding"}}
	c, st := newTestCache(t, o, nil)
	names := []string{"Accept-Encoding", "Accept-Language"}
	key := cacheKey(staleURL)
	at := time.Now().Add(-2 * time.Minute)
	fields := map[string]string{}
	for _, lang := range []string{"fr", "en"} {
		e := &entry{
			status:      200,
			header:      http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language, Accept-Encoding"}, "Etag": {`"v1"`}},
			body:        []byte("old " + lang),
			vary:        http.Header{"Accept-Language": {lang}, "Accept-Encoding": {"br,gzip"}},
			requestTime: at, responseTime: at,
		}
		fields[lang] = variantField(names, http.Header{"Accept-Language": {lang}}, identity)
		mk := encodeMarker(marker{names: names, codings: []string{"zstd", identity}})
		if err := st.put(key, fields[lang], e, mk, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	// It accepts fewer codings than the request the variant was stored for.
	rec := doRequest(c, http.MethodGet, staleURL, http.Header{"Accept-Language": {"fr"}})
	if rec.Body.String() != "old fr" || !strings.HasPrefix(lastCacheStatus(rec), validated) || o.lastReq.Header.Get("If-None-Match") != `"v1"` {
		t.Fatalf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	fr := storedAt(t, c, st, staleURL, fields["fr"])
	if fr == nil || time.Since(fr.responseTime) > time.Minute || string(fr.body) != "old fr" {
		t.Fatalf("the variant must be refreshed in its field: %+v", fr)
	}
	if !reflect.DeepEqual(fr.vary, http.Header{"Accept-Language": {"fr"}, "Accept-Encoding": {"br,gzip"}}) {
		t.Errorf("the variant must keep what it was selected by: %v", fr.vary)
	}
	if en := storedAt(t, c, st, staleURL, fields["en"]); en == nil || time.Since(en.responseTime) < time.Minute {
		t.Error("the other variant must be left as it was")
	}
	if m := func() marker { v, _ := st.lookup(key, ""); mk, _ := decodeMarker(v.value); return mk }(); !sameStrings(m.names, names) || !sameStrings(m.codings, []string{"zstd", identity}) {
		t.Errorf("marker = %+v", m)
	}
	if n, _ := st.count(context.Background(), key); n != 5 {
		t.Errorf("fields = %d, want the marker and the 2 fields of 2 variants", n)
	}
	browser := http.Header{"Accept-Language": {"fr"}, "Accept-Encoding": {"gzip, br"}}
	if next := doRequest(c, http.MethodGet, staleURL, browser); !isHit(lastCacheStatus(next)) || next.Body.String() != "old fr" || o.calls != 1 {
		t.Errorf("the variant is fresh again, for the same requests: %q", lastCacheStatus(next))
	}
}

func TestBackgroundValidation(t *testing.T) {
	const swr = "max-age=60, stale-while-revalidate=300"
	tests := []struct {
		name       string
		origin     func(o *origin)
		wantStored string // body of the entry afterwards; "": deleted
		wantFresh  bool
		wantSets   int
	}{
		{name: "§4.3.3 304 refreshes the entry", wantStored: "old", wantFresh: true, wantSets: 2},
		{name: "§4.3.3 a whole response replaces it", origin: func(o *origin) { o.etag = `"v2"` }, wantStored: "hello", wantFresh: true, wantSets: 2},
		{name: "§4.3.4 304 about another response deletes it", origin: func(o *origin) { o.etagOf304 = `"v2"` }, wantSets: 1},
		{name: "§4.3.4 304 without validator leaves it as it is", origin: func(o *origin) { o.bare304 = true }, wantStored: "old", wantSets: 1},
		{name: "§5.2.2.5 304 with no-store deletes it", origin: func(o *origin) { o.cacheControl = "no-store" }, wantSets: 1},
		{name: "RFC 5861 §4 backend error keeps it", origin: func(o *origin) { o.status = 503; o.etag = `"v2"` }, wantStored: "old", wantSets: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			if tt.origin != nil {
				tt.origin(o)
			}
			c, st := newTestCache(t, o, nil)
			storeValidated(t, st, swr, 70*time.Second)

			rec := doRequest(c, http.MethodGet, staleURL, http.Header{"If-None-Match": {`"v0"`}})
			if rec.Code != 200 || rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
				t.Fatalf("the stale response must be served: got %d %q %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
			}
			waitRevalidations(t, c)
			if o.calls != 1 || o.lastReq.Header.Get("If-None-Match") != `"v1"` || o.lastReq.Header.Get("If-Modified-Since") != lastModified {
				t.Fatalf("§4.3.1 the revalidation must send the validators of the entry (calls %d): %v", o.calls, o.lastReq.Header)
			}
			e := storedAt(t, c, st, staleURL, "")
			body := ""
			if e != nil {
				body = string(e.body)
			}
			if body != tt.wantStored || st.sets != tt.wantSets {
				t.Fatalf("stored body = %q, sets = %d; want %q, %d", body, st.sets, tt.wantStored, tt.wantSets)
			}
			if e != nil && (time.Since(e.responseTime) < time.Minute) != tt.wantFresh {
				t.Errorf("received %v, want fresh = %v", e.responseTime, tt.wantFresh)
			}
		})
	}
}

// The 200 response to a forwarded HEAD updates or invalidates the stored GET
// response it could have been answered with (§4.3.5).
func TestHeadFreshensStoredResponse(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string // of the stored response, 2 minutes old
		reqHdr       http.Header
		origin       func(o *origin)
		wantStored   bool
		wantFresh    bool // stored again, with the fields of the HEAD response
		wantSets     int
		wantDels     int
	}{
		{name: "same response, stale entry: refreshed", cacheControl: "max-age=60", wantStored: true, wantFresh: true, wantSets: 2},
		{name: "same response, fresh entry refused by the request: refreshed", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, wantStored: true, wantFresh: true, wantSets: 2},
		{name: "same response, request no-cache: refreshed", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"no-cache"}}, wantStored: true, wantFresh: true, wantSets: 2},
		{name: "another entity tag, fresh entry: invalidated", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, origin: func(o *origin) { o.etag = `"v2"` }, wantSets: 1, wantDels: 1},
		{name: "another entity tag, request no-cache: invalidated", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"no-cache"}}, origin: func(o *origin) { o.etag = `"v2"` }, wantSets: 1, wantDels: 1},
		{name: "another length: invalidated", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, origin: func(o *origin) { o.body = "longer" }, wantSets: 1, wantDels: 1},
		{name: "no validator: nothing is known of the stored response", cacheControl: "max-age=60", origin: func(o *origin) { o.etag = ""; o.lastModified = "" }, wantStored: true, wantSets: 1},
		{name: "no validator but another length: invalidated", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, origin: func(o *origin) { o.etag = ""; o.lastModified = ""; o.body = "longer" }, wantSets: 1, wantDels: 1},
		{name: "another entity tag, stale entry: it is stale already", cacheControl: "max-age=60", origin: func(o *origin) { o.etag = `"v2"` }, wantStored: true, wantSets: 1},
		{name: "another content coding is another variant", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, origin: func(o *origin) { o.etag = `"v2"`; o.extra = http.Header{"Content-Encoding": {"gzip"}} }, wantStored: true, wantSets: 1},
		{name: "not a 200", cacheControl: "max-age=3600", reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, origin: func(o *origin) { o.status = 404; o.etag = `"v2"` }, wantStored: true, wantSets: 1},
		{name: "same response that may no longer be stored: deleted", cacheControl: "max-age=60", origin: func(o *origin) { o.cacheControl = "no-store" }, wantSets: 1, wantDels: 1},
		{name: "§5.2.1.5 request no-store: not stored again", cacheControl: "max-age=60", reqHdr: http.Header{"Cache-Control": {"no-store"}}, wantStored: true, wantSets: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			o.body = "old" // the length of the stored body
			o.cacheControl = "max-age=600"
			if tt.origin != nil {
				tt.origin(o)
			}
			c, st := newTestCache(t, o, nil)
			storeValidated(t, st, tt.cacheControl, 2*time.Minute)

			rec := doRequest(c, http.MethodHead, staleURL, tt.reqHdr)
			if o.calls != 1 || o.lastReq.Method != http.MethodHead || o.lastReq.Header.Get("If-None-Match") != "" {
				t.Fatalf("the HEAD must be forwarded as it is (backend calls %d): %v", o.calls, o.lastReq.Header)
			}
			if rec.Body.Len() != 0 || !strings.Contains(lastCacheStatus(rec), "; fwd-status=") {
				t.Errorf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
			}
			e := storedAt(t, c, st, staleURL, "")
			if (e != nil) != tt.wantStored || st.sets != tt.wantSets || st.dels != tt.wantDels {
				t.Fatalf("stored = %v, sets = %d, dels = %d; want %v, %d, %d", e != nil, st.sets, st.dels, tt.wantStored, tt.wantSets, tt.wantDels)
			}
			if e == nil {
				return
			}
			if string(e.body) != "old" || e.status != 200 {
				t.Errorf("the stored response keeps its body: %d %q", e.status, e.body)
			}
			fresh := time.Since(e.responseTime) < time.Minute && e.header.Get("Cache-Control") == "max-age=600"
			if fresh != tt.wantFresh {
				t.Errorf("refreshed = %v, want %v: received %v, %v", fresh, tt.wantFresh, e.responseTime, e.header)
			}
			if tt.wantFresh {
				next := doRequest(c, http.MethodGet, staleURL, nil)
				if !isHit(lastCacheStatus(next)) || next.Body.String() != "old" || next.Header().Get("Content-Length") != "3" || o.calls != 1 {
					t.Errorf("the entry must be fresh again: %q %q", lastCacheStatus(next), next.Body.String())
				}
			}
		})
	}
}

func TestHeadWithoutStoredResponse(t *testing.T) {
	o := newOrigin()
	c, st := newTestCache(t, o, nil)
	for _, hdr := range []http.Header{nil, {"Cache-Control": {"no-cache"}}} {
		doRequest(c, http.MethodHead, testURL, hdr)
	}
	if st.sets != 0 || st.dels != 0 {
		t.Errorf("a HEAD stores nothing (sets %d, dels %d)", st.sets, st.dels)
	}
}

// A stored response is validated without its body, which is only read when the
// client gets it: not when it gets a 304 in turn.
func TestValidationReadsTheBodyWhenSent(t *testing.T) {
	entryOnly := []string{""}
	withBody := []string{"", bodyField("")}
	tests := []struct {
		name            string
		ifNoneMatch     string
		gone            bool // the body is no longer in the store
		wantCode        int
		wantBody        string
		wantReads       [][]string
		wantValidations int
		wantCalls       int
	}{
		{name: "§4.3.2 the client has it: 304, the body is not read", ifNoneMatch: `"v1"`, wantCode: 304, wantReads: [][]string{entryOnly}, wantValidations: 1, wantCalls: 1},
		{name: "§4.3.3 the client has another one: the body is read once validated", ifNoneMatch: `"v0"`, wantCode: 200, wantBody: "old", wantReads: [][]string{entryOnly, withBody}, wantValidations: 1, wantCalls: 1},
		{name: "validated but its body is gone: the backend is asked for a whole response", ifNoneMatch: `"v0"`, gone: true, wantCode: 200, wantBody: "hello", wantReads: [][]string{entryOnly, withBody}, wantValidations: 1, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			c, st := newTestCache(t, o, nil)
			storeValidated(t, st, "max-age=60", 2*time.Minute)
			key := cacheKey(staleURL)
			if tt.gone {
				if err := st.del(context.Background(), key, []string{bodyField("")}); err != nil {
					t.Fatal(err)
				}
			}

			rec := doRequest(c, http.MethodGet, staleURL, http.Header{"If-None-Match": {tt.ifNoneMatch}})
			if rec.Code != tt.wantCode || rec.Body.String() != tt.wantBody {
				t.Errorf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if o.validations != tt.wantValidations || o.calls != tt.wantCalls {
				t.Errorf("validations = %d, backend calls = %d; want %d, %d", o.validations, o.calls, tt.wantValidations, tt.wantCalls)
			}
			if !reflect.DeepEqual(st.reads, tt.wantReads) {
				t.Errorf("fields read = %q, want %q", st.reads, tt.wantReads)
			}
			// Either way, a whole response is stored in the end.
			want := "old"
			if tt.gone {
				want = "hello"
			}
			if rec := doRequest(c, http.MethodGet, staleURL, nil); !isHit(lastCacheStatus(rec)) || rec.Body.String() != want {
				t.Errorf("then: got %q %q, want a hit with %q", lastCacheStatus(rec), rec.Body.String(), want)
			}
		})
	}
}
