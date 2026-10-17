package traedis

import (
	"context"
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
// stored responses of its target URI (§4.4): the whole URI key. The response
// goes through a recorder that copies nothing: it streams, and keeps
// http.Hijacker and trailers.
func (c *cache) invalidate(w http.ResponseWriter, r *http.Request) {
	key := redisKey(c.name, cacheURI(r, c.cfg.sortQuery))
	hook := &invalidateHook{c: c, ctx: context.WithoutCancel(r.Context()), key: key, status: cacheStatus{fwd: "method"}}
	if c.cfg.exposeKey {
		hook.status.key = key
	}
	rec := newRecorder(w, hook, 0, nil)
	c.next.ServeHTTP(rec, r)
	if !rec.wroteHeader && !rec.hijacked {
		rec.WriteHeader(http.StatusOK)
	}
}

// invalidateHook deletes the URI key when the backend sends a 2xx or 3xx
// status (§4.4), before that status goes downstream: the client that changed a
// resource must not read what was stored before. A Redis failure leaves the
// stored responses as they are (fail open): they are served until they expire.
type invalidateHook struct {
	c      *cache
	ctx    context.Context
	key    string
	status cacheStatus
}

func (i *invalidateHook) beforeHeader(code int, h http.Header) disposition {
	i.status.fwdStatus = code
	if code >= 200 && code < 400 {
		i.status.detail = "invalidated"
		if err := i.c.unstore(i.ctx, i.key); err != nil {
			i.status.detail = "redis"
		}
	}
	if i.c.cfg.exposeCacheStatus {
		h.Add("Cache-Status", i.status.String())
	}
	return passOn
}
