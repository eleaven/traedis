package traedis

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type hookStub struct {
	calls    int
	status   int
	capture  bool
	withhold bool
}

func (h *hookStub) beforeHeader(status int, hdr http.Header) disposition {
	h.calls++
	h.status = status
	hdr.Add("X-Hook", "1")
	if h.withhold {
		return withhold
	}
	if h.capture {
		return keepCopy
	}
	return passOn
}

// writerStub records the status codes written.
type writerStub struct {
	header http.Header
	codes  []int
	body   bytes.Buffer
}

func (w *writerStub) Header() http.Header         { return w.header }
func (w *writerStub) WriteHeader(code int)        { w.codes = append(w.codes, code) }
func (w *writerStub) Write(p []byte) (int, error) { return w.body.Write(p) }

func TestRecorderCapturesBody(t *testing.T) {
	rw := httptest.NewRecorder()
	hook := &hookStub{capture: true}
	rec := newRecorder(rw, hook, 1024, nil)

	rec.Header().Set("Content-Type", "text/plain")
	_, _ = rec.Write([]byte("hello "))
	_, _ = rec.Write([]byte("world"))

	if rw.Code != 200 || rw.Body.String() != "hello world" || rw.Header().Get("X-Hook") != "1" {
		t.Errorf("client got %d %q %v", rw.Code, rw.Body.String(), rw.Header())
	}
	if hook.calls != 1 || hook.status != 200 {
		t.Errorf("hook called %d times with %d", hook.calls, hook.status)
	}
	if body, ok := rec.body(); !ok || string(body) != "hello world" {
		t.Errorf("body() = %q, %v", body, ok)
	}
}

func TestRecorderNoCaptureWhenHookDeclines(t *testing.T) {
	rec := newRecorder(httptest.NewRecorder(), &hookStub{capture: false}, 1024, nil)
	rec.WriteHeader(http.StatusNotFound)
	_, _ = rec.Write([]byte("nope"))
	if _, ok := rec.body(); ok {
		t.Error("body must not be captured")
	}
}

func TestRecorderWithholdsResponse(t *testing.T) {
	rw := &writerStub{header: http.Header{}}
	rec := newRecorder(rw, &hookStub{withhold: true}, 1024, nil)
	rec.WriteHeader(http.StatusEarlyHints)
	rec.WriteHeader(http.StatusServiceUnavailable)
	n, err := rec.Write([]byte("unavailable"))
	rec.Flush()

	if n != 11 || err != nil {
		t.Errorf("Write() = %d, %v: the backend must not see an error", n, err)
	}
	if len(rw.codes) != 1 || rw.codes[0] != http.StatusEarlyHints || rw.body.Len() != 0 {
		t.Errorf("client got codes %v and body %q, want only the informational response", rw.codes, rw.body.String())
	}
	if !rec.withheld || rec.status != http.StatusServiceUnavailable {
		t.Errorf("withheld = %v, status = %d", rec.withheld, rec.status)
	}
	if _, ok := rec.body(); ok {
		t.Error("a withheld response must not be captured")
	}
}

func TestRecorderStreamsBodiesLargerThanMax(t *testing.T) {
	rw := httptest.NewRecorder()
	rec := newRecorder(rw, &hookStub{capture: true}, 10, nil)
	payload := strings.Repeat("x", 25)
	for i := 0; i < len(payload); i += 5 {
		_, _ = rec.Write([]byte(payload[i : i+5]))
	}
	if rw.Body.String() != payload {
		t.Errorf("client got %d bytes, want %d", rw.Body.Len(), len(payload))
	}
	if _, ok := rec.body(); ok {
		t.Error("an oversized body must not be captured")
	}
	if rec.buf.Cap() != 0 {
		t.Error("the buffer must be released")
	}
}

func TestRecorderSkipsCaptureForLargeContentLength(t *testing.T) {
	rw := httptest.NewRecorder()
	rec := newRecorder(rw, &hookStub{capture: true}, 10, nil)
	rec.Header().Set("Content-Length", "11")
	rec.WriteHeader(200)
	if rec.capture {
		t.Error("capture must be off when Content-Length exceeds max")
	}
}

// With a Content-Length, the copy is allocated once: a body is not copied again and again as it grows.
func TestRecorderAllocatesItsCopyOnce(t *testing.T) {
	const size = 64 << 10
	rec := newRecorder(httptest.NewRecorder(), &hookStub{capture: true}, 1<<20, nil)
	rec.Header().Set("Content-Length", strconv.Itoa(size))
	rec.WriteHeader(200)
	allocated := rec.buf.Cap()
	if allocated < size {
		t.Fatalf("allocated = %d, want room for the %d bytes of Content-Length", allocated, size)
	}
	chunk := make([]byte, 1<<10)
	for i := 0; i < size/len(chunk); i++ {
		_, _ = rec.Write(chunk)
	}
	if body, ok := rec.body(); !ok || len(body) != size || rec.buf.Cap() != allocated {
		t.Errorf("body() = %d bytes, %v; capacity %d, was %d", len(body), ok, rec.buf.Cap(), allocated)
	}

	// Nothing is allocated from a Content-Length that is not a length.
	for _, length := range []string{"-1", "0", "many", "99999999999999999999"} {
		rec := newRecorder(httptest.NewRecorder(), &hookStub{capture: true}, 1<<20, nil)
		rec.Header().Set("Content-Length", length)
		rec.WriteHeader(200)
		if rec.buf.Cap() != 0 {
			t.Errorf("Content-Length %q: allocated %d bytes", length, rec.buf.Cap())
		}
	}
}

// §3.3: a body shorter than its Content-Length is not a complete response.
func TestRecorderIncompleteBody(t *testing.T) {
	tests := []struct {
		name          string
		contentLength string
		body          string
		want          bool
	}{
		{name: "§3.3 as long as its Content-Length", contentLength: "5", body: "hello", want: true},
		{name: "§3.3 shorter than its Content-Length", contentLength: "10", body: "hello"},
		{name: "§3.3 no body at all", contentLength: "10"},
		{name: "no Content-Length", body: "hello", want: true},
		{name: "empty body", contentLength: "0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			rec := newRecorder(rw, &hookStub{capture: true}, 1024, nil)
			if tt.contentLength != "" {
				rec.Header().Set("Content-Length", tt.contentLength)
			}
			rec.WriteHeader(200)
			if tt.body != "" {
				_, _ = rec.Write([]byte(tt.body))
			}
			body, ok := rec.body()
			if ok != tt.want || (ok && string(body) != tt.body) {
				t.Errorf("body() = %q, %v; want complete = %v", body, ok, tt.want)
			}
			if rw.Body.String() != tt.body {
				t.Errorf("client got %q, want %q", rw.Body.String(), tt.body)
			}
		})
	}
}

func TestRecorderPassesInformationalResponses(t *testing.T) {
	rw := &writerStub{header: http.Header{}}
	hook := &hookStub{capture: true}
	rec := newRecorder(rw, hook, 1024, nil)
	rec.WriteHeader(http.StatusEarlyHints)
	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusInternalServerError) // superfluous
	if len(rw.codes) != 2 || rw.codes[0] != 103 || rw.codes[1] != 200 {
		t.Errorf("codes = %v, want [103 200]", rw.codes)
	}
	if hook.calls != 1 || hook.status != 200 {
		t.Errorf("hook called %d times with %d", hook.calls, hook.status)
	}
}

// Under Yaegi v0.16.1, compiled code cannot see Flush on an interpreted ResponseWriter (see recorder); the method still flushes the
// compiled writer it wraps.
func TestRecorderPreservesFlusher(t *testing.T) {
	rw := httptest.NewRecorder()
	rec := newRecorder(rw, &hookStub{}, 1024, nil)
	var w any = rec
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("recorder must implement http.Flusher")
	}
	f.Flush()
	if !rw.Flushed || rw.Code != 200 {
		t.Errorf("flushed = %v, code = %d", rw.Flushed, rw.Code)
	}
}

// Compiled code cannot flush an interpreted writer under Yaegi v0.16.1: the recorder flushes by itself a response of unknown length, so
// that one that streams still does, kept for the store or not. A response with a Content-Length is never flushed: that would make a
// compress middleware before the cache compress the small ones. The client writer is a compiled one, as in Traefik.
func TestRecorderFlushes(t *testing.T) {
	tests := []struct {
		name          string
		contentLength string
		capture       bool
		want          bool
	}{
		{name: "unknown length: the header and every write", want: true},
		{name: "known length: nothing", contentLength: "10"},
		{name: "unknown length, copy kept: the header and every write", capture: true, want: true},
		{name: "known length, copy kept: nothing", contentLength: "10", capture: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			rec := newRecorder(rw, &hookStub{capture: tt.capture}, 1024, nil)
			if tt.contentLength != "" {
				rec.Header().Set("Content-Length", tt.contentLength)
			}
			rec.WriteHeader(http.StatusOK)
			if rw.Flushed != tt.want {
				t.Errorf("flushed with the header = %v, want %v", rw.Flushed, tt.want)
			}
			for i := 0; i < 2; i++ {
				rw.Flushed = false
				if n, err := rec.Write([]byte("hello")); n != 5 || err != nil {
					t.Fatalf("Write() = %d, %v", n, err)
				}
				if rw.Flushed != tt.want {
					t.Errorf("write %d flushed = %v, want %v", i, rw.Flushed, tt.want)
				}
			}
			if rw.Body.String() != "hellohello" {
				t.Errorf("client got %q", rw.Body.String())
			}
			if body, ok := rec.body(); ok != tt.capture || string(body) != rec.buf.String() {
				t.Errorf("body() = %q, %v", body, ok)
			}
		})
	}
}

// Nothing of a withheld response is sent: it is not flushed either.
func TestRecorderDoesNotFlushWithheldResponse(t *testing.T) {
	rw := httptest.NewRecorder()
	rec := newRecorder(rw, &hookStub{withhold: true}, 1024, nil)
	rec.WriteHeader(http.StatusServiceUnavailable)
	_, _ = rec.Write([]byte("unavailable"))
	rec.Flush()
	if rw.Flushed || rw.Body.Len() != 0 {
		t.Errorf("flushed = %v, body = %q", rw.Flushed, rw.Body.String())
	}
}

func TestRecorderPreservesHijacker(t *testing.T) {
	recCh := make(chan *recorder, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec := newRecorder(w, &hookStub{capture: true}, 1024, nil)
		recCh <- rec
		var rw http.ResponseWriter = rec
		hj, ok := rw.(http.Hijacker)
		if !ok {
			t.Error("recorder must implement http.Hijacker")
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
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hijacked" {
		t.Errorf("body = %q", body)
	}
	if _, ok := (<-recCh).body(); ok {
		t.Error("a hijacked response must not be captured")
	}
}

func TestRecorderPreservesTrailers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec := newRecorder(w, &hookStub{capture: true}, 1024, nil)
		rec.Header().Set("Trailer", "X-Checksum")
		_, _ = rec.Write([]byte("body"))
		rec.Header().Set("X-Checksum", "abc")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := resp.Trailer.Get("X-Checksum"); got != "abc" {
		t.Errorf("trailer = %q, want abc", got)
	}
}
