package traedis

import (
	"context"
	"net/http"
	"os"
	"time"
)

// revalidate refreshes e, the entry of key served to r, the one of field, in
// the background (RFC 5861 §3). Background revalidations are bounded: one per
// key and maxRevalidations at once, each given revalidationTimeout. Past
// that, the next request for a stale entry tries again.
func (c *cache) revalidate(r *http.Request, key, field string, e *entry) {
	c.revalMu.Lock()
	busy := c.revalidating[key] || len(c.revalidating) >= c.cfg.maxRevalidations
	if !busy {
		c.revalidating[key] = true
	}
	c.revalMu.Unlock()
	if busy {
		return
	}

	// The cache's own request for the whole representation, cloned while r is
	// still ours: it keeps the headers that select the variant. It also keeps
	// the trace headers but not the request context:
	// Traefik keeps there the state of a request (access log fields…) that
	// must not be written to once its response is sent. Its server is kept:
	// without one, Traefik's proxy returns from a response that the backend
	// cut short as if it were complete (§3.3), instead of panicking.
	ctx := context.WithValue(context.Background(), http.ServerContextKey, r.Context().Value(http.ServerContextKey))
	req := r.Clone(ctx)
	req.Method = http.MethodGet
	req.Body = http.NoBody
	req.ContentLength = 0
	for _, name := range clientOnly {
		req.Header.Del(name)
	}
	// Its body stays with the request that is served: the background has no
	// use of it, and must not keep it in memory outside of the budget.
	bare := *e
	bare.body = nil
	bare.held = 0
	go c.refresh(req, key, field, &bare)
}

// refresh runs a background revalidation: a GET with the validators of the
// entry, if it has some (§4.3.1). A 304 refreshes the entry, any other response
// replaces it. A 404 or a 410 that is not stored deletes it; any other
// response leaves it in place, to be served stale for as long as it is allowed
// (see gone).
func (c *cache) refresh(req *http.Request, key, field string, e *entry) {
	defer c.revalidated(key)
	ctx, cancel := context.WithTimeout(req.Context(), c.cfg.revalidationTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	m := &missHook{c: c, req: req, reqCC: parseCacheControl(nil), key: key, field: field, requestTime: time.Now(), stored: e}
	m.store = true
	m.revalidation = true
	out, validating := backendRequest(req, e)
	m.validating = validating
	rec := newRecorder(&nullWriter{header: http.Header{}}, m, c.cfg.maxBodyBytes, c.bodies)
	defer rec.release()
	c.next.ServeHTTP(rec, out)
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	if m.validated != nil {
		// A 304 that does not update the entry leaves it as it is (§4.3.4).
		if m.updated {
			c.freshen(ctx, req, m.reqCC, key, field, m.validated)
		}
		return
	}
	if fields, ok := c.toStore(rec, m); ok {
		c.put(ctx, key, m.variant, fields, m.ttl)
	} else if m.gone {
		c.drop(ctx, key, field)
	}
}

// revalidated ends the revalidation of key. Nothing else recovers a panic of
// the backend in a goroutine of ours: it must not take Traefik down.
func (c *cache) revalidated(key string) {
	if p := recover(); p != nil {
		logf(os.Stderr, c.name, "panic during a background revalidation: %v", p)
	}
	c.revalMu.Lock()
	delete(c.revalidating, key)
	c.revalMu.Unlock()
}

// nullWriter is the client of a background revalidation: nobody.
type nullWriter struct {
	header http.Header
}

func (w *nullWriter) Header() http.Header         { return w.header }
func (w *nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *nullWriter) WriteHeader(int)             {}
