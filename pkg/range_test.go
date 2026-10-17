package traedis

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

const rangeModified = "Sat, 03 Oct 2026 12:00:00 GMT"

func rangeBackend() *backend {
	return &backend{
		header: http.Header{
			"Cache-Control": {"max-age=60"}, "Content-Type": {"text/plain"},
			"Etag": {`"v1"`}, "Last-Modified": {rangeModified},
		},
		body: "0123456789",
	}
}

// RFC 9110 §14: a request for some bytes of a stored response gets those.
func TestRangeFromStoredResponse(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		header   http.Header // of the request
		response http.Header // added to the one of the backend
		wantCode int
		wantBody string
		// wantRange is the Content-Range of the answer.
		wantRange string
	}{
		{
			name:      "§14.1.2 first bytes",
			header:    http.Header{"Range": {"bytes=0-3"}},
			wantCode:  206,
			wantBody:  "0123",
			wantRange: "bytes 0-3/10",
		},
		{
			name:      "§14.1.2 from an offset",
			header:    http.Header{"Range": {"bytes=7-"}},
			wantCode:  206,
			wantBody:  "789",
			wantRange: "bytes 7-9/10",
		},
		{name: "§14.1.2 last bytes", header: http.Header{"Range": {"bytes=-2"}}, wantCode: 206, wantBody: "89", wantRange: "bytes 8-9/10"},
		{
			name:      "§14.1.2 past the end",
			header:    http.Header{"Range": {"bytes=8-100"}},
			wantCode:  206,
			wantBody:  "89",
			wantRange: "bytes 8-9/10",
		},
		{name: "§15.5.17 no such bytes", header: http.Header{"Range": {"bytes=10-20"}}, wantCode: 416, wantRange: "bytes */10"},
		{name: "§14.2 another range unit is ignored", header: http.Header{"Range": {"lines=1-2"}}, wantCode: 200, wantBody: "0123456789"},
		{name: "§14.2 only for a GET", method: http.MethodHead, header: http.Header{"Range": {"bytes=0-3"}}, wantCode: 200},
		{
			name:     "§14.3 not when the backend refuses ranges",
			header:   http.Header{"Range": {"bytes=0-3"}},
			response: http.Header{"Accept-Ranges": {"none"}},
			wantCode: 200, wantBody: "0123456789",
		},
		{
			name:     "§13.1.5 If-Range with the stored entity tag",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-Range": {`"v1"`}},
			wantCode: 206, wantBody: "0123", wantRange: "bytes 0-3/10",
		},
		{
			name:     "§13.1.5 If-Range with another entity tag: the whole response",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-Range": {`"v0"`}},
			wantCode: 200, wantBody: "0123456789",
		},
		{
			name:     "§13.1.5 If-Range with a weak entity tag: the whole response",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-Range": {`W/"v1"`}},
			wantCode: 200, wantBody: "0123456789",
		},
		{
			name:     "§13.1.5 If-Range with the stored date",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-Range": {rangeModified}},
			wantCode: 206, wantBody: "0123", wantRange: "bytes 0-3/10",
		},
		{
			name:     "§13.1.5 If-Range with another date: the whole response",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-Range": {"Fri, 02 Oct 2026 12:00:00 GMT"}},
			wantCode: 200, wantBody: "0123456789",
		},
		{
			name:     "§13.2.2 If-None-Match comes first",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-None-Match": {`"v1"`}},
			wantCode: 304,
		},
		{
			name:     "§13.2.2 If-None-Match that does not match, then Range",
			header:   http.Header{"Range": {"bytes=0-3"}, "If-None-Match": {`"v0"`}},
			wantCode: 206, wantBody: "0123", wantRange: "bytes 0-3/10",
		},
		{
			name:     "the bytes of a compressed response are those of its coding",
			header:   http.Header{"Range": {"bytes=0-3"}, "Accept-Encoding": {"gzip"}},
			response: http.Header{"Content-Encoding": {"gzip"}, "Vary": {"Accept-Encoding"}},
			wantCode: 206, wantBody: "0123", wantRange: "bytes 0-3/10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := rangeBackend()
			for name, values := range tt.response {
				b.header[name] = values
			}
			c, _ := newTestCache(t, b, nil)
			// Stored by a request for all of it.
			doRequest(c, http.MethodGet, testURL, http.Header{"Accept-Encoding": tt.header["Accept-Encoding"]})

			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := doRequest(c, method, testURL, tt.header)
			if b.calls != 1 || !isHit(lastCacheStatus(rec)) {
				t.Fatalf("Cache-Status = %q, backend calls = %d, want a hit", lastCacheStatus(rec), b.calls)
			}
			if rec.Code != tt.wantCode || rec.Header().Get("Content-Range") != tt.wantRange {
				t.Errorf(
					"got %d with Content-Range %q, want %d with %q", rec.Code, rec.Header().Get("Content-Range"), tt.wantCode, tt.wantRange,
				)
			}
			if tt.wantCode == http.StatusRequestedRangeNotSatisfiable {
				return
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantCode == http.StatusPartialContent && rec.Header().Get("Content-Length") != "4" && len(tt.wantBody) == 4 {
				t.Errorf("Content-Length = %q", rec.Header().Get("Content-Length"))
			}
			if rec.Header().Get("Etag") != `"v1"` || rec.Header().Get("Age") == "" {
				t.Errorf("header = %v, want the one of the stored response", rec.Header())
			}
		})
	}
}

// §14.6: several ranges come as the parts of a multipart/byteranges body.
func TestRangeSeveralParts(t *testing.T) {
	// Since Go 1.26.9, http.ServeContent ignores a Range header with more ranges than this, which is 1 for a module older than go 1.26 like
	// this one. The test takes the default of a recent one (in Traefik, it is up to its build). An older Go ignores the setting.
	t.Setenv("GODEBUG", strings.TrimPrefix(os.Getenv("GODEBUG")+",httpservecontentmaxranges=200", ","))
	b := rangeBackend()
	c, _ := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"Range": {"bytes=0-1,8-9"}})
	if rec.Code != http.StatusPartialContent || !strings.HasPrefix(rec.Header().Get("Content-Type"), "multipart/byteranges; boundary=") {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, part := range []string{
		"Content-Range: bytes 0-1/10\r\nContent-Type: text/plain\r\n\r\n01\r\n",
		"Content-Range: bytes 8-9/10\r\nContent-Type: text/plain\r\n\r\n89\r\n",
	} {
		if !strings.Contains(body, part) {
			t.Errorf("body = %q, want it to contain %q", body, part)
		}
	}
}

// §14.2: past maxRanges, a request gets the whole response, not one part per range, which would make the answer many times larger than the
// request.
func TestTooManyRanges(t *testing.T) {
	t.Setenv("GODEBUG", strings.TrimPrefix(os.Getenv("GODEBUG")+",httpservecontentmaxranges=200", ",")) // see TestRangeSeveralParts
	body := strings.Repeat("0123456789", 4)
	ranges := func(n int) string {
		var r []string
		for i := 0; i < n; i++ {
			r = append(r, strconv.Itoa(2*i)+"-"+strconv.Itoa(2*i))
		}
		return "bytes=" + strings.Join(r, ",")
	}
	b := rangeBackend()
	b.body = body
	c, _ := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)

	rec := doRequest(c, http.MethodGet, testURL, http.Header{"Range": {ranges(maxRanges)}})
	if rec.Code != http.StatusPartialContent || strings.Count(rec.Body.String(), "Content-Range: ") != maxRanges {
		t.Errorf(
			"%d ranges: got %d with %d parts, want a 206 with one part each",
			maxRanges, rec.Code, strings.Count(rec.Body.String(), "Content-Range: "),
		)
	}
	for _, r := range []string{ranges(maxRanges + 1), ranges(maxRanges) + ",,"} {
		rec = doRequest(c, http.MethodGet, testURL, http.Header{"Range": {r}})
		if rec.Code != http.StatusOK || rec.Body.String() != body || rec.Header().Get("Content-Range") != "" ||
			!isHit(lastCacheStatus(rec)) {
			t.Errorf("Range: %s: got %d %q %v, want the whole stored response", r, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if b.calls != 1 {
		t.Errorf("backend calls = %d: answered from the cache", b.calls)
	}
}

// §3.3: the cache does not store partial responses. Without a stored one, the request is for the backend, whose 206 is not stored.
func TestRangeWithoutStoredResponse(t *testing.T) {
	b := rangeBackend()
	b.status = http.StatusPartialContent
	b.header["Content-Range"] = []string{"bytes 0-3/10"}
	b.body = "0123"
	c, st := newTestCache(t, b, nil)
	for i := 1; i <= 2; i++ {
		rec := doRequest(c, http.MethodGet, testURL, http.Header{"Range": {"bytes=0-3"}})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123" || b.calls != i {
			t.Errorf("request %d: got %d %q (backend calls %d)", i, rec.Code, rec.Body.String(), b.calls)
		}
		if b.lastReq.Header.Get("Range") != "bytes=0-3" || lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=206" {
			t.Errorf("request %d: Range = %q, Cache-Status = %q", i, b.lastReq.Header.Get("Range"), lastCacheStatus(rec))
		}
	}
	if st.sets != 0 {
		t.Errorf("sets = %d, want 0", st.sets)
	}
}

// Only a 200 has ranges: another stored response is served as it is.
func TestRangeOfAnotherStatus(t *testing.T) {
	b := rangeBackend()
	b.status = http.StatusNotFound
	c, _ := newTestCache(t, b, func(cfg *Config) { cfg.StatusCodes = []string{"200", "404"} })
	doRequest(c, http.MethodGet, testURL, nil)
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"Range": {"bytes=0-3"}})
	if rec.Code != http.StatusNotFound || rec.Body.String() != "0123456789" || !isHit(lastCacheStatus(rec)) {
		t.Errorf("got %d %q %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
	}
}
