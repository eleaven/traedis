package traedis

import (
	"net/http"
	"time"
)

// flight is a request forwarded for a key that the next requests for the key wait for, rather than being forwarded too (see collapse).
type flight struct {
	done chan struct{} // closed once its response is stored, or known not to be
	// pass is set, before done is closed, when nothing was stored: the requests that waited are forwarded at once, without waiting again.
	pass bool
}

// collapsible reports whether r, a GET or a HEAD that nothing stored could answer, may wait for the response to another request for its key:
// when it only missed, or found a stale response. A request whose own directives refused a fresh one (fwd=request) asks the backend, and
// only-if-cached is never forwarded.
func collapsible(reqCC cacheControl, fwd string) bool {
	return !reqCC.onlyIfCached && (fwd == "uri-miss" || fwd == "vary-miss" || fwd == "stale")
}

// leads reports whether the response to r may be stored for the requests that wait for it: a GET that does not say no-store, and that the
// backend answers in full. A conditional request goes to the backend as it is when the cache has no validator of its own to send in its place
// (§4.3.2): its 304 is not stored.
func leads(r *http.Request, reqCC cacheControl, stored *entry) bool {
	if r.Method != http.MethodGet || reqCC.noStore {
		return false
	}
	if !conditional(r.Header) {
		return true
	}
	return stored != nil && (stored.header.Get("ETag") != "" || stored.header.Get("Last-Modified") != "")
}

// collapse forwards r, a request that nothing stored could answer (see collapsible), unless a request for the same key is being forwarded:
// r then waits for its response to be stored, and is answered from the cache (RFC 9211 §2.6). A cold or expired URL that many clients
// request at once costs the backend one request, not one per client.
//
// The requests that wait are let go:
//   - once the response is stored: they look for it again, as any request does. One that it does not answer (another variant, a response
//     evicted in the meantime) waits for the next request forwarded, or is forwarded and others wait for it;
//   - as soon as its status and header tell that it will not be stored (private, no-store, 304…): they are forwarded at once, together, so
//     that a URL that is never stored is not served one request at a time;
//   - when the request fails or its client goes away, without anything stored: they are forwarded too;
//   - after collapseTimeout, whatever the request forwarded is doing: they are forwarded too, and don't wait again.
//
// Requests only wait for those of the same Traefik instance and middleware. They hold no body of the memory budget while they wait.
func (c *cache) collapse(
	w http.ResponseWriter, r *http.Request, key, field string, reqCC cacheControl, status cacheStatus, stored *entry,
) {
	deadline := time.Now().Add(c.cfg.collapseTimeout)
	for {
		f, leader := c.join(key, leads(r, reqCC, stored))
		if leader {
			c.lead(w, r, key, field, reqCC, status, stored, f)
			return
		}
		wait := time.Until(deadline)
		if f == nil || wait <= 0 {
			break
		}
		if stored != nil {
			// Read again if it is needed after all (see ready).
			letGo(c.bodies, stored)
			stored.body = nil
		}
		await(r.Context(), f.done, wait)
		landed, pass := c.landed(key, f)
		if !landed || pass || r.Context().Err() != nil {
			break
		}
		status.collapsed = true
		var done bool
		stored, field, status, done = c.lookup(w, r, key, reqCC, status, time.Until(deadline))
		if done {
			return
		}
	}
	c.forward(w, r, key, field, reqCC, status, true, stored, nil)
}

// lead forwards r for the requests that wait for f, and lets them go once it is done, even if it panics (see forward).
func (c *cache) lead(
	w http.ResponseWriter, r *http.Request, key, field string, reqCC cacheControl, status cacheStatus, stored *entry, f *flight,
) {
	wrote := false
	defer c.landAfter(key, f, &wrote)
	wrote = c.forward(w, r, key, field, reqCC, status, true, stored, f)
}

// landAfter lands f once lead is done: the requests that wait look for the response if one is being written. A pointer rather than a
// closure: see write.
func (c *cache) landAfter(key string, f *flight, wrote *bool) {
	c.land(key, f, !*wrote)
}

// join returns the flight of key, if a request for it is being forwarded. Otherwise, with lead, it starts one that r leads: leader is then
// set.
func (c *cache) join(key string, lead bool) (f *flight, leader bool) {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	if f = c.flights[key]; f != nil || !lead {
		return f, false
	}
	f = &flight{done: make(chan struct{})}
	c.flights[key] = f
	return f, true
}

// land lets go the requests that wait for f, once only: with pass, they are forwarded without looking for a response.
func (c *cache) land(key string, f *flight, pass bool) {
	c.flightMu.Lock()
	if c.flights[key] == f {
		delete(c.flights, key)
		f.pass = pass
		close(f.done)
	}
	c.flightMu.Unlock()
}

// landed reports whether f landed and, if so, whether it said to pass.
func (c *cache) landed(key string, f *flight) (landed, pass bool) {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	if c.flights[key] == f {
		return false, false
	}
	return true, f.pass
}
