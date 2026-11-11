package traedis

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldBackend holds its responses until the test lets them go: their header once head is closed, their body once tail is. Its body is
// "hello" followed by the Accept-Language of the request.
type heldBackend struct {
	mu      sync.Mutex
	calls   int
	header  http.Header
	panics  bool          // the first request panics, as the proxy of Traefik does when the client goes away
	arrived chan struct{} // gets a value for each request that reaches the backend
	head    chan struct{}
	tail    chan struct{}
}

func newHeldBackend(header http.Header) *heldBackend {
	return &heldBackend{header: header, arrived: make(chan struct{}, 64), head: make(chan struct{}), tail: make(chan struct{})}
}

func (b *heldBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	b.arrived <- struct{}{}
	<-b.head
	if b.panics && first {
		panic(http.ErrAbortHandler)
	}
	for k, v := range b.header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.WriteHeader(http.StatusOK)
	<-b.tail
	_, _ = io.WriteString(w, "hello"+r.Header.Get("Accept-Language"))
}

func (b *heldBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *heldBackend) open() {
	close(b.head)
	close(b.tail)
}

// arrive waits for n more requests to reach b.
func arrive(t *testing.T, b *heldBackend, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-b.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d requests reached the backend", i, n)
		}
	}
}

// bgRequest is one made in the background by startRequest.
type bgRequest struct {
	rec  *httptest.ResponseRecorder
	done chan struct{}
}

// startRequest makes a request to c in the background. A panic of the backend ends it, as Traefik recovers it.
func startRequest(c *cache, method, target string, hdr http.Header) *bgRequest {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	r := &bgRequest{rec: httptest.NewRecorder(), done: make(chan struct{})}
	go serveInBackground(c, r, req)
	return r
}

func serveInBackground(c *cache, r *bgRequest, req *http.Request) {
	defer close(r.done)
	defer recoverAbort()
	c.ServeHTTP(r.rec, req)
}

func recoverAbort() {
	_ = recover()
}

// startMany starts n requests that will wait for the one already forwarded.
func startMany(c *cache, n int, method, target string, hdr http.Header) []*bgRequest {
	var requests []*bgRequest
	for i := 0; i < n; i++ {
		requests = append(requests, startRequest(c, method, target, hdr))
	}
	// Time for them to look for a response and to start waiting.
	time.Sleep(200 * time.Millisecond)
	return requests
}

func finish(t *testing.T, c *cache, requests ...*bgRequest) {
	t.Helper()
	for _, r := range requests {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("a request is still waiting")
		}
	}
	settle(c)
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	if len(c.flights) != 0 {
		t.Errorf("%d flights left", len(c.flights))
	}
}

func collapsing(cfg *Config) {
	cfg.CollapseTimeout = "5s"
}

func isCollapsed(status, fwd string) bool {
	return strings.HasPrefix(status, "traedis; fwd="+fwd+"; ttl=") && strings.HasSuffix(status, "; collapsed")
}

var publicMaxAge = http.Header{"Cache-Control": {"public, max-age=60"}}

// The requests for a URL that is being fetched wait for its response, which they are all served: the backend gets one request.
func TestCollapsedRequests(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, _ := newTestCache(t, b, collapsing)
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	waiting := startMany(c, 4, http.MethodGet, testURL, nil)
	b.open()
	finish(t, c, append(waiting, leader)...)

	if n := b.count(); n != 1 {
		t.Errorf("backend calls = %d, want 1", n)
	}
	if got := lastCacheStatus(leader.rec); got != "traedis; fwd=uri-miss; fwd-status=200" {
		t.Errorf("leader Cache-Status = %q", got)
	}
	for _, r := range waiting {
		if got := lastCacheStatus(r.rec); r.rec.Code != 200 || r.rec.Body.String() != "hello" || !isCollapsed(got, "uri-miss") {
			t.Errorf("got %d %q, Cache-Status %q", r.rec.Code, r.rec.Body.String(), got)
		}
	}
}

// Off by default: every request that misses is forwarded.
func TestRequestsAreNotCollapsedByDefault(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, _ := newTestCache(t, b, nil)
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	others := []*bgRequest{startRequest(c, http.MethodGet, testURL, nil), startRequest(c, http.MethodGet, testURL, nil)}
	arrive(t, b, 2)
	b.open()
	finish(t, c, append(others, leader)...)
	if n := b.count(); n != 3 {
		t.Errorf("backend calls = %d, want 3", n)
	}
}

// A stale response is refreshed once for all the requests that find it.
func TestCollapsedRequestsForAStaleResponse(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, st := newTestCache(t, b, collapsing)
	storeReceived(t, st, testURL, http.Header{"Cache-Control": {"max-age=60"}}, time.Now().Add(-2*time.Minute))
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	waiting := startMany(c, 2, http.MethodGet, testURL, nil)
	b.open()
	finish(t, c, append(waiting, leader)...)

	if n := b.count(); n != 1 {
		t.Errorf("backend calls = %d, want 1", n)
	}
	for _, r := range waiting {
		if got := lastCacheStatus(r.rec); r.rec.Body.String() != "hello" || !isCollapsed(got, "stale") {
			t.Errorf("got %q, Cache-Status %q", r.rec.Body.String(), got)
		}
	}
}

// A HEAD waits for a GET, and is answered from its response.
func TestHeadWaitsForAGet(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, _ := newTestCache(t, b, collapsing)
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	head := startMany(c, 1, http.MethodHead, testURL, nil)[0]
	b.open()
	finish(t, c, head, leader)

	if got := lastCacheStatus(head.rec); b.count() != 1 || !isCollapsed(got, "uri-miss") {
		t.Errorf("backend calls = %d, Cache-Status = %q", b.count(), got)
	}
	if head.rec.Body.Len() != 0 || head.rec.Header().Get("Content-Length") != "5" {
		t.Errorf("body = %q, Content-Length = %q", head.rec.Body.String(), head.rec.Header().Get("Content-Length"))
	}
}

// A response that is not stored lets the requests go as soon as its header says so, not once its body is sent: they are forwarded
// together.
func TestResponseNotStoredLetsTheRequestsGo(t *testing.T) {
	b := newHeldBackend(http.Header{"Cache-Control": {"private"}})
	c, _ := newTestCache(t, b, collapsing)
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	waiting := startMany(c, 3, http.MethodGet, testURL, nil)
	close(b.head)
	// They reach the backend while the body of the first response is still held back.
	arrive(t, b, 3)
	close(b.tail)
	finish(t, c, append(waiting, leader)...)

	for _, r := range waiting {
		if got := lastCacheStatus(r.rec); got != "traedis; fwd=uri-miss; fwd-status=200" || r.rec.Body.String() != "hello" {
			t.Errorf("got %q, Cache-Status %q", r.rec.Body.String(), got)
		}
	}
}

// The requests don't wait for longer than collapseTimeout: they are forwarded then.
func TestCollapseTimeout(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, _ := newTestCache(t, b, func(cfg *Config) { cfg.CollapseTimeout = "50ms" })
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	waiting := []*bgRequest{startRequest(c, http.MethodGet, testURL, nil), startRequest(c, http.MethodGet, testURL, nil)}
	arrive(t, b, 2)
	b.open()
	finish(t, c, append(waiting, leader)...)

	for _, r := range waiting {
		if got := lastCacheStatus(r.rec); got != "traedis; fwd=uri-miss; fwd-status=200" {
			t.Errorf("Cache-Status = %q", got)
		}
	}
}

// A request forwarded that fails, here because its client went away, lets the requests that wait for it go: they are forwarded.
func TestFailedRequestLetsTheRequestsGo(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	b.panics = true
	c, _ := newTestCache(t, b, collapsing)
	leader := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	waiting := startMany(c, 2, http.MethodGet, testURL, nil)
	b.open()
	finish(t, c, append(waiting, leader)...)

	if n := b.count(); n != 3 {
		t.Errorf("backend calls = %d, want 3", n)
	}
	for _, r := range waiting {
		if got := lastCacheStatus(r.rec); got != "traedis; fwd=uri-miss; fwd-status=200" || r.rec.Body.String() != "hello" {
			t.Errorf("got %q, Cache-Status %q", r.rec.Body.String(), got)
		}
	}
}

// A request that the stored response does not answer, because it selects another variant, is forwarded once it is stored: never served
// the response of another variant.
func TestCollapsedRequestsOfAnotherVariant(t *testing.T) {
	b := newHeldBackend(http.Header{"Cache-Control": {"public, max-age=60"}, "Vary": {"Accept-Language"}})
	c, _ := newTestCache(t, b, collapsing)
	fr := http.Header{"Accept-Language": {"fr"}}
	leader := startRequest(c, http.MethodGet, testURL, fr)
	arrive(t, b, 1)
	sameVariant := startMany(c, 2, http.MethodGet, testURL, fr)
	other := startMany(c, 1, http.MethodGet, testURL, http.Header{"Accept-Language": {"en"}})[0]
	b.open()
	finish(t, c, append(sameVariant, leader, other)...)

	if n := b.count(); n != 2 {
		t.Errorf("backend calls = %d, want 2", n)
	}
	for _, r := range sameVariant {
		if got := lastCacheStatus(r.rec); r.rec.Body.String() != "hellofr" || !isCollapsed(got, "uri-miss") {
			t.Errorf("got %q, Cache-Status %q", r.rec.Body.String(), got)
		}
	}
	if got := lastCacheStatus(other.rec); other.rec.Body.String() != "helloen" || got != "traedis; fwd=vary-miss; fwd-status=200" {
		t.Errorf("other variant: got %q, Cache-Status %q", other.rec.Body.String(), got)
	}
}

// §4.3.2: a conditional request is forwarded as it is when the cache has nothing to validate: its 304 would not be stored, so nobody waits
// for it.
func TestConditionalRequestIsNotWaitedFor(t *testing.T) {
	b := newHeldBackend(publicMaxAge)
	c, _ := newTestCache(t, b, collapsing)
	conditionalReq := startRequest(c, http.MethodGet, testURL, http.Header{"If-None-Match": {`"a"`}})
	arrive(t, b, 1)
	plain := startRequest(c, http.MethodGet, testURL, nil)
	arrive(t, b, 1)
	b.open()
	finish(t, c, conditionalReq, plain)
}

// Requests whose own directives refuse the stored response, or that must not be forwarded, never wait.
func TestCollapsible(t *testing.T) {
	tests := []struct {
		name  string
		reqCC string
		fwd   string
		want  bool
	}{
		{name: "miss", fwd: "uri-miss", want: true},
		{name: "variant miss", fwd: "vary-miss", want: true},
		{name: "stale", fwd: "stale", want: true},
		{name: "refused by the request", fwd: "request"},
		{name: "only-if-cached", reqCC: "only-if-cached", fwd: "uri-miss"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapsible(parseCacheControl([]string{tt.reqCC}), tt.fwd); got != tt.want {
				t.Errorf("collapsible(%q, %q) = %v, want %v", tt.reqCC, tt.fwd, got, tt.want)
			}
		})
	}
}

func TestLeads(t *testing.T) {
	withETag := &entry{header: http.Header{"Etag": {`"a"`}}}
	withoutValidator := &entry{header: http.Header{}}
	tests := []struct {
		name   string
		method string
		header http.Header
		reqCC  string
		stored *entry
		want   bool
	}{
		{name: "GET", method: http.MethodGet, want: true},
		{name: "HEAD: its response is not stored", method: http.MethodHead},
		{name: "no-store", method: http.MethodGet, reqCC: "no-store"},
		{name: "conditional, nothing to validate", method: http.MethodGet, header: http.Header{"If-None-Match": {`"a"`}}},
		{
			name: "conditional, stored response without validator", method: http.MethodGet,
			header: http.Header{"If-Modified-Since": {"Mon, 02 Nov 2026 10:00:00 GMT"}}, stored: withoutValidator,
		},
		{
			name: "conditional, stored response validated instead", method: http.MethodGet,
			header: http.Header{"If-None-Match": {`"b"`}}, stored: withETag, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, testURL, nil)
			for k, v := range tt.header {
				r.Header[k] = v
			}
			if got := leads(r, parseCacheControl([]string{tt.reqCC}), tt.stored); got != tt.want {
				t.Errorf("leads() = %v, want %v", got, tt.want)
			}
		})
	}
}
