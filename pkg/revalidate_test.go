package traedis

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const staleURL = "http://example.com/a"

// storeAged stores a response to staleURL received age ago.
func storeAged(t *testing.T, st *memStore, cacheControl string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	storeEntry(t, st, staleURL, &entry{
		status: 200, header: http.Header{"Cache-Control": {cacheControl}, "Content-Type": {"text/plain"}}, body: []byte("old"),
		requestTime: at, responseTime: at,
	})
}

// waitRevalidations waits for the background revalidations of c to end.
func waitRevalidations(t *testing.T, c *cache) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		c.revalMu.Lock()
		n := len(c.revalidating)
		c.revalMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background revalidation still running")
}

func isStaleWhileRevalidate(status string) bool {
	return strings.HasPrefix(status, "traedis; hit; ttl=-") && strings.HasSuffix(status, "; detail=stale-while-revalidate")
}

func TestStaleWhileRevalidate(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

	req := httptest.NewRequest(http.MethodGet, staleURL, nil)
	req = req.WithContext(contextWithSpan(req))
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("Tracestate", "vendor=1")
	req.Header.Set("If-None-Match", `"v1"`)
	req.Header.Set("Range", "bytes=0-1")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("Accept-Language", "fr")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	settle(c)

	// The bytes that the request asks for: see serveRange.
	if rec.Code != 206 || rec.Body.String() != "ol" {
		t.Fatalf("RFC 5861 §3 the stale response must be served: got %d %q", rec.Code, rec.Body.String())
	}
	if got := lastCacheStatus(rec); got != "traedis; hit; ttl=-10; detail=stale-while-revalidate" && got != "traedis; hit; ttl=-11; detail=stale-while-revalidate" {
		t.Errorf("Cache-Status = %q", got)
	}
	if age := rec.Header().Get("Age"); age != "70" && age != "71" {
		t.Errorf("§4.2.3 Age = %q, want 70", age)
	}

	waitRevalidations(t, c)
	if b.calls != 1 {
		t.Fatalf("backend calls = %d, want 1 (background revalidation)", b.calls)
	}
	got := b.lastReq
	if got.Method != http.MethodGet || got.URL.Path != "/a" || got.Header.Get("Accept-Language") != "fr" {
		t.Errorf("revalidation request = %s %s %v", got.Method, got.URL, got.Header)
	}
	if got.Header.Get("Traceparent") != req.Header.Get("Traceparent") || got.Header.Get("Tracestate") != "vendor=1" {
		t.Error("traceparent/tracestate must reach the backend")
	}
	for _, name := range []string{"If-None-Match", "Range", "Cache-Control"} {
		if got.Header.Get(name) != "" {
			t.Errorf("%s of the client must not be sent by the revalidation", name)
		}
	}
	if got.Context().Value(ctxKey{}) != nil {
		t.Error("the context of a request that is over must not reach the backend")
	}
	if _, ok := got.Context().Deadline(); !ok {
		t.Error("the revalidation must have a deadline")
	}
	if req.Header.Get("If-None-Match") == "" {
		t.Error("the client request must not be modified")
	}

	// The entry was replaced: the next request is a fresh hit.
	next := doRequest(c, http.MethodGet, staleURL, nil)
	if b.calls != 1 || next.Body.String() != "hello" || !strings.HasPrefix(lastCacheStatus(next), "traedis; hit; ttl=") || isStaleWhileRevalidate(lastCacheStatus(next)) {
		t.Errorf("after revalidation: %q %q (backend calls %d)", next.Body.String(), lastCacheStatus(next), b.calls)
	}
}

func TestStaleWhileRevalidateOnHead(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

	rec := doRequest(c, http.MethodHead, staleURL, nil)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "3" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Errorf("HEAD: body %q, Content-Length %q, Cache-Status %q", rec.Body.String(), rec.Header().Get("Content-Length"), lastCacheStatus(rec))
	}
	waitRevalidations(t, c)
	if b.calls != 1 || b.lastReq.Method != http.MethodGet || st.sets != 2 {
		t.Errorf("the entry must be refreshed with a GET (calls %d, sets %d)", b.calls, st.sets)
	}
}

func TestDefaultStaleWhileRevalidate(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, func(c *Config) { c.DefaultStaleWhileRevalidate = "30s" })
	storeAged(t, st, "max-age=60", 70*time.Second)

	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Errorf("§4.2.4 the configuration allows the stale response: got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	if rec.Header().Get("Cache-Control") != "max-age=60" {
		t.Errorf("Cache-Control must never be rewritten: %q", rec.Header().Get("Cache-Control"))
	}
	waitRevalidations(t, c)
	if b.calls != 1 || st.sets != 2 {
		t.Errorf("the entry must be refreshed in the background (calls %d, sets %d)", b.calls, st.sets)
	}
}

func TestNotServedWhileRevalidating(t *testing.T) {
	tests := []struct {
		name         string
		cacheControl string
		age          time.Duration
		reqHdr       http.Header
		mutate       func(*Config)
	}{
		{name: "§4.2.4 no directive", cacheControl: "max-age=60", age: 70 * time.Second},
		{name: "RFC 5861 §3 window over", cacheControl: "max-age=60, stale-while-revalidate=30", age: 91 * time.Second},
		{name: "window capped at staleTtl", cacheControl: "max-age=60, stale-while-revalidate=3600", age: 80 * time.Second, mutate: func(c *Config) { c.StaleTTL = "10s" }},
		{name: "§5.2.2.2 must-revalidate", cacheControl: "max-age=60, stale-while-revalidate=30, must-revalidate", age: 70 * time.Second},
		{name: "§5.2.2.8 proxy-revalidate", cacheControl: "max-age=60, stale-while-revalidate=30, proxy-revalidate", age: 70 * time.Second},
		{name: "§5.2.2.10 s-maxage", cacheControl: "s-maxage=60, stale-while-revalidate=30", age: 70 * time.Second},
		{name: "§5.2.1.1 request max-age", cacheControl: "max-age=60, stale-while-revalidate=30", age: 70 * time.Second, reqHdr: http.Header{"Cache-Control": {"max-age=600"}}},
		{name: "§5.2.1.3 request min-fresh", cacheControl: "max-age=60, stale-while-revalidate=30", age: 70 * time.Second, reqHdr: http.Header{"Cache-Control": {"min-fresh=1"}}},
		{name: "defaultStaleWhileRevalidate over", cacheControl: "max-age=60", age: 91 * time.Second, mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "30s" }},
		{name: "defaultStaleWhileRevalidate does not replace the directive", cacheControl: "max-age=60, stale-while-revalidate=0", age: 70 * time.Second, mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "30s" }},
		{name: "defaultStaleWhileRevalidate with §5.2.2.2 must-revalidate", cacheControl: "max-age=60, must-revalidate", age: 70 * time.Second, mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "30s" }},
		{name: "defaultStaleIfError is not defaultStaleWhileRevalidate", cacheControl: "max-age=60", age: 70 * time.Second, mutate: func(c *Config) { c.DefaultStaleIfError = "30s" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cacheableBackend()
			c, st := newTestCache(t, b, tt.mutate)
			storeAged(t, st, tt.cacheControl, tt.age)
			rec := doRequest(c, http.MethodGet, staleURL, tt.reqHdr)
			if b.calls != 1 || rec.Body.String() != "hello" {
				t.Errorf("the request must be forwarded: got %q (backend calls %d)", rec.Body.String(), b.calls)
			}
			if got := lastCacheStatus(rec); got != "traedis; fwd=stale; fwd-status=200" {
				t.Errorf("Cache-Status = %q", got)
			}
			if len(c.revalidating) != 0 {
				t.Error("no background revalidation expected")
			}
		})
	}
}

func TestRevalidationOutcome(t *testing.T) {
	tests := []struct {
		name    string
		reqHdr  http.Header
		status  int
		respHdr http.Header
		body    string
		mutate  func(*Config)
		// what becomes of the stale entry
		replaced bool
		deleted  bool
	}{
		{name: "storable response replaces the entry", respHdr: http.Header{"Cache-Control": {"max-age=60"}}, replaced: true},
		{name: "RFC 5861 §4 backend error keeps it", status: 503, respHdr: http.Header{}},
		{name: "throttling keeps it", status: 429, respHdr: http.Header{}},
		{name: "status not in statusCodes deletes it", status: 404, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, deleted: true},
		{name: "resource gone deletes it", status: 410, respHdr: http.Header{}, deleted: true},
		{name: "a client's Authorization rejected by the backend cannot evict it", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 401, respHdr: http.Header{}},
		{name: "a client's Cookie rejected by the backend cannot evict it", reqHdr: http.Header{"Cookie": {"sid=1"}}, status: 403, respHdr: http.Header{"Cache-Control": {"no-store"}}},
		{name: "a request rejected by the backend cannot evict it", reqHdr: http.Header{"X-Custom": {"bad"}}, status: 400, respHdr: http.Header{}},
		{name: "§5.2.2.5 no-store deletes it", respHdr: http.Header{"Cache-Control": {"no-store"}}, deleted: true},
		{name: "§5.2.2.7 private deletes it", respHdr: http.Header{"Cache-Control": {"private, max-age=60"}}, deleted: true},
		{name: "body larger than maxBodyBytes deletes it", respHdr: http.Header{"Cache-Control": {"max-age=60"}}, body: strings.Repeat("x", 100), mutate: func(c *Config) { c.MaxBodyBytes = 10 }, deleted: true},
		{name: "a client's Cookie cannot evict it", reqHdr: http.Header{"Cookie": {"sid=1"}}, respHdr: http.Header{}},
		{name: "a client's Authorization cannot evict it", reqHdr: http.Header{"Authorization": {"Bearer x"}}, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "a client's no-store cannot evict it nor prevent the refresh", reqHdr: http.Header{"Cache-Control": {"no-store"}}, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, replaced: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if body == "" {
				body = "hello"
			}
			b := &backend{status: tt.status, header: tt.respHdr, body: body}
			c, st := newTestCache(t, b, tt.mutate)
			storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

			rec := doRequest(c, http.MethodGet, staleURL, tt.reqHdr)
			if rec.Code != 200 || rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
				t.Fatalf("the stale response must be served: got %d %q %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
			}
			waitRevalidations(t, c)
			if b.calls != 1 {
				t.Fatalf("backend calls = %d, want 1", b.calls)
			}
			if replaced := st.sets == 2; replaced != tt.replaced {
				t.Errorf("replaced = %v, want %v", replaced, tt.replaced)
			}
			_, kept := st.lookup(cacheKey(staleURL), "")
			if kept == tt.deleted {
				t.Errorf("deleted = %v, want %v", !kept, tt.deleted)
			}
			if _, bodyKept := st.lookup(cacheKey(staleURL), bodyField("")); bodyKept != kept {
				t.Errorf("body kept = %v, entry kept = %v: they go together", bodyKept, kept)
			}
			if kept {
				e := st.entry(cacheKey(staleURL), "")
				if e == nil {
					t.Fatal("field \"\" must hold an entry")
				}
				if want := map[bool]string{true: body, false: "old"}[tt.replaced]; string(e.body) != want {
					t.Errorf("stored body = %q, want %q", e.body, want)
				}
			}
		})
	}
}

// gatedBackend answers once its gate is closed.
type gatedBackend struct {
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (b *gatedBackend) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-b.gate
	w.Header().Set("Cache-Control", "max-age=60")
	_, _ = io.WriteString(w, "hello")
}

func (b *gatedBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestRevalidationIsNotRepeatedWhileRunning(t *testing.T) {
	b := &gatedBackend{gate: make(chan struct{})}
	s, err := parseConfig(CreateConfig())
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	c := newCache(b, "cache", s, st)
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

	for i := 0; i < 3; i++ {
		rec := doRequest(c, http.MethodGet, staleURL, nil)
		if rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
			t.Fatalf("request %d: got %q %q", i, rec.Body.String(), lastCacheStatus(rec))
		}
	}
	close(b.gate)
	waitRevalidations(t, c)
	if got := b.count(); got != 1 {
		t.Errorf("backend calls = %d, want 1 revalidation for 3 requests", got)
	}
	if rec := doRequest(c, http.MethodGet, staleURL, nil); rec.Body.String() != "hello" || b.count() != 1 {
		t.Errorf("after revalidation: %q (backend calls %d)", rec.Body.String(), b.count())
	}
}

// waitingBackend never answers: it waits for its request to be given up, as
// Traefik's proxy does, which then answers a 504.
type waitingBackend struct {
	gaveUp chan struct{}
}

func (b *waitingBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
	w.WriteHeader(http.StatusGatewayTimeout)
	close(b.gaveUp)
}

// A background revalidation is given revalidationTimeout: a backend that
// never answers does not keep its place for ever.
func TestRevalidationTimeout(t *testing.T) {
	b := &waitingBackend{gaveUp: make(chan struct{})}
	c, st := newTestCache(t, b, func(cfg *Config) { cfg.RevalidationTimeout = "50ms" })
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Fatalf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	<-b.gaveUp
	waitRevalidations(t, c)
	if e := st.entry(cacheKey(staleURL), ""); e == nil || e.status != http.StatusOK {
		t.Errorf("the stale entry must be kept: %+v", e)
	}
}

func TestRevalidationsAreBounded(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, func(cfg *Config) { cfg.MaxRevalidations = 3 })
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)
	for i := 0; i < c.cfg.maxRevalidations; i++ {
		c.revalidating["traedis:busy"+strconv.Itoa(i)] = true
	}

	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if rec.Body.String() != "old" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Errorf("the stale response must still be served: got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	if len(c.revalidating) != c.cfg.maxRevalidations || b.calls != 0 {
		t.Errorf("revalidations = %d, backend calls = %d: the limit must hold", len(c.revalidating), b.calls)
	}
}

type panicBackend struct{}

func (panicBackend) ServeHTTP(_ http.ResponseWriter, _ *http.Request) {
	panic("backend panic")
}

func TestRevalidationPanicIsRecovered(t *testing.T) {
	s, err := parseConfig(CreateConfig())
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	c := newCache(panicBackend{}, "cache", s, st)
	storeAged(t, st, "max-age=60, stale-while-revalidate=30", 70*time.Second)

	rec := doRequest(c, http.MethodGet, staleURL, nil)
	if rec.Body.String() != "old" {
		t.Fatalf("got %q", rec.Body.String())
	}
	waitRevalidations(t, c)
	if _, kept := st.lookup(cacheKey(staleURL), ""); !kept || st.sets != 1 {
		t.Error("the entry must be left as it was")
	}
}

// §3.3: a response that the backend cuts short is not stored, whether it had a
// Content-Length or not. Traefik's proxy (httputil.ReverseProxy) aborts such a
// response with a panic, but only for a request that has a server: the one of
// a background revalidation must keep it.
func TestIncompleteResponseIsNotStored(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "§3.3 shorter than its Content-Length", response: "HTTP/1.1 200 OK\r\nCache-Control: max-age=60\r\nContent-Length: 10\r\n\r\nhello"},
		{name: "§3.3 chunked without its last chunk", response: "HTTP/1.1 200 OK\r\nCache-Control: max-age=60\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = buf.WriteString(tt.response)
				_ = buf.Flush()
				_ = conn.Close()
			}))
			defer origin.Close()
			target, err := url.Parse(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			c, st := newTestCache(t, proxy, nil)
			front := httptest.NewServer(c)
			defer front.Close()
			uri := front.URL + "/a"
			key := cacheKey(uri)

			if resp, err := http.Get(uri); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if _, stored := st.lookup(key, ""); stored {
				t.Fatal("miss: the incomplete response was stored")
			}

			at := time.Now().Add(-70 * time.Second)
			storeEntry(t, st, uri, &entry{
				status: 200, header: http.Header{"Cache-Control": {"max-age=60, stale-while-revalidate=30"}}, body: []byte("old"),
				requestTime: at, responseTime: at,
			})
			resp, err := http.Get(uri)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "old" || !isStaleWhileRevalidate(resp.Header.Get("Cache-Status")) {
				t.Fatalf("got %q %q", body, resp.Header.Get("Cache-Status"))
			}
			waitRevalidations(t, c)
			if v, kept := st.lookup(key, ""); !kept || st.sets != 1 {
				t.Errorf("background revalidation: the entry must be left as it was (kept %v, %d bytes, %d writes)", kept, len(v.value), st.sets)
			}
		})
	}
}
