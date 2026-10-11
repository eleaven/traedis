package traedis

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"strconv"
)

// disposition is what a headerHook wants done with a response.
type disposition int

const (
	passOn   disposition = iota // send it downstream
	keepCopy                    // send it downstream and keep a copy of the body
	withhold                    // send nothing downstream: the caller answers instead
)

// headerHook is told about the final status before it is sent downstream.
type headerHook interface {
	// beforeHeader may edit h and tells what to do with the response.
	beforeHeader(status int, h http.Header) disposition
}

// recorder streams a response to the client while keeping a copy of at most max
// body bytes. It never alters the stream: past max, it stops buffering. It
// preserves http.Flusher, http.Hijacker and trailers (Header is the underlying map).
// A withheld response is dropped instead: nothing of it is sent, but what the
// backend put in the header stays there.
//
// It never holds back a response that streams. Yaegi v0.16.1 hides
// http.Flusher from compiled code, which cannot ask for a flush any more: the
// recorder flushes by itself the header and every write of a response of
// unknown length. One with a Content-Length is left to the buffers of the
// server: flushing it would only make a compress middleware placed before the
// cache compress the responses it finds too small.
type recorder struct {
	rw          http.ResponseWriter
	hook        headerHook
	max         int64
	status      int
	wroteHeader bool
	capture     bool
	withheld    bool
	streams     bool // unknown length: flushed as it is written
	hijacked    bool
	buf         bytes.Buffer
}

func newRecorder(rw http.ResponseWriter, hook headerHook, max int64) *recorder {
	return &recorder{rw: rw, hook: hook, max: max}
}

func (r *recorder) Header() http.Header {
	return r.rw.Header()
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	// Informational responses (103 Early Hints…) pass through; 101 is final.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.rw.WriteHeader(code)
		return
	}
	r.wroteHeader = true
	r.status = code
	d := r.hook.beforeHeader(code, r.rw.Header())
	if d == withhold {
		r.withheld = true
		return
	}
	r.capture = d == keepCopy
	if cl := r.rw.Header().Get("Content-Length"); r.capture && cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err != nil || n > r.max {
			r.capture = false
		}
	}
	r.streams = r.rw.Header().Get("Content-Length") == ""
	r.rw.WriteHeader(code)
	if r.streams {
		flush(r.rw)
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.withheld {
		return len(p), nil
	}
	n, err := r.rw.Write(p)
	if r.streams {
		flush(r.rw)
	}
	if r.capture {
		if err != nil || int64(r.buf.Len()+n) > r.max {
			r.stopCapture()
		} else {
			r.buf.Write(p[:n])
		}
	}
	return n, err
}

func (r *recorder) stopCapture() {
	r.capture = false
	r.buf = bytes.Buffer{}
}

// Flush implements http.Flusher, for callers that can see it.
func (r *recorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.withheld {
		return
	}
	flush(r.rw)
}

// flush sends what was written to w to the client now.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return
	}
	_ = http.NewResponseController(w).Flush()
}

// Hijack implements http.Hijacker (WebSockets). A hijacked response is never stored.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	r.hijacked = true
	r.stopCapture()
	if h, ok := r.rw.(http.Hijacker); ok {
		return h.Hijack()
	}
	return http.NewResponseController(r.rw).Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.rw
}

// body returns the captured body, and false when the response was not fully
// captured (not wanted, too large, write error or hijacked) or is incomplete
// (§3.3): not as long as its Content-Length says.
func (r *recorder) body() ([]byte, bool) {
	if !r.capture || r.hijacked {
		return nil, false
	}
	if cl := r.rw.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(r.buf.Len()) {
		return nil, false
	}
	return r.buf.Bytes(), true
}
