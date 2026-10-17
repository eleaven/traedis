// Package benchmark measures the plugin the way Traefik uses it: through New and the handler it returns, against a real Redis
// (TRAEDIS_REDIS_DSN). run.sh runs it under Yaegi.
package benchmark

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	traedis "github.com/eleaven/traedis/pkg"
)

const (
	defaultMaxBody = 5 << 20
	// longLived keeps an entry fresh for a whole run; shortLived lets the entries of a benchmark that creates many of them expire right
	// behind it.
	longLived  = "max-age=300"
	shortLived = "max-age=1"
)

// runID isolates a run: its URLs (hence its Redis keys) are never those of a previous or concurrent run. Entries are left to expire.
var runID = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())

// seq makes URLs unique across the successive calls of a benchmark function.
var seq int

var sizes = []struct {
	name  string
	bytes int
}{
	{"1KB", 1 << 10},
	{"100KB", 100 << 10},
	{"1MB", 1 << 20},
}

// backend is the next handler. http.ServeContent is compiled code, like Traefik's reverse proxy: under Yaegi it reaches the plugin's writer
// through the same wrapper, and it copies the body in 32 KiB chunks.
type backend struct {
	body         []byte
	cacheControl string
	vary         string
}

func (h *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", h.cacheControl)
	if h.vary != "" {
		w.Header().Set("Vary", h.vary)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(h.body))
}

func newHandler(b *testing.B, next http.Handler, maxBodyBytes int64) http.Handler {
	b.Helper()
	dsn := os.Getenv("TRAEDIS_REDIS_DSN")
	if dsn == "" {
		b.Skip("TRAEDIS_REDIS_DSN is not set")
	}
	cfg := traedis.CreateConfig()
	cfg.Redis.DSN = dsn
	// A slow machine must not turn the run into bypasses.
	cfg.Redis.DialTimeout = "5s"
	cfg.Redis.Timeout = "5s"
	cfg.StaleTTL = "0s" // entries are gone as soon as they expire
	cfg.MaxBodyBytes = maxBodyBytes
	h, err := traedis.New(context.Background(), next, cfg, "bench")
	if err != nil {
		b.Fatal(err)
	}
	return h
}

func newRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, "http://bench.local/"+runID+path, nil)
}

// serve sends one request through the middleware. The body goes to buf, reused between iterations: the harness must not allocate what the
// plugin writes.
func serve(h http.Handler, req *http.Request, buf *bytes.Buffer) *httptest.ResponseRecorder {
	buf.Reset()
	rec := httptest.NewRecorder()
	rec.Body = buf
	h.ServeHTTP(rec, req)
	return rec
}

// expect fails the benchmark when it did not measure the outcome it is named after.
func expect(b *testing.B, rec *httptest.ResponseRecorder, want string) {
	b.Helper()
	if rec == nil {
		b.Fatalf("no response, want Cache-Status %q", want)
	}
	if got := rec.Header().Get("Cache-Status"); !strings.Contains(got, want) {
		b.Fatalf("Cache-Status = %q, want %q", got, want)
	}
}

// BenchmarkHit serves a fresh stored response: key, HMGET of the entry and its body, decoding, write.
func BenchmarkHit(b *testing.B) {
	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			h := newHandler(b, &backend{body: make([]byte, size.bytes), cacheControl: longLived}, defaultMaxBody)
			req := newRequest(http.MethodGet, "/hit/"+size.name+"?b=2&a=1")
			buf := &bytes.Buffer{}
			rec := serve(h, req, buf) // stored here

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rec = serve(h, req, buf)
			}
			b.StopTimer()
			expect(b, rec, "hit")
		})
	}
}

// BenchmarkHitVariant is BenchmarkHit/1KB for a response with Vary: it adds the marker read along with the variant, and its selection by
// the request headers.
func BenchmarkHitVariant(b *testing.B) {
	h := newHandler(
		b, &backend{body: make([]byte, 1<<10), cacheControl: longLived, vary: "Accept-Language, Accept-Encoding"}, defaultMaxBody,
	)
	req := newRequest(http.MethodGet, "/hit-variant?b=2&a=1")
	req.Header.Set("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	buf := &bytes.Buffer{}
	rec := serve(h, req, buf) // stored here

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "hit")
}

// BenchmarkHitParallel is BenchmarkHit/1KB from GOMAXPROCS goroutines: it adds the contention on the Redis connection pool.
func BenchmarkHitParallel(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 1<<10), cacheControl: longLived}, defaultMaxBody)
	path := "/hit-parallel?b=2&a=1"
	serve(h, newRequest(http.MethodGet, path), &bytes.Buffer{}) // stored here

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := newRequest(http.MethodGet, path)
		buf := &bytes.Buffer{}
		for pb.Next() {
			rec := serve(h, req, buf)
			if got := rec.Header().Get("Cache-Status"); !strings.Contains(got, "hit") {
				b.Errorf("Cache-Status = %q, want a hit", got)
				return
			}
		}
	})
}

// BenchmarkMiss requests a new URL every time: key, HMGET, recording, encoding, HSETEX.
func BenchmarkMiss(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 1<<10), cacheControl: shortLived}, defaultMaxBody)
	req := newRequest(http.MethodGet, "/miss")
	buf := &bytes.Buffer{}
	var rec *httptest.ResponseRecorder

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq++
		req.URL.RawQuery = "i=" + strconv.Itoa(seq)
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "fwd=uri-miss")
	expect(b, serve(h, req, buf), "hit") // it was stored
}

// BenchmarkStore measures the write path on larger bodies: recording, encoding, HSETEX. The client sends no-cache, so the response is
// fetched and stored again over the same key: nothing piles up in Redis.
func BenchmarkStore(b *testing.B) {
	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			h := newHandler(b, &backend{body: make([]byte, size.bytes), cacheControl: longLived}, defaultMaxBody)
			path := "/store/" + size.name
			req := newRequest(http.MethodGet, path)
			req.Header.Set("Cache-Control", "no-cache")
			buf := &bytes.Buffer{}
			var rec *httptest.ResponseRecorder

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rec = serve(h, req, buf)
			}
			b.StopTimer()
			expect(b, rec, "fwd=request")
			expect(b, serve(h, newRequest(http.MethodGet, path), buf), "hit") // it was stored
		})
	}
}

// BenchmarkNotStored is the cost added to a response that may not be cached: key, HMGET, and the recorder deciding not to keep the body.
func BenchmarkNotStored(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 1<<10), cacheControl: "no-store"}, defaultMaxBody)
	req := newRequest(http.MethodGet, "/not-stored")
	buf := &bytes.Buffer{}
	var rec *httptest.ResponseRecorder

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "fwd=uri-miss")
}

// BenchmarkOversize streams a cacheable body larger than maxBodyBytes: it must go through without being buffered.
func BenchmarkOversize(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 2<<20), cacheControl: longLived}, 1<<20)
	req := newRequest(http.MethodGet, "/oversize")
	buf := &bytes.Buffer{}
	var rec *httptest.ResponseRecorder

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "fwd=uri-miss") // still a miss: never stored
}

// BenchmarkPassThrough is a request the cache has nothing to do with (OPTIONS): the writer is not wrapped and Redis is not contacted.
func BenchmarkPassThrough(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 1<<10), cacheControl: "no-store"}, defaultMaxBody)
	req := newRequest(http.MethodOptions, "/pass-through")
	buf := &bytes.Buffer{}
	var rec *httptest.ResponseRecorder

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "fwd=method")
}

// BenchmarkInvalidate is an unsafe request (POST) that succeeds: key, DEL, and a response flushed as it is written.
func BenchmarkInvalidate(b *testing.B) {
	h := newHandler(b, &backend{body: make([]byte, 1<<10), cacheControl: "no-store"}, defaultMaxBody)
	req := newRequest(http.MethodPost, "/invalidate")
	buf := &bytes.Buffer{}
	var rec *httptest.ResponseRecorder

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec = serve(h, req, buf)
	}
	b.StopTimer()
	expect(b, rec, "detail=invalidated")
}
