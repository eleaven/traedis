package traedis

import (
	"bufio"
	"context"
	"net"
	"net/http"
)

// safeMethod reports whether a method is safe (RFC 9110 §9.2.1). Any other
// one, known or not, may change the state of its target (§4.4).
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// invalidate proxies an unsafe request. A non-error response invalidates the
// stored responses of its target URI (§4.4): the whole URI key.
func (c *cache) invalidate(w http.ResponseWriter, r *http.Request) {
	key := redisKey(c.name, cacheURI(r, c.cfg.sortQuery))
	iw := &invalidator{
		rw:     w,
		c:      c,
		ctx:    context.WithoutCancel(r.Context()),
		key:    key,
		status: cacheStatus{fwd: "method"},
	}
	if c.cfg.exposeKey {
		iw.status.key = key
	}
	c.next.ServeHTTP(iw, r)
	if !iw.wroteHeader && !iw.hijacked {
		iw.WriteHeader(http.StatusOK)
	}
}

// invalidator is the writer of the response to an unsafe request. It deletes
// the URI key when the backend sends a 2xx or 3xx status (§4.4), before that
// status goes downstream: the client that changed a resource must not read
// what was stored before. A Redis failure leaves the stored responses as they
// are (fail open): they are served until they expire.
//
// It never holds back a response that streams. Yaegi v0.16.1 hides
// http.Flusher from compiled code, which cannot ask for a flush any more: the
// invalidator flushes by itself the header and every write of a response of
// unknown length, and leaves one with a Content-Length to the buffers of the
// server (see recorder). It preserves http.Hijacker and trailers (Header is
// the underlying map).
type invalidator struct {
	rw          http.ResponseWriter
	c           *cache
	ctx         context.Context
	key         string
	status      cacheStatus
	wroteHeader bool
	streams     bool // unknown length: flushed as it is written
	hijacked    bool
}

func (i *invalidator) Header() http.Header {
	return i.rw.Header()
}

func (i *invalidator) WriteHeader(code int) {
	if i.wroteHeader {
		return
	}
	// Informational responses (103 Early Hints…) pass through; 101 is final.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		i.rw.WriteHeader(code)
		return
	}
	i.wroteHeader = true
	i.status.fwdStatus = code
	if code >= 200 && code < 400 {
		i.status.detail = "invalidated"
		if err := i.c.unstore(i.ctx, i.key); err != nil {
			i.status.detail = "redis"
		}
	}
	h := i.rw.Header()
	h.Add("Cache-Status", i.status.String())
	i.streams = h.Get("Content-Length") == ""
	i.rw.WriteHeader(code)
	if i.streams {
		flush(i.rw)
	}
}

func (i *invalidator) Write(p []byte) (int, error) {
	if !i.wroteHeader {
		i.WriteHeader(http.StatusOK)
	}
	n, err := i.rw.Write(p)
	if i.streams {
		flush(i.rw)
	}
	return n, err
}

// Flush implements http.Flusher, for callers that can see it.
func (i *invalidator) Flush() {
	if !i.wroteHeader {
		i.WriteHeader(http.StatusOK)
	}
	flush(i.rw)
}

// Hijack implements http.Hijacker: the connection is no longer ours.
func (i *invalidator) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	i.hijacked = true
	if h, ok := i.rw.(http.Hijacker); ok {
		return h.Hijack()
	}
	return http.NewResponseController(i.rw).Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (i *invalidator) Unwrap() http.ResponseWriter {
	return i.rw
}
