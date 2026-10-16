package traedis

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSafeMethod(t *testing.T) {
	for method, want := range map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true, http.MethodTrace: true,
		http.MethodPost: false, http.MethodPut: false, http.MethodPatch: false, http.MethodDelete: false,
		http.MethodConnect: false, "PURGE": false, "get": false,
	} {
		if got := safeMethod(method); got != want {
			t.Errorf("RFC 9110 §9.2.1 safeMethod(%q) = %v, want %v", method, got, want)
		}
	}
}

const invalidatedURL = "http://example.com/items/1?b=2&a=1"

// storeFresh stores a fresh response at uri, and two variants at variedURL.
func storeFresh(t *testing.T, st *memStore, uri string) {
	t.Helper()
	now := time.Now()
	storeEntry(t, st, uri, &entry{
		status: 200, header: http.Header{"Cache-Control": {"max-age=60"}}, body: []byte("cached"),
		requestTime: now, responseTime: now,
	})
}

func TestInvalidation(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		status          int
		wantInvalidated bool
	}{
		{name: "§4.4 POST 200", method: http.MethodPost, status: 200, wantInvalidated: true},
		{name: "§4.4 POST 201", method: http.MethodPost, status: 201, wantInvalidated: true},
		{name: "§4.4 PUT 204", method: http.MethodPut, status: 204, wantInvalidated: true},
		{name: "§4.4 PATCH 200", method: http.MethodPatch, status: 200, wantInvalidated: true},
		{name: "§4.4 DELETE 204", method: http.MethodDelete, status: 204, wantInvalidated: true},
		{name: "§4.4 a method whose safety is unknown", method: "PURGE", status: 200, wantInvalidated: true},
		{name: "§4.4 3xx is not an error", method: http.MethodPost, status: 303, wantInvalidated: true},
		{name: "§4.4 304", method: http.MethodPut, status: 304, wantInvalidated: true},
		{name: "§4.4 400 is an error", method: http.MethodPost, status: 400},
		{name: "§4.4 405", method: http.MethodDelete, status: 405},
		{name: "§4.4 500", method: http.MethodPost, status: 500},
		{name: "§4.4 503", method: http.MethodPut, status: 503},
		{name: "RFC 9110 §9.2.1 OPTIONS is safe", method: http.MethodOptions, status: 204},
		{name: "RFC 9110 §9.2.1 TRACE is safe", method: http.MethodTrace, status: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &backend{status: tt.status, header: http.Header{"Cache-Status": {"origin; fwd=miss"}, "Location": {"/items/2"}}, body: "done"}
			c, st := newTestCache(t, b, nil)
			storeFresh(t, st, invalidatedURL)
			storeFresh(t, st, "http://example.com/items/1")
			storeFresh(t, st, "http://example.com/items/2")
			// A variant of the target URI, in the same key.
			if err := st.putRaw(cacheKey(invalidatedURL), "variant", []byte("v")); err != nil {
				t.Fatal(err)
			}

			rec := doRequest(c, tt.method, invalidatedURL, nil)
			if b.calls != 1 || b.lastReq.Method != tt.method || rec.Code != tt.status {
				t.Fatalf("the request must reach the backend: got %d (backend calls %d)", rec.Code, b.calls)
			}
			if tt.status != 204 && tt.status != 304 && rec.Body.String() != "done" {
				t.Errorf("body = %q", rec.Body.String())
			}
			_, kept := st.lookup(cacheKey(invalidatedURL), "")
			_, variantKept := st.lookup(cacheKey(invalidatedURL), "variant")
			if kept == tt.wantInvalidated || variantKept == tt.wantInvalidated {
				t.Errorf("§4.4 stored responses of the target URI kept = %v, %v; want invalidated = %v", kept, variantKept, tt.wantInvalidated)
			}
			for _, other := range []string{"http://example.com/items/1", "http://example.com/items/2"} {
				if _, ok := st.lookup(cacheKey(other), ""); !ok {
					t.Errorf("%s is another URI: it must be kept", other)
				}
			}

			got := rec.Header().Values("Cache-Status")
			want := "traedis; fwd=method; fwd-status=" + strconv.Itoa(tt.status)
			if tt.wantInvalidated {
				want += "; detail=invalidated"
			}
			if safeMethod(tt.method) {
				// Not handled at all (see TestPassThroughDoesNotWrapWriter).
				if st.invalidations != 0 {
					t.Errorf("invalidations = %d, want none", st.invalidations)
				}
				return
			}
			if len(got) != 2 || got[0] != "origin; fwd=miss" || got[1] != want {
				t.Errorf("Cache-Status = %q, want %q after the backend's", got, want)
			}
		})
	}
}

func TestInvalidationThenMiss(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, func(cfg *Config) { cfg.ExposeKey = true })
	doRequest(c, http.MethodGet, testURL, nil)
	if rec := doRequest(c, http.MethodGet, testURL, nil); !isHit(lastCacheStatus(rec)) {
		t.Fatalf("expected a hit: %q", lastCacheStatus(rec))
	}

	rec := doRequest(c, http.MethodPost, testURL, nil)
	want := `traedis; fwd=method; fwd-status=200; key="` + cacheKey(testURL) + `"; detail=invalidated`
	if lastCacheStatus(rec) != want {
		t.Errorf("Cache-Status = %q, want %q", lastCacheStatus(rec), want)
	}
	rec = doRequest(c, http.MethodGet, testURL, nil)
	if b.calls != 3 || lastCacheStatus(rec) != `traedis; fwd=uri-miss; fwd-status=200; key="`+cacheKey(testURL)+`"` {
		t.Errorf("§4.4 the next GET must be a miss: %q (backend calls %d)", lastCacheStatus(rec), b.calls)
	}
}

// The stored responses are gone before the client knows its request succeeded.
func TestInvalidationBeforeResponse(t *testing.T) {
	c, st := newTestCache(t, nil, nil)
	storeFresh(t, st, "http://example.com/a")
	c.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, kept := st.lookup(cacheKey("http://example.com/a"), ""); !kept {
			t.Error("nothing must be invalidated before the backend answers")
		}
		w.WriteHeader(http.StatusNoContent)
		if _, kept := st.lookup(cacheKey("http://example.com/a"), ""); kept {
			t.Error("§4.4 the key must be deleted when the status is received")
		}
	})
	doRequest(c, http.MethodDelete, "http://example.com/a", nil)
}

func TestInvalidationWithImplicitStatus(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"nothing written": func(http.ResponseWriter, *http.Request) {},
		"body only":       func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "done") },
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			c, st := newTestCache(t, h, nil)
			storeFresh(t, st, "http://example.com/a")
			rec := doRequest(c, http.MethodPost, "http://example.com/a", nil)
			if rec.Code != 200 || st.invalidations != 1 || lastCacheStatus(rec) != "traedis; fwd=method; fwd-status=200; detail=invalidated" {
				t.Errorf("an implicit 200 invalidates: got %d %q (invalidations %d)", rec.Code, lastCacheStatus(rec), st.invalidations)
			}
		})
	}
}

func TestInvalidationFailsOpen(t *testing.T) {
	b := cacheableBackend()
	c, st := newTestCache(t, b, nil)
	st.err = errors.New("redis down")
	rec := doRequest(c, http.MethodPost, testURL, nil)
	if rec.Code != 200 || rec.Body.String() != "hello" || b.calls != 1 {
		t.Fatalf("fail open: got %d %q", rec.Code, rec.Body.String())
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=method; fwd-status=200; detail=redis" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestInvalidationPreservesHijacker(t *testing.T) {
	c, st := newTestCache(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the writer of an unsafe request must implement http.Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack() = %v", err)
			return
		}
		//goland:noinspection GoUnhandledErrorResult
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = buf.Flush()
	}), nil)
	srv := httptest.NewServer(c)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hijacked" {
		t.Errorf("body = %q", body)
	}
	if st.invalidations != 0 {
		t.Error("a hijacked response has no status of ours: nothing is invalidated")
	}
}

// A response to an unsafe request reaches the client as it is written, not
// when the handler returns.
func TestUnsafeResponseStreams(t *testing.T) {
	release := make(chan struct{})
	// The handler never flushes, as compiled code that cannot see Flush.
	c, _ := newTestCache(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "first\n")
		<-release
		_, _ = io.WriteString(w, "second\n")
	}), nil)
	srv := httptest.NewServer(onlyWriter{c})
	defer srv.Close()
	defer close(release)

	resp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case got := <-line:
		if got != "first\n" {
			t.Errorf("first line = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first write was not sent before the handler finished")
	}
}

// onlyWriter hands the cache the server's writer itself, like Traefik does.
type onlyWriter struct {
	h http.Handler
}

func (o onlyWriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.h.ServeHTTP(w, r)
}
