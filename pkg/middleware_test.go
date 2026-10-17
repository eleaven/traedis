package traedis

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// backend is a scripted origin that records what it receives.
type backend struct {
	status  int
	header  http.Header
	body    string
	calls   int
	lastReq *http.Request
	writer  http.ResponseWriter
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.calls++
	b.lastReq = r
	b.writer = w
	for k, v := range b.header {
		w.Header()[k] = append([]string(nil), v...)
	}
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	_, _ = io.WriteString(w, b.body)
}

func newTestCache(t *testing.T, b http.Handler, mutate func(*Config)) (*cache, *memStore) {
	t.Helper()
	cfg := CreateConfig()
	if mutate != nil {
		mutate(cfg)
	}
	s, err := parseConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	return newCache(b, "cache", s, st), st
}

// honourClientCacheControl makes newTestCache act on the Cache-Control of
// requests, for the tests of request directives (§5.2.1).
func honourClientCacheControl(cfg *Config) {
	cfg.IgnoreClientCacheControl = false
}

// cacheKey returns the Redis key of a URI for the cache of newTestCache.
func cacheKey(uri string) string {
	return redisKey("cache", uri)
}

func doRequest(h http.Handler, method, target string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c, ok := h.(*cache); ok {
		settle(c)
	}
	return rec
}

// settle waits for what the responses served by c had to store (see later).
func settle(c *cache) {
	for {
		var pending *pendingWrite
		c.writeMu.Lock()
		for _, p := range c.writing {
			pending = p
		}
		c.writeMu.Unlock()
		if pending == nil {
			return
		}
		<-pending.done
	}
}

func cacheableBackend() *backend {
	return &backend{
		header: http.Header{"Cache-Control": {"public, max-age=60"}, "Content-Type": {"text/plain"}},
		body:   "hello",
	}
}

// closeTo allows for the backend response delay, counted in the initial age (§4.2.3).
func closeTo(got, want time.Duration) bool {
	return got <= want && got > want-time.Second
}

const testURL = "http://example.com/600x400?b=2&a=1"

func lastCacheStatus(rec *httptest.ResponseRecorder) string {
	values := rec.Header().Values("Cache-Status")
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func TestMissThenHit(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)

	first := doRequest(c, http.MethodGet, testURL, nil)
	if first.Code != 200 || first.Body.String() != "hello" {
		t.Fatalf("miss: got %d %q", first.Code, first.Body.String())
	}
	if got := lastCacheStatus(first); got != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("miss Cache-Status = %q", got)
	}
	v, ok := st.lookup(cacheKey(testURL), "")
	if !ok {
		t.Fatal("response not stored")
	}
	if want := time.Minute + time.Hour; !closeTo(v.ttl, want) {
		t.Errorf("stored ttl = %v, want %v", v.ttl, want)
	}

	// Same resource: the host is case-insensitive.
	second := doRequest(c, http.MethodGet, "http://EXAMPLE.com/600x400?b=2&a=1", nil)
	if b.calls != 1 {
		t.Errorf("backend calls = %d, want 1", b.calls)
	}
	if second.Code != 200 || second.Body.String() != "hello" || second.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("hit: got %d %q %v", second.Code, second.Body.String(), second.Header())
	}
	if got := lastCacheStatus(second); !strings.HasPrefix(got, "traedis; hit; ttl=") {
		t.Errorf("hit Cache-Status = %q", got)
	}
	if second.Header().Get("Age") != "0" || second.Header().Get("Content-Length") != "5" {
		t.Errorf("Age = %q, Content-Length = %q", second.Header().Get("Age"), second.Header().Get("Content-Length"))
	}
	if len(second.Header().Values("Cache-Status")) != 1 {
		t.Errorf("our miss Cache-Status must not be stored: %q", second.Header().Values("Cache-Status"))
	}
}

func storeEntry(t *testing.T, st *memStore, uri string, e *entry) {
	t.Helper()
	if err := st.put(cacheKey(uri), "", e, nil, time.Hour); err != nil {
		t.Fatal(err)
	}
}

// storeReceived stores at uri a 200 response with header and the body "old",
// received at the given time.
func storeReceived(t *testing.T, st *memStore, uri string, header http.Header, at time.Time) {
	t.Helper()
	storeEntry(t, st, uri, &entry{status: 200, header: header, body: []byte("old"), requestTime: at, responseTime: at})
}

// §4: URIs that differ by the order of their query parameters are not the same,
// unless sortQuery says so.
func TestSortQuery(t *testing.T) {
	const reordered = "http://example.com/600x400?a=1&b=2"
	tests := []struct {
		name      string
		sortQuery bool
		wantHit   bool
	}{
		{name: "§4 another parameter order is another URI"},
		{name: "sortQuery: the parameter order is ignored", sortQuery: true, wantHit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cacheableBackend()
			c, _ := newTestCache(t, b, func(cfg *Config) { cfg.SortQuery = tt.sortQuery })
			doRequest(c, http.MethodGet, testURL, nil)
			if rec := doRequest(c, http.MethodGet, reordered, nil); isHit(lastCacheStatus(rec)) != tt.wantHit {
				t.Errorf("Cache-Status = %q, want hit = %v", lastCacheStatus(rec), tt.wantHit)
			}
			// §4.4: an unsafe request invalidates what its own URI names.
			doRequest(c, http.MethodPost, reordered, nil)
			if rec := doRequest(c, http.MethodGet, testURL, nil); isHit(lastCacheStatus(rec)) == tt.wantHit {
				t.Errorf("after a POST: Cache-Status = %q, want invalidated = %v", lastCacheStatus(rec), tt.wantHit)
			}
		})
	}
}

func TestHitAgeAndTTL(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	stored := time.Now().Add(-10 * time.Second)
	storeEntry(t, st, "http://example.com/a", &entry{
		status:       200,
		header:       http.Header{"Cache-Control": {"max-age=60"}, "Date": {stored.UTC().Format(http.TimeFormat)}},
		body:         []byte("cached"),
		requestTime:  stored,
		responseTime: stored,
	})

	rec := doRequest(c, http.MethodGet, "http://example.com/a", nil)
	if b.calls != 0 || rec.Body.String() != "cached" {
		t.Fatalf("expected a hit, got %q (backend calls %d)", rec.Body.String(), b.calls)
	}
	if age := rec.Header().Get("Age"); age != "10" && age != "11" {
		t.Errorf("§4.2.3 Age = %q, want 10", age)
	}
	if got := lastCacheStatus(rec); got != "traedis; hit; ttl=50" && got != "traedis; hit; ttl=49" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestHeadIsServedFromStoredGet(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)

	rec := doRequest(c, http.MethodHead, testURL, nil)
	if b.calls != 1 {
		t.Errorf("backend calls = %d, want 1", b.calls)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "5" {
		t.Errorf("HEAD: body %q, Content-Length %q", rec.Body.String(), rec.Header().Get("Content-Length"))
	}
	if !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
		t.Errorf("Cache-Status = %q", lastCacheStatus(rec))
	}
}

func TestHeadMissIsNotStored(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	rec := doRequest(c, http.MethodHead, testURL, nil)
	if b.calls != 1 || b.lastReq.Method != http.MethodHead {
		t.Errorf("backend must receive the HEAD request")
	}
	if st.sets != 0 {
		t.Error("a HEAD response must not be stored")
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestRequestDirectives(t *testing.T) {
	tests := []struct {
		name       string
		header     http.Header
		wantCalls  int
		wantCode   int
		wantStatus string
	}{
		{name: "§5.2.1.4 no-cache forwards and refreshes", header: http.Header{"Cache-Control": {"no-cache"}}, wantCalls: 2, wantCode: 200, wantStatus: "traedis; fwd=request; fwd-status=200"},
		{name: "§5.2.1.1 max-age=0 forwards", header: http.Header{"Cache-Control": {"max-age=0"}}, wantCalls: 2, wantCode: 200, wantStatus: "traedis; fwd=request; fwd-status=200"},
		{name: "§5.2.1.3 min-fresh too large forwards", header: http.Header{"Cache-Control": {"min-fresh=3600"}}, wantCalls: 2, wantCode: 200, wantStatus: "traedis; fwd=request; fwd-status=200"},
		{name: "§5.2.1.5 no-store may be served", header: http.Header{"Cache-Control": {"no-store"}}, wantCalls: 1, wantCode: 200, wantStatus: "hit"},
		{name: "§5.2.1.7 only-if-cached served from cache", header: http.Header{"Cache-Control": {"only-if-cached"}}, wantCalls: 1, wantCode: 200, wantStatus: "hit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cacheableBackend()
			c, _ := newTestCache(t, b, honourClientCacheControl)
			doRequest(c, http.MethodGet, testURL, nil)
			rec := doRequest(c, http.MethodGet, testURL, tt.header)
			if b.calls != tt.wantCalls || rec.Code != tt.wantCode {
				t.Errorf("backend calls = %d, code = %d; want %d, %d", b.calls, rec.Code, tt.wantCalls, tt.wantCode)
			}
			if got := lastCacheStatus(rec); !strings.Contains(got, tt.wantStatus) {
				t.Errorf("Cache-Status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

// By default, the Cache-Control of requests is ignored (§5.2.1: request
// directives are advisory): a client can neither replace the stored response
// with the answer to its own request, nor read a stale one, nor tell whether
// a URL is stored without storing it.
func TestClientCacheControlIgnored(t *testing.T) {
	for _, directive := range []string{"no-cache", "max-age=0", "min-fresh=3600", "only-if-cached", "no-store"} {
		t.Run(directive+" on a stored response", func(t *testing.T) {
			b := cacheableBackend()
			c, st := newTestCache(t, b, nil)
			doRequest(c, http.MethodGet, testURL, nil)
			rec := doRequest(c, http.MethodGet, testURL, http.Header{"Cache-Control": {directive}})
			if b.calls != 1 || rec.Code != 200 || !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") || st.sets != 1 {
				t.Errorf("backend calls = %d, sets = %d, got %d %q: the stored response must answer", b.calls, st.sets, rec.Code, lastCacheStatus(rec))
			}
		})
	}
	for _, directive := range []string{"only-if-cached", "no-store"} {
		t.Run(directive+" on a miss", func(t *testing.T) {
			b := cacheableBackend()
			c, st := newTestCache(t, b, nil)
			rec := doRequest(c, http.MethodGet, testURL, http.Header{"Cache-Control": {directive}})
			if b.calls != 1 || rec.Code != 200 || lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" || st.sets != 1 {
				t.Errorf("backend calls = %d, sets = %d, got %d %q: forwarded and stored", b.calls, st.sets, rec.Code, lastCacheStatus(rec))
			}
		})
	}
	t.Run("max-stale on a stale response", func(t *testing.T) {
		b := cacheableBackend()
		c, st := newTestCache(t, b, nil)
		storeAged(t, st, "max-age=60", 70*time.Second)
		rec := doRequest(c, http.MethodGet, staleURL, http.Header{"Cache-Control": {"max-stale"}})
		if b.calls != 1 || rec.Body.String() != "hello" || lastCacheStatus(rec) != "traedis; fwd=stale; fwd-status=200" {
			t.Errorf("backend calls = %d, got %q %q: a stale response is not served", b.calls, rec.Body.String(), lastCacheStatus(rec))
		}
	})
	t.Run("the request reaches the backend as it is", func(t *testing.T) {
		b := cacheableBackend()
		c, _ := newTestCache(t, b, nil)
		doRequest(c, http.MethodGet, testURL, http.Header{"Cache-Control": {"no-cache"}})
		if got := b.lastReq.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control sent = %q", got)
		}
	})
}

// authorizationHeaders keep the response to a client from the others, as
// Authorization does (§3.5): the backend has to allow it.
func TestAuthorizationHeaders(t *testing.T) {
	b := &backend{header: http.Header{"Content-Type": {"text/plain"}}, body: "hello alice"}
	c, st := newTestCache(t, b, func(cfg *Config) { cfg.AuthorizationHeaders = []string{"x-webauth-user"} })
	alice := http.Header{"X-Webauth-User": {"alice"}}
	doRequest(c, http.MethodGet, testURL, alice)
	b.body = "hello bob"
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"X-Webauth-User": {"bob"}})
	if st.sets != 0 || rec.Body.String() != "hello bob" {
		t.Errorf("sets = %d, bob got %q: defaultTtl must not apply", st.sets, rec.Body.String())
	}

	b.header.Set("Cache-Control", "public, max-age=60")
	doRequest(c, http.MethodGet, testURL, alice)
	if st.sets != 1 {
		t.Errorf("sets = %d: public allows sharing the response", st.sets)
	}
}

func TestMaxStale(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string // of the stored response
		age          time.Duration
		method       string
		reqCC        string
		mutate       func(*Config)
		wantStatus   string // prefix of Cache-Status
		wantCalls    int
	}{
		{name: "§5.2.1.2 max-stale: any staleness", cacheControl: "max-age=60", age: 30 * time.Minute, reqCC: "max-stale", wantStatus: "traedis; hit; ttl=-1740; detail=max-stale"},
		{name: "§5.2.1.2 max-stale=N: within N seconds", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "max-stale=30", wantStatus: "traedis; hit; ttl=-1"},
		{name: "§5.2.1.2 max-stale=N: staler than N seconds", cacheControl: "max-age=60", age: 100 * time.Second, reqCC: "max-stale=30", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§5.2.1.2 max-stale=0", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "max-stale=0", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§5.2.1.2 HEAD", cacheControl: "max-age=60", age: 70 * time.Second, method: http.MethodHead, reqCC: "max-stale", wantStatus: "traedis; hit; ttl=-1"},
		{name: "§5.2.1.2 a fresh response is a plain hit", cacheControl: "max-age=60", age: 10 * time.Second, reqCC: "max-stale", wantStatus: "traedis; hit; ttl=4"},
		{name: "capped at staleTtl", cacheControl: "max-age=60", age: 90 * time.Second, reqCC: "max-stale", mutate: func(c *Config) { c.StaleTTL = "10s" }, wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§4.2.4 must-revalidate forbids it", cacheControl: "max-age=60, must-revalidate", age: 70 * time.Second, reqCC: "max-stale", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§4.2.4 proxy-revalidate forbids it", cacheControl: "max-age=60, proxy-revalidate", age: 70 * time.Second, reqCC: "max-stale", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§4.2.4 s-maxage forbids it", cacheControl: "s-maxage=60", age: 70 * time.Second, reqCC: "max-stale", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§5.2.1.1 with max-age: age within both", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "max-age=100, max-stale=30", wantStatus: "traedis; hit; ttl=-1"},
		{name: "§5.2.1.1 with max-age: too old", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "max-age=65, max-stale=30", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§5.2.1.3 min-fresh asks for the opposite", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "min-fresh=0, max-stale", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
		{name: "§5.2.1.7 with only-if-cached", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "only-if-cached, max-stale", wantStatus: "traedis; hit; ttl=-1"},
		{name: "§5.2.1.4 no-cache wins", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "no-cache, max-stale", wantStatus: "traedis; fwd=request; fwd-status=200", wantCalls: 1},
		{name: "malformed max-stale is ignored", cacheControl: "max-age=60", age: 70 * time.Second, reqCC: "max-stale=soon", wantStatus: "traedis; fwd=stale; fwd-status=200", wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cacheableBackend()
			c, st := newTestCache(t, b, func(cfg *Config) {
				honourClientCacheControl(cfg)
				if tt.mutate != nil {
					tt.mutate(cfg)
				}
			})
			storeAged(t, st, tt.cacheControl, tt.age)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := doRequest(c, method, staleURL, http.Header{"Cache-Control": {tt.reqCC}})

			got := lastCacheStatus(rec)
			if !strings.HasPrefix(got, tt.wantStatus) {
				t.Errorf("Cache-Status = %q, want %q…", got, tt.wantStatus)
			}
			if b.calls != tt.wantCalls || len(c.revalidating) != 0 {
				t.Errorf("backend calls = %d, want %d; revalidations = %d, want none", b.calls, tt.wantCalls, len(c.revalidating))
			}
			if tt.wantCalls > 0 {
				return
			}
			wantBody := "old"
			if method == http.MethodHead {
				wantBody = ""
			}
			if rec.Code != 200 || rec.Body.String() != wantBody {
				t.Errorf("the stored response must be served: got %d %q", rec.Code, rec.Body.String())
			}
			if age := rec.Header().Get("Age"); age != strconv.Itoa(int(tt.age/time.Second)) && age != strconv.Itoa(int(tt.age/time.Second)+1) {
				t.Errorf("§4.2.3 Age = %q, want %d", age, tt.age/time.Second)
			}
			if st.sets != 1 {
				t.Error("the entry must be left as it was")
			}
			stale := strings.Contains(got, "ttl=-")
			if stale != strings.HasSuffix(got, "; detail=max-stale") {
				t.Errorf("Cache-Status = %q: detail=max-stale is for a stale response", got)
			}
		})
	}
}

// stale-while-revalidate serves the same response and refreshes it.
func TestMaxStaleWithStaleWhileRevalidate(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)
	rec := doRequest(c, http.MethodGet, staleURL, http.Header{"Cache-Control": {"max-stale"}})
	if rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Errorf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	waitRevalidations(t, c)
	if b.calls != 1 {
		t.Errorf("backend calls = %d, want the background revalidation", b.calls)
	}
}

func TestOnlyIfCachedMissIs504(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, honourClientCacheControl)
	rec := doRequest(c, http.MethodGet, testURL, http.Header{"Cache-Control": {"only-if-cached"}})
	if rec.Code != http.StatusGatewayTimeout || b.calls != 0 {
		t.Errorf("§5.2.1.7 got %d with %d backend calls, want 504 and none", rec.Code, b.calls)
	}
	// RFC 9211 §2: not for a response the cache made up, without forwarding.
	if got := rec.Header().Values("Cache-Status"); len(got) != 0 {
		t.Errorf("Cache-Status = %q, want none", got)
	}
}

// RFC 9110 §6.6.1: a response received without Date is sent with the time it
// was received, not the time it is served from the cache.
func TestStoredResponseWithoutDateGetsOne(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	received := time.Now().Add(-10 * time.Second)
	storeEntry(t, st, "http://example.com/a", &entry{
		status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Etag": {`"v1"`}}, body: []byte("cached"),
		requestTime: received, responseTime: received,
	})
	want := received.UTC().Format(http.TimeFormat)
	rec := doRequest(c, http.MethodGet, "http://example.com/a", nil)
	if !isHit(lastCacheStatus(rec)) || rec.Header().Get("Date") != want || rec.Header().Get("Age") != "10" {
		t.Errorf("hit: Date = %q, want %q (Age %q, Cache-Status %q)", rec.Header().Get("Date"), want, rec.Header().Get("Age"), lastCacheStatus(rec))
	}
	rec = doRequest(c, http.MethodGet, "http://example.com/a", http.Header{"If-None-Match": {`"v1"`}})
	if rec.Code != http.StatusNotModified || rec.Header().Get("Date") != want {
		t.Errorf("304: got %d, Date = %q, want %q", rec.Code, rec.Header().Get("Date"), want)
	}

	// The Date of the backend is left alone.
	date := received.Add(-5 * time.Second).UTC().Format(http.TimeFormat)
	storeEntry(t, st, "http://example.com/b", &entry{
		status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Date": {date}}, body: []byte("cached"),
		requestTime: received, responseTime: received,
	})
	if rec := doRequest(c, http.MethodGet, "http://example.com/b", nil); rec.Header().Get("Date") != date {
		t.Errorf("Date = %q, want the one of the backend %q", rec.Header().Get("Date"), date)
	}
}

func TestStaleEntryIsRefreshed(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	old := time.Now().Add(-2 * time.Minute)
	storeEntry(t, st, "http://example.com/a", &entry{
		status: 200, header: http.Header{"Cache-Control": {"max-age=60"}}, body: []byte("old"),
		requestTime: old, responseTime: old,
	})
	rec := doRequest(c, http.MethodGet, "http://example.com/a", nil)
	if b.calls != 1 || rec.Body.String() != "hello" {
		t.Errorf("§4.2.4 a stale entry must not be served without a directive allowing it")
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=stale; fwd-status=200" {
		t.Errorf("Cache-Status = %q", got)
	}
	if st.sets != 2 {
		t.Errorf("the entry must be overwritten")
	}
}

func TestStaleIfError(t *testing.T) {
	const sie = "max-age=60, stale-if-error=30"
	tests := []struct {
		name         string
		cacheControl string
		age          time.Duration
		method       string
		reqHdr       http.Header
		status       int
		mutate       func(*Config)
		wantStatus   string // Cache-Status of a stale response; "" when the error must go through
		wantFwd      string // Cache-Status when the error goes through
	}{
		{name: "RFC 5861 §4 500", cacheControl: sie, age: 70 * time.Second, status: 500, wantStatus: "traedis; fwd=stale; fwd-status=500; ttl=-1"},
		{name: "RFC 5861 §4 502", cacheControl: sie, age: 70 * time.Second, status: 502, wantStatus: "traedis; fwd=stale; fwd-status=502; ttl=-1"},
		{name: "RFC 5861 §4 503", cacheControl: sie, age: 70 * time.Second, status: 503, wantStatus: "traedis; fwd=stale; fwd-status=503; ttl=-1"},
		{name: "RFC 5861 §4 504", cacheControl: sie, age: 70 * time.Second, status: 504, wantStatus: "traedis; fwd=stale; fwd-status=504; ttl=-1"},
		{name: "RFC 5861 §4 HEAD", cacheControl: sie, age: 70 * time.Second, method: http.MethodHead, status: 503, wantStatus: "traedis; fwd=stale; fwd-status=503; ttl=-1"},
		{name: "RFC 5861 §4 regardless of the request max-age, on a fresh entry", cacheControl: sie, age: 15 * time.Second, reqHdr: http.Header{"Cache-Control": {"max-age=0"}}, status: 503, wantStatus: "traedis; fwd=request; fwd-status=503; ttl=4"},
		{name: "RFC 5861 §4 regardless of the request min-fresh", cacheControl: sie, age: 70 * time.Second, reqHdr: http.Header{"Cache-Control": {"min-fresh=10"}}, status: 503, wantStatus: "traedis; fwd=stale; fwd-status=503; ttl=-1"},
		{name: "RFC 5861 §4 501 is not an error", cacheControl: sie, age: 70 * time.Second, status: 501, wantFwd: "traedis; fwd=stale; fwd-status=501"},
		{name: "RFC 5861 §4 404 is not an error", cacheControl: sie, age: 70 * time.Second, status: 404, wantFwd: "traedis; fwd=stale; fwd-status=404"},
		{name: "RFC 5861 §4 window over", cacheControl: sie, age: 91 * time.Second, status: 503, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "window capped at staleTtl", cacheControl: "max-age=60, stale-if-error=3600", age: 80 * time.Second, status: 503, mutate: func(c *Config) { c.StaleTTL = "10s" }, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "§4.2.4 no directive", cacheControl: "max-age=60", age: 70 * time.Second, status: 503, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "§5.2.2.2 must-revalidate", cacheControl: sie + ", must-revalidate", age: 70 * time.Second, status: 503, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "§5.2.2.10 s-maxage", cacheControl: "s-maxage=60, stale-if-error=30", age: 70 * time.Second, status: 503, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "§4.2.4 defaultStaleIfError when the directive is absent", cacheControl: "max-age=60", age: 70 * time.Second, status: 503, mutate: func(c *Config) { c.DefaultStaleIfError = "30s" }, wantStatus: "traedis; fwd=stale; fwd-status=503; ttl=-1"},
		{name: "defaultStaleIfError over", cacheControl: "max-age=60", age: 91 * time.Second, status: 503, mutate: func(c *Config) { c.DefaultStaleIfError = "30s" }, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "defaultStaleIfError does not replace the directive", cacheControl: "max-age=60, stale-if-error=0", age: 70 * time.Second, status: 503, mutate: func(c *Config) { c.DefaultStaleIfError = "30s" }, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "defaultStaleIfError with §5.2.2.10 s-maxage", cacheControl: "s-maxage=60", age: 70 * time.Second, status: 503, mutate: func(c *Config) { c.DefaultStaleIfError = "30s" }, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "defaultStaleWhileRevalidate is not defaultStaleIfError", cacheControl: "max-age=60", age: 91 * time.Second, status: 503, mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "30s" }, wantFwd: "traedis; fwd=stale; fwd-status=503"},
		{name: "§5.2.1.4 request no-cache does not look at the cache", cacheControl: sie, age: 70 * time.Second, reqHdr: http.Header{"Cache-Control": {"no-cache"}}, status: 503, wantFwd: "traedis; fwd=request; fwd-status=503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &backend{
				status: tt.status,
				header: http.Header{"Content-Type": {"text/html"}, "Retry-After": {"120"}, "Cache-Status": {"origin; fwd=miss"}},
				body:   "error",
			}
			c, st := newTestCache(t, b, func(cfg *Config) {
				honourClientCacheControl(cfg)
				if tt.mutate != nil {
					tt.mutate(cfg)
				}
			})
			at := time.Now().Add(-tt.age)
			storeEntry(t, st, "http://example.com/a", &entry{
				status: 200, header: http.Header{"Cache-Control": {tt.cacheControl}, "Content-Type": {"text/plain"}}, body: []byte("old"),
				requestTime: at, responseTime: at,
			})
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, "http://example.com/a", nil)
			for k, v := range tt.reqHdr {
				req.Header[k] = v
			}
			rec := httptest.NewRecorder()
			rec.Header().Set("X-Before", "kept") // set by a middleware before the cache
			c.ServeHTTP(rec, req)
			settle(c)

			if b.calls != 1 {
				t.Fatalf("backend calls = %d, want 1", b.calls)
			}
			if st.sets != 1 || st.dels != 0 {
				t.Errorf("the entry must be left as it was (sets %d, dels %d)", st.sets, st.dels)
			}
			if rec.Header().Get("X-Before") != "kept" {
				t.Error("headers set before the cache must be kept")
			}
			got := rec.Header().Values("Cache-Status")
			if tt.wantStatus == "" {
				if rec.Code != tt.status || rec.Body.String() != "error" || rec.Header().Get("Retry-After") != "120" {
					t.Errorf("the backend response must go through: got %d %q %v", rec.Code, rec.Body.String(), rec.Header())
				}
				if len(got) != 2 || got[0] != "origin; fwd=miss" || got[1] != tt.wantFwd {
					t.Errorf("Cache-Status = %q, want %q", got, tt.wantFwd)
				}
				return
			}

			wantBody := "old"
			if method == http.MethodHead {
				wantBody = ""
			}
			if rec.Code != 200 || rec.Body.String() != wantBody || rec.Header().Get("Content-Length") != "3" {
				t.Errorf("the stored response must be served: got %d %q", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != "text/plain" || rec.Header().Get("Retry-After") != "" {
				t.Errorf("nothing of the backend error must be sent: %v", rec.Header())
			}
			if len(got) != 1 || !strings.HasPrefix(got[0], tt.wantStatus) || !strings.HasSuffix(got[0], "; detail=stale-if-error") {
				t.Errorf("Cache-Status = %q, want %q…; detail=stale-if-error", got, tt.wantStatus)
			}
			if age := rec.Header().Get("Age"); age != strconv.Itoa(int(tt.age/time.Second)) && age != strconv.Itoa(int(tt.age/time.Second)+1) {
				t.Errorf("§4.2.3 Age = %q, want %d", age, tt.age/time.Second)
			}
		})
	}
}

func TestStaleIfErrorAfterStaleWhileRevalidate(t *testing.T) {
	b := &backend{status: 503, body: "error"}
	c, st := newTestCache(t, b, nil)
	// Past the stale-while-revalidate window, within the stale-if-error one.
	storeAged(t, st, "max-age=60, stale-while-revalidate=10, stale-if-error=300", 2*time.Minute)

	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if rec.Code != 200 || rec.Body.String() != "old" || b.calls != 1 {
		t.Errorf("got %d %q (backend calls %d)", rec.Code, rec.Body.String(), b.calls)
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=stale; fwd-status=503; ttl=-60; detail=stale-if-error" && got != "traedis; fwd=stale; fwd-status=503; ttl=-61; detail=stale-if-error" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestCorruptEntryIsAMiss(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	if err := st.putRaw(cacheKey("http://example.com/a"), "", []byte(entryVersion+"garbage")); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(c, http.MethodGet, "http://example.com/a", nil)
	if b.calls != 1 || rec.Body.String() != "hello" || lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("a corrupt entry must be a miss: %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
}

func TestFailOpenWhenStoreFails(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	st.err = errors.New("redis down")
	rec := doRequest(c, http.MethodGet, testURL, nil)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("fail open: got %d %q", rec.Code, rec.Body.String())
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=bypass; fwd-status=200; detail=redis" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestFailOpenWithUnreachableRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := CreateConfig()
	cfg.Redis.DSN = "redis://" + addr + "/0"
	b := cacheableBackend()
	h, err := New(context.Background(), b, cfg, "cache")
	if err != nil {
		t.Fatal(err)
	}
	rec := doRequest(h, http.MethodGet, testURL, nil)
	if rec.Code != 200 || rec.Body.String() != "hello" || b.calls != 1 {
		t.Errorf("fail open: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestNotStored(t *testing.T) {
	tests := []struct {
		name    string
		reqHdr  http.Header
		respHdr http.Header
		status  int
		body    string
		mutate  func(*Config)
	}{
		{name: "§5.2.2.5 no-store", respHdr: http.Header{"Cache-Control": {"no-store"}}},
		{name: "§5.2.2.7 private", respHdr: http.Header{"Cache-Control": {"private, max-age=60"}}},
		{name: "§3.5 Authorization without public", reqHdr: http.Header{"Authorization": {"Bearer x"}}, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "Set-Cookie", respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Set-Cookie": {"sid=1"}}},
		{name: "Set-Cookie, even with public", respHdr: http.Header{"Cache-Control": {"public, max-age=60"}, "Set-Cookie": {"sid=1"}}},
		{name: "Vary: Accept-Encoding with a coding the cache does not know", respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}, "Content-Encoding": {"compress"}}},
		{name: "statusCodes narrows", status: 404, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "defaultTtl not applied to requests with Cookie", reqHdr: http.Header{"Cookie": {"sid=1"}}, respHdr: http.Header{}},
		{name: "body larger than maxBodyBytes is streamed, not stored", respHdr: http.Header{"Cache-Control": {"max-age=60"}}, body: strings.Repeat("x", 100), mutate: func(c *Config) { c.MaxBodyBytes = 10 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if body == "" {
				body = "hello"
			}
			b := &backend{status: tt.status, header: tt.respHdr, body: body}
			c, st := newTestCache(t, b, tt.mutate)
			rec := doRequest(c, http.MethodGet, testURL, tt.reqHdr)
			if rec.Body.String() != body {
				t.Errorf("client got %d bytes, want %d", rec.Body.Len(), len(body))
			}
			if st.sets != 0 {
				t.Error("response must not be stored")
			}
		})
	}
}

func TestSetCookieIsNotReplayed(t *testing.T) {
	for _, cacheControl := range []string{`max-age=60, private="Set-Cookie, X-User"`, `max-age=60, no-cache="set-cookie", private="x-user"`} {
		t.Run(cacheControl, func(t *testing.T) {
			b := &backend{header: http.Header{"Cache-Control": {cacheControl}, "Set-Cookie": {"sid=1"}, "X-User": {"bob"}, "X-Other": {"kept"}}, body: "hello"}
			c, _ := newTestCache(t, b, nil)
			first := doRequest(c, http.MethodGet, testURL, nil)
			if first.Header().Get("Set-Cookie") != "sid=1" || first.Header().Get("X-User") != "bob" {
				t.Errorf("the client causing the miss must get the whole response: %v", first.Header())
			}
			second := doRequest(c, http.MethodGet, testURL, nil)
			if b.calls != 1 || !strings.HasPrefix(lastCacheStatus(second), "traedis; hit") || second.Body.String() != "hello" {
				t.Fatalf("§5.2.2.7 the rest of the response must be stored: %q (backend calls %d)", lastCacheStatus(second), b.calls)
			}
			h := second.Header()
			if h.Get("Set-Cookie") != "" || h.Get("X-User") != "" || h.Get("X-Other") != "kept" || h.Get("Cache-Control") != cacheControl {
				t.Errorf("§3.1 a hit must not have the fields that are not stored, and only those: %v", h)
			}
		})
	}
}

func TestDefaultTTLWithoutExplicitFreshness(t *testing.T) {
	b := &backend{body: "hello"}
	c, st := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	rec := doRequest(c, http.MethodGet, testURL, nil)
	if b.calls != 1 || !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit; ttl=") {
		t.Errorf("§4.2.2 expected a hit with defaultTtl, got %q", lastCacheStatus(rec))
	}
	if rec.Header().Get("Cache-Control") != "" {
		t.Error("Cache-Control must never be rewritten")
	}
	if v, _ := st.lookup(cacheKey(testURL), ""); !closeTo(v.ttl, 5*time.Minute+time.Hour) {
		t.Errorf("ttl = %v", v.ttl)
	}
}

func TestCacheStatusAppendedAfterUpstream(t *testing.T) {
	b := cacheableBackend()
	b.header["Cache-Status"] = []string{"origin; hit"}
	c, _ := newTestCache(t, b, nil)
	miss := doRequest(c, http.MethodGet, testURL, nil)
	hit := doRequest(c, http.MethodGet, testURL, nil)
	for _, rec := range []*httptest.ResponseRecorder{miss, hit} {
		values := rec.Header().Values("Cache-Status")
		if len(values) != 2 || values[0] != "origin; hit" || !strings.HasPrefix(values[1], "traedis;") {
			t.Errorf("RFC 9211 Cache-Status = %q", values)
		}
	}
}

func TestExposeKey(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, func(cfg *Config) { cfg.ExposeKey = true })
	rec := doRequest(c, http.MethodGet, testURL, nil)
	want := `key="` + cacheKey(testURL) + `"`
	if !strings.Contains(lastCacheStatus(rec), want) {
		t.Errorf("Cache-Status = %q, want it to contain %s", lastCacheStatus(rec), want)
	}

	c, _ = newTestCache(t, b, nil)
	if rec := doRequest(c, http.MethodGet, testURL, nil); strings.Contains(lastCacheStatus(rec), "key=") {
		t.Error("the key must not be exposed by default")
	}
}

// Two middlewares may be on the way to the same URL (another entry point, a
// rule on a header…): what one stores is not what the other one serves.
func TestMiddlewaresDoNotShareEntries(t *testing.T) {
	b := cacheableBackend()
	first, st := newTestCache(t, b, nil)
	doRequest(first, http.MethodGet, testURL, nil)

	other := newCache(b, "other", first.cfg, st)
	if rec := doRequest(other, http.MethodGet, testURL, nil); lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("another middleware: Cache-Status = %q", lastCacheStatus(rec))
	}
	// The same one in another Traefik instance.
	replica := newCache(b, first.name, first.cfg, st)
	if rec := doRequest(replica, http.MethodGet, testURL, nil); !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
		t.Errorf("the same middleware: Cache-Status = %q", lastCacheStatus(rec))
	}
	// An unsafe request only invalidates what its own middleware stored.
	doRequest(other, http.MethodPost, testURL, nil)
	if rec := doRequest(first, http.MethodGet, testURL, nil); !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
		t.Errorf("after the POST of another middleware: Cache-Status = %q", lastCacheStatus(rec))
	}
}

func TestPassThroughDoesNotWrapWriter(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		header     http.Header
		wantStatus string
	}{
		{name: "RFC 9110 §9.2.1 OPTIONS", method: http.MethodOptions, wantStatus: "traedis; fwd=method"},
		{name: "RFC 9110 §9.2.1 TRACE", method: http.MethodTrace, wantStatus: "traedis; fwd=method"},
		{name: "WebSocket upgrade", method: http.MethodGet, header: http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}}, wantStatus: "traedis; fwd=bypass"},
		{name: "server-sent events", method: http.MethodGet, header: http.Header{"Accept": {"text/event-stream"}}, wantStatus: "traedis; fwd=bypass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cacheableBackend()
			c, st := newTestCache(t, b, nil)
			rec := doRequest(c, tt.method, testURL, tt.header)
			if _, ok := b.writer.(*httptest.ResponseRecorder); !ok {
				t.Error("the backend must get the original writer (http.Flusher preserved)")
			}
			if st.sets != 0 || b.calls != 1 {
				t.Errorf("sets = %d, calls = %d", st.sets, b.calls)
			}
			if got := rec.Header().Values("Cache-Status"); len(got) != 1 || got[0] != tt.wantStatus {
				t.Errorf("Cache-Status = %q", got)
			}
		})
	}
}

// RFC 9110 §9.3.1: some backends read the content of a GET, which is not part
// of the cache key. Such a request must neither store what it gets nor be
// answered with what another one stored.
func TestRequestWithBodyBypassesCache(t *testing.T) {
	tests := []struct {
		name   string
		method string
		length int64 // -1: chunked
	}{
		{name: "GET with Content-Length", method: http.MethodGet, length: 4},
		{name: "chunked GET", method: http.MethodGet, length: -1},
		{name: "HEAD with Content-Length", method: http.MethodHead, length: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The backend answers with the parameters it reads from the content.
			calls := 0
			c, st := newTestCache(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				params, _ := io.ReadAll(r.Body)
				w.Header().Set("Cache-Control", "max-age=60")
				_, _ = io.WriteString(w, "hello "+string(params))
			}), nil)
			doRequest(c, http.MethodGet, testURL, nil)

			req := httptest.NewRequest(tt.method, testURL, strings.NewReader("evil"))
			req.ContentLength = tt.length
			rec := httptest.NewRecorder()
			c.ServeHTTP(rec, req)
			settle(c)
			if calls != 2 {
				t.Errorf("backend calls = %d, want 2: a request with content is forwarded", calls)
			}
			if got := rec.Header().Values("Cache-Status"); len(got) != 1 || got[0] != "traedis; fwd=bypass" {
				t.Errorf("Cache-Status = %q", got)
			}
			if st.sets != 1 {
				t.Errorf("sets = %d, want 1: the response to a request with content is not stored", st.sets)
			}
			if rec := doRequest(c, http.MethodGet, testURL, nil); rec.Body.String() != "hello " || !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
				t.Errorf("after it: got %q, Cache-Status = %q", rec.Body.String(), lastCacheStatus(rec))
			}
		})
	}
}

// Behind a proxy that terminates TLS, the backend redirects the http requests
// to https: that redirection must not answer the https requests, which reach
// Traefik over http too and would follow it in a loop.
func TestRedirectionToHTTPSIsNotServedToHTTPS(t *testing.T) {
	c, _ := newTestCache(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Proto") != "https" {
			http.Redirect(w, r, "https://example.com/a", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "secure")
	}), nil)
	plain := http.Header{"X-Forwarded-Proto": {"http"}}
	secure := http.Header{"X-Forwarded-Proto": {"https"}}

	if rec := doRequest(c, http.MethodGet, "http://example.com/a", plain); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("http: got %d", rec.Code)
	}
	if rec := doRequest(c, http.MethodGet, "http://example.com/a", secure); rec.Code != 200 || rec.Body.String() != "secure" {
		t.Errorf("https after http: got %d %q, Cache-Status = %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
	}
	// Each scheme has its own stored response.
	for _, tt := range []struct {
		hdr  http.Header
		code int
	}{{plain, http.StatusMovedPermanently}, {secure, 200}} {
		if rec := doRequest(c, http.MethodGet, "http://example.com/a", tt.hdr); rec.Code != tt.code || !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
			t.Errorf("%v: got %d, Cache-Status = %q", tt.hdr, rec.Code, lastCacheStatus(rec))
		}
	}
}

// A response to a GET reaches the client as it is written, not when the
// handler returns, whether the cache keeps a copy of it or not: under Yaegi
// v0.16.1 the backend cannot flush the writer the cache wraps.
func TestResponseStreams(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string
		wantSets     int
	}{
		{name: "not stored", cacheControl: "no-store"},
		{name: "stored", cacheControl: "max-age=60", wantSets: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			// The handler never flushes, as compiled code that cannot see Flush.
			c, st := newTestCache(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.Header().Set("Cache-Control", tt.cacheControl)
				_, _ = io.WriteString(w, "first\n")
				<-release
				_, _ = io.WriteString(w, "second\n")
			}), nil)
			srv := httptest.NewServer(onlyWriter{c})
			defer srv.Close()

			// The response is waited for aside: without a flush, its header does
			// not come before the handler returns.
			begun := make(chan streamed, 1)
			go func() {
				resp, err := http.Get(srv.URL)
				if err != nil {
					begun <- streamed{err: err}
					return
				}
				body := bufio.NewReader(resp.Body)
				line, _ := body.ReadString('\n')
				begun <- streamed{resp: resp, body: body, line: line}
			}()
			var s streamed
			select {
			case got := <-begun:
				s = got
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the first write was not sent before the handler finished")
			}
			if s.err != nil {
				close(release)
				t.Fatal(s.err)
			}
			//goland:noinspection GoUnhandledErrorResult
			defer s.resp.Body.Close()
			if s.line != "first\n" {
				t.Errorf("first line = %q", s.line)
			}

			// The stream is not altered, and still stored when it may be.
			close(release)
			rest, err := io.ReadAll(s.body)
			if err != nil || string(rest) != "second\n" {
				t.Errorf("rest of the body = %q, %v", rest, err)
			}
			settle(c)
			if st.sets != tt.wantSets {
				t.Errorf("sets = %d, want %d", st.sets, tt.wantSets)
			}
		})
	}
}

// streamed is the beginning of a response that is still being written.
type streamed struct {
	resp *http.Response
	body *bufio.Reader
	line string
	err  error
}

type ctxKey struct{}

func contextWithSpan(req *http.Request) context.Context {
	return context.WithValue(req.Context(), ctxKey{}, "span")
}

func TestTraceContextIsPropagated(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, nil)
	req := httptest.NewRequest(http.MethodGet, testURL, nil)
	req = req.WithContext(contextWithSpan(req))
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("Tracestate", "vendor=1")
	c.ServeHTTP(httptest.NewRecorder(), req)
	settle(c)

	got := b.lastReq
	if got.Context().Value(ctxKey{}) != "span" {
		t.Error("the request context must reach the backend")
	}
	if got.Header.Get("Traceparent") != req.Header.Get("Traceparent") || got.Header.Get("Tracestate") != "vendor=1" {
		t.Error("traceparent/tracestate must reach the backend")
	}
}

// The body of a stored response is only read from the store when it is sent:
// along with the entry when the answer is sure to have it, after it when a
// condition of the client turns out not to hold.
func TestBodyIsOnlyReadWhenSent(t *testing.T) {
	entryOnly := []string{""}
	withBody := []string{"", bodyField("")}
	tests := []struct {
		name      string
		method    string
		reqHdr    http.Header
		wantCode  int
		wantBody  string
		wantReads [][]string
	}{
		{name: "GET: one round trip", wantCode: 200, wantBody: "hello", wantReads: [][]string{withBody}},
		{name: "HEAD", method: http.MethodHead, wantCode: 200, wantReads: [][]string{entryOnly}},
		{name: "§4.3.2 condition that holds: 304", reqHdr: http.Header{"If-None-Match": {`"v1"`}}, wantCode: 304, wantReads: [][]string{entryOnly}},
		{name: "§4.3.2 If-Modified-Since that holds: 304", reqHdr: http.Header{"If-Modified-Since": {lastModified}}, wantCode: 304, wantReads: [][]string{entryOnly}},
		{name: "§4.3.2 condition that does not hold: the body is read then", reqHdr: http.Header{"If-None-Match": {`"v0"`}}, wantCode: 200, wantBody: "hello", wantReads: [][]string{entryOnly, withBody}},
		{name: "HEAD with a condition that does not hold", method: http.MethodHead, reqHdr: http.Header{"If-None-Match": {`"v0"`}}, wantCode: 200, wantReads: [][]string{entryOnly}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			c, st := newTestCache(t, o, nil)
			doRequest(c, http.MethodGet, testURL, nil)
			st.reads = nil
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := doRequest(c, method, testURL, tt.reqHdr)

			if o.calls != 1 || !isHit(lastCacheStatus(rec)) {
				t.Fatalf("expected a hit: %q (backend calls %d)", lastCacheStatus(rec), o.calls)
			}
			if rec.Code != tt.wantCode || rec.Body.String() != tt.wantBody {
				t.Errorf("got %d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if tt.wantCode == 200 && rec.Header().Get("Content-Length") != "5" {
				t.Errorf("Content-Length = %q: it is known without the body", rec.Header().Get("Content-Length"))
			}
			if !reflect.DeepEqual(st.reads, tt.wantReads) {
				t.Errorf("fields read = %q, want %q", st.reads, tt.wantReads)
			}
		})
	}
}

// changingStore changes what is stored right after its next read: what another
// request does between the read of an entry and the read of its body.
type changingStore struct {
	*memStore
	change func(st *memStore)
}

func (s *changingStore) get(ctx context.Context, key string, fields []string) ([][]byte, error) {
	values, err := s.memStore.get(ctx, key, fields)
	if change := s.change; change != nil {
		s.change = nil
		change(s.memStore)
	}
	return values, err
}

// The body read after its entry must be the one of that entry.
func TestBodyOfAnotherResponseIsNotServed(t *testing.T) {
	key := cacheKey(testURL)
	tests := []struct {
		name   string
		change func(st *memStore)
	}{
		{name: "another response of the same length was stored", change: func(st *memStore) {
			now := time.Now()
			_ = st.put(key, "", &entry{
				status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Etag": {`"v2"`}}, body: []byte("HELLO"),
				requestTime: now, responseTime: now,
			}, nil, time.Hour)
		}},
		{name: "the body expired", change: func(st *memStore) {
			_ = st.del(context.Background(), key, []string{bodyField("")})
		}},
		{name: "the response was invalidated", change: func(st *memStore) {
			_ = st.invalidate(context.Background(), key)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin()
			c, mem := newTestCache(t, o, nil)
			st := &changingStore{memStore: mem}
			c.store = st
			doRequest(c, http.MethodGet, testURL, nil)

			o.etag = `"v3"`
			o.body = "fresh"
			st.change = tt.change
			rec := doRequest(c, http.MethodGet, testURL, http.Header{"If-None-Match": {`"v0"`}})
			if rec.Code != 200 || rec.Body.String() != "fresh" || rec.Header().Get("ETag") != `"v3"` || o.calls != 2 {
				t.Fatalf("the backend must answer: got %d %q %v (backend calls %d)", rec.Code, rec.Body.String(), rec.Header(), o.calls)
			}
			if got := lastCacheStatus(rec); got != "traedis; fwd=uri-miss; fwd-status=200" {
				t.Errorf("Cache-Status = %q", got)
			}
			if rec := doRequest(c, http.MethodGet, testURL, nil); !isHit(lastCacheStatus(rec)) || rec.Body.String() != "fresh" {
				t.Errorf("its response must be stored: got %q %q", lastCacheStatus(rec), rec.Body.String())
			}
		})
	}
}

// RFC 5861 §4: the stored response that replaces a backend error has its body
// read then, when find left it in the store. Without it, the error goes
// through.
func TestStaleIfErrorReadsTheBody(t *testing.T) {
	for _, gone := range []bool{false, true} {
		b := &backend{status: 503, body: "error"}
		c, st := newTestCache(t, b, nil)
		storeAged(t, st, "max-age=60, stale-if-error=300", 2*time.Minute)
		if gone {
			if err := st.del(context.Background(), cacheKey(staleURL), []string{bodyField("")}); err != nil {
				t.Fatal(err)
			}
		}
		rec := doRequest(c, http.MethodGet, staleURL, http.Header{"If-None-Match": {`"v0"`}})
		wantCode, wantBody, wantStatus := 200, "old", "; detail=stale-if-error"
		if gone {
			wantCode, wantBody, wantStatus = 503, "error", "traedis; fwd=stale; fwd-status=503"
		}
		if rec.Code != wantCode || rec.Body.String() != wantBody || !strings.HasSuffix(lastCacheStatus(rec), wantStatus) || b.calls != 1 {
			t.Errorf("body gone = %v: got %d %q %q (backend calls %d)", gone, rec.Code, rec.Body.String(), lastCacheStatus(rec), b.calls)
		}
	}
}

// What the age and the freshness lifetime of a response are worked out from is
// stored with it, rather than parsed from its header on every hit.
func TestFreshnessIsStoredWithTheEntry(t *testing.T) {
	b := &backend{header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"10"}}, body: "hello"}
	c, st := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	e := st.entry(cacheKey(testURL), "")
	if e == nil || e.lifetime != time.Minute || e.initialAge < 10*time.Second || e.initialAge >= 11*time.Second {
		t.Fatalf("stored entry = %+v, want a lifetime of 1m and an initial age of 10s", e)
	}
	rec := doRequest(c, http.MethodGet, testURL, nil)
	if got := lastCacheStatus(rec); got != "traedis; hit; ttl=50" && got != "traedis; hit; ttl=49" {
		t.Errorf("Cache-Status = %q", got)
	}
	if rec.Header().Get("Age") != "10" {
		t.Errorf("§4.2.3 Age = %q, want 10", rec.Header().Get("Age"))
	}

	// Without explicit freshness, defaultTtl applies as it is configured now.
	b = &backend{header: http.Header{"Content-Type": {"text/plain"}}, body: "hello"}
	c, st = newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	if e := st.entry(cacheKey(testURL), ""); e == nil || e.lifetime != -1 {
		t.Fatalf("stored entry = %+v, want no explicit lifetime", e)
	}
	if rec := doRequest(c, http.MethodGet, testURL, nil); !isHit(lastCacheStatus(rec)) {
		t.Errorf("defaultTtl: got %q", lastCacheStatus(rec))
	}
	c.cfg.defaultTTL = 0
	if rec := doRequest(c, http.MethodGet, testURL, nil); lastCacheStatus(rec) != "traedis; fwd=stale; fwd-status=200" {
		t.Errorf("defaultTtl lowered to 0: got %q", lastCacheStatus(rec))
	}
}

// gatedStore holds its writes back until its gate is closed, as a slow Redis.
type gatedStore struct {
	*memStore
	gate   chan struct{}
	panics bool
}

func (s *gatedStore) set(ctx context.Context, key string, fields []fieldValue, ttl time.Duration, replace bool) error {
	<-s.gate
	if s.panics {
		panic("store panic")
	}
	return s.memStore.set(ctx, key, fields, ttl, replace)
}

func newGatedCache(t *testing.T, b http.Handler, mutate func(*Config)) (*cache, *gatedStore) {
	t.Helper()
	c, mem := newTestCache(t, b, mutate)
	st := &gatedStore{memStore: mem, gate: make(chan struct{})}
	c.store = st
	return c, st
}

// serve makes a request without waiting for what its response has to store.
func serve(t *testing.T, c *cache, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		c.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the request waits for the store")
	}
	return rec
}

func pendingWrites(c *cache) int {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return len(c.writing)
}

// A response is complete for its client once the handler returns: storing it
// must not hold that back, however slow Redis is.
func TestStoringDoesNotDelayTheResponse(t *testing.T) {
	b := cacheableBackend()
	c, st := newGatedCache(t, b, nil)
	rec := serve(t, c, http.MethodGet, testURL)
	if rec.Body.String() != "hello" || lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	if _, stored := st.lookup(cacheKey(testURL), ""); stored || pendingWrites(c) != 1 {
		t.Errorf("stored = %v, writes running = %d, want a write that waits for the store", stored, pendingWrites(c))
	}
	close(st.gate)
	settle(c)
	if _, stored := st.lookup(cacheKey(testURL), ""); !stored || pendingWrites(c) != 0 {
		t.Errorf("stored = %v, writes running = %d, want the response stored", stored, pendingWrites(c))
	}
}

// The next request for the URL waits for the response to be stored, which it
// is then served: a miss is followed by a hit.
func TestNextRequestWaitsForTheStore(t *testing.T) {
	b := cacheableBackend()
	c, st := newGatedCache(t, b, func(cfg *Config) { cfg.Redis.DialTimeout = "5s"; cfg.Redis.Timeout = "5s" })
	serve(t, c, http.MethodGet, testURL)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(st.gate)
	}()
	if rec := doRequest(c, http.MethodGet, testURL, nil); !isHit(lastCacheStatus(rec)) || b.calls != 1 {
		t.Errorf("Cache-Status = %q, backend calls = %d, want a hit", lastCacheStatus(rec), b.calls)
	}
}

// Not for longer than redis.dialTimeout: it is then forwarded, and its
// response not stored on top of the one that still is.
func TestNextRequestDoesNotWaitForASlowStore(t *testing.T) {
	b := cacheableBackend()
	c, st := newGatedCache(t, b, func(cfg *Config) { cfg.Redis.DialTimeout = "20ms" })
	serve(t, c, http.MethodGet, testURL)
	rec := serve(t, c, http.MethodGet, testURL)
	if lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" || b.calls != 2 || pendingWrites(c) != 1 {
		t.Errorf("Cache-Status = %q, backend calls = %d, writes running = %d", lastCacheStatus(rec), b.calls, pendingWrites(c))
	}
	close(st.gate)
	settle(c)
	if st.sets != 1 {
		t.Errorf("sets = %d, want 1", st.sets)
	}
}

// No more writes run than Redis has connections: past that, a response is not
// stored, and a later one is.
func TestStoreWritesAreBounded(t *testing.T) {
	b := cacheableBackend()
	c, st := newGatedCache(t, b, func(cfg *Config) { cfg.Redis.PoolSize = 2 })
	urls := []string{"http://example.com/1", "http://example.com/2", "http://example.com/3"}
	for _, u := range urls {
		serve(t, c, http.MethodGet, u)
	}
	if n := pendingWrites(c); n != 2 {
		t.Errorf("writes running = %d, want 2", n)
	}
	close(st.gate)
	settle(c)
	if _, stored := st.lookup(cacheKey(urls[2]), ""); stored || st.sets != 2 {
		t.Errorf("third response stored = %v, sets = %d", stored, st.sets)
	}
	doRequest(c, http.MethodGet, urls[2], nil)
	if _, stored := st.lookup(cacheKey(urls[2]), ""); !stored {
		t.Error("the response to a later request must be stored")
	}
}

// §4.4: a response that is still being stored when an unsafe request
// invalidates its URL does not outlive it, whether the write ends while the
// unsafe request waits for it or after.
func TestInvalidationOfAResponseBeingStored(t *testing.T) {
	for _, dialTimeout := range []string{"5s", "20ms"} {
		t.Run("redis.dialTimeout "+dialTimeout, func(t *testing.T) {
			// (Not as an argument: Yaegi v0.16.1 then fails to make a handler
			// of it in a subtest.)
			b := cacheableBackend()
			c, st := newGatedCache(t, b, func(cfg *Config) { cfg.Redis.DialTimeout = dialTimeout; cfg.Redis.Timeout = "5s" })
			serve(t, c, http.MethodGet, testURL)
			go func() {
				time.Sleep(100 * time.Millisecond)
				close(st.gate)
			}()
			rec := serve(t, c, http.MethodPost, testURL)
			if lastCacheStatus(rec) != "traedis; fwd=method; fwd-status=200; detail=invalidated" {
				t.Errorf("Cache-Status = %q", lastCacheStatus(rec))
			}
			settle(c)
			if _, stored := st.lookup(cacheKey(testURL), ""); stored {
				t.Error("the response stored before the unsafe request must be gone")
			}
		})
	}
}

// A panic while storing must not take Traefik down, nor keep the next
// responses from being stored.
func TestStorePanicIsRecovered(t *testing.T) {
	b := cacheableBackend()
	c, st := newGatedCache(t, b, nil)
	close(st.gate)
	st.panics = true
	doRequest(c, http.MethodGet, testURL, nil)
	if n := pendingWrites(c); n != 0 {
		t.Fatalf("writes running = %d, want 0", n)
	}
	st.panics = false
	doRequest(c, http.MethodGet, testURL, nil)
	if _, stored := st.lookup(cacheKey(testURL), ""); !stored {
		t.Error("the next response must be stored")
	}
}
