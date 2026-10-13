package traedis

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// entryOverhead bounds an encoded entry, the header and metadata of a stored
// response: its body, stored apart, is bounded by maxBodyBytes.
const entryOverhead = 1 << 20

// cache is the middleware handler.
type cache struct {
	next http.Handler
	// name is the one Traefik gives the middleware ("cache@file"): part of its
	// Redis keys (see redisKey) and of its log lines.
	name    string
	cfg     settings
	store   store
	markers *markers

	revalMu      sync.Mutex
	revalidating map[string]bool // keys being revalidated, at most maxRevalidations

	writeMu sync.Mutex
	writing map[string]*pendingWrite // keys being written to (see later), at most redis.poolSize
}

// pendingWrite is a write to a key that is running.
type pendingWrite struct {
	done chan struct{} // closed once it has run
	// invalidated is set, under writeMu, when an unsafe request invalidated
	// the key in the meantime: what is written must not stay.
	invalidated bool
}

// New creates the middleware. It never contacts Redis: Traefik must start even
// when Redis is unreachable.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	// No value of ours is larger than a body and an entry.
	return newCache(next, name, cfg, newRedisClient(name, cfg.redis, cfg.maxBodyBytes+entryOverhead)), nil
}

func newCache(next http.Handler, name string, cfg settings, s store) *cache {
	return &cache{
		next: next, name: name, cfg: cfg, store: s, markers: &markers{},
		revalidating: map[string]bool{},
		writing:      map[string]*pendingWrite{},
	}
}

func (c *cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !safeMethod(r.Method) {
		c.invalidate(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		c.passThrough(w, r, "method")
		return
	}
	if expectsStream(r) {
		c.passThrough(w, r, "bypass")
		return
	}

	key := redisKey(c.name, cacheURI(r, c.cfg.sortQuery))
	status := cacheStatus{}
	if c.cfg.exposeKey {
		status.key = key
	}
	reqCC := parseCacheControl(r.Header.Values("Cache-Control"))

	if !lookupAllowed(r, reqCC) {
		status.fwd = "request"
		c.forward(w, r, key, "", reqCC, status, true, nil)
		return
	}

	// What the previous response for this URL had to store comes first (see
	// later). Read here, not by a method: see find.
	c.writeMu.Lock()
	pending := c.writing[key]
	c.writeMu.Unlock()
	if pending != nil {
		await(r.Context(), pending.done, c.cfg.redis.dialTimeout)
	}

	// The body is only read along with the entry when the answer is sure to
	// have it: neither a HEAD nor a possible 304 (see ready).
	withBody := r.Method == http.MethodGet && !conditional(r.Header)
	stored, field, miss, err := c.find(r.Context(), key, r.Header, withBody)
	if err != nil {
		// Fail open; don't try to write to a failing Redis.
		status.fwd = "bypass"
		status.detail = "redis"
		c.forward(w, r, key, "", reqCC, status, false, nil)
		return
	}
	status.fwd = miss
	if stored != nil {
		age := currentAge(stored, time.Now())
		lifetime := freshnessLifetime(stored, c.cfg.defaultTTL)
		// detail is how a stale response may be served, if it may.
		detail := ""
		usable := servable(reqCC, age, lifetime)
		if !usable {
			// Only a stale response has its Cache-Control parsed.
			cc := parseCacheControl(stored.header.Values("Cache-Control"))
			if servableWhileRevalidating(reqCC, cc, age-lifetime, c.cfg) {
				detail = "stale-while-revalidate"
			} else if servableOnRequest(reqCC, cc, age, lifetime, c.cfg.staleTTL) {
				detail = "max-stale"
			}
			usable = detail != ""
		}
		// Read with its body, the entry is ready.
		if usable && (withBody || c.ready(r, key, field, stored)) {
			if detail == "stale-while-revalidate" {
				c.revalidate(r, key, field, stored)
			}
			status.hit = true
			status.detail = detail
			c.serve(w, r, stored, age, lifetime, status)
			return
		}
		if usable {
			// The store no longer has its body: nothing stored can answer.
			stored = nil
		} else {
			// RFC 9211 §2.2: the response is stale, or fresh but not what the
			// request asks for (max-age, min-fresh).
			status.fwd = "stale"
			if age < lifetime {
				status.fwd = "request"
			}
		}
	}
	c.forward(w, r, key, field, reqCC, status, true, stored)
}

// find returns the stored response of key selected by the request header h
// (§4.1) and the field it is in, or none. miss is then why: uri-miss, or
// vary-miss when the key holds variants. An error is a Redis failure.
//
// The field "" of the key tells where the response is: it is the entry itself,
// or the marker that selects a variant. One round trip is enough when the
// marker is the one last seen there (see markers): the variant it selects is
// asked for along with it. A wrong guess takes a second one.
//
// With withBody, the body is read along with the entry. Otherwise it is left
// in the store, for ready to read if the answer turns out to have one.
func (c *cache) find(ctx context.Context, key string, h http.Header, withBody bool) (*entry, string, string, error) {
	a := &accepts{header: h}
	miss := "uri-miss"
	// Read here, not by a method of markers: under Yaegi, the call would cost
	// several times the lookup.
	c.markers.mu.Lock()
	known, varies := c.markers.known[key]
	c.markers.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		field := ""
		fields := []string{""}
		if varies {
			miss = "vary-miss"
			if field = known.marker.field(h, a); field != "" {
				fields = append(fields, field)
			}
		}
		if withBody && (!varies || field != "") {
			fields = append(fields, bodyField(field))
		}
		values, err := c.store.get(ctx, key, fields)
		if err != nil {
			return nil, field, miss, err
		}
		if values[0] == nil {
			// Nothing in the key: no response, and no marker any more.
			if varies {
				c.markers.set(key, knownMarker{}, false)
			}
			return nil, "", "uri-miss", nil
		}
		if !varies || string(values[0]) != known.raw {
			if mk, ok := decodeMarker(values[0]); ok || varies {
				// Wrong guess: the key has another marker, or one where it had
				// none, or none any more. (Two assignments: Yaegi v0.16.1 gets
				// the parallel one wrong.)
				known = knownMarker{raw: string(values[0]), marker: mk}
				varies = ok
				c.markers.set(key, known, varies)
				miss = "uri-miss"
				continue
			}
		}

		// The entry is in field "", or in the field of its own that the marker
		// selects, if any.
		at := 0
		if varies {
			at = 1
		}
		if at == len(fields) || values[at] == nil {
			return nil, field, miss, nil
		}
		e, err := decodeEntry(values[at], c.cfg.maxBodyBytes)
		if err != nil || !selects(e, h, a) {
			return nil, field, miss, nil
		}
		if withBody {
			if int64(len(values[at+1])) != e.size {
				return nil, field, miss, nil
			}
			e.body = values[at+1]
		}
		return e, field, miss, nil
	}
	// The key changed twice while it was read.
	return nil, "", miss, nil
}

// ready reports whether e, the entry of field, can answer r: its body is read
// from the store now, when find left it there and the answer has one. It is
// not when the store no longer has the entry as it was read: the body there
// may be the one of another response.
func (c *cache) ready(r *http.Request, key, field string, e *entry) bool {
	if e.body != nil || e.size == 0 || !sendsBody(r, e) {
		return true
	}
	values, err := c.store.get(r.Context(), key, []string{field, bodyField(field)})
	if err != nil || !bytes.Equal(values[0], e.raw) || int64(len(values[1])) != e.size {
		return false
	}
	e.body = values[1]
	return true
}

// passThrough proxies a request the cache has nothing to do with (OPTIONS,
// TRACE, upgrades, event streams), without wrapping the writer (Yaegi would
// hide http.Flusher from the backend). Cache-Status is set before the
// backend's own values.
func (c *cache) passThrough(w http.ResponseWriter, r *http.Request, fwd string) {
	w.Header().Add("Cache-Status", cacheStatus{fwd: fwd}.String())
	c.next.ServeHTTP(w, r)
}

// expectsStream reports requests whose responses must be flushed as they come:
// they are never wrapped (see passThrough).
func expectsStream(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(v), "text/event-stream") {
			return true
		}
	}
	return false
}

// serve answers from a stored entry (§4), fresh or stale: with a 304 when the
// client says it already has it (§4.3.2), with the bytes it asks for when it
// asks for some (see serveRange). The entry is ready for r (see ready).
func (c *cache) serve(w http.ResponseWriter, r *http.Request, e *entry, age, lifetime time.Duration, status cacheStatus) {
	h := w.Header()
	for name, values := range e.header {
		h[name] = append([]string(nil), values...)
	}
	if e.header.Get("Date") == "" {
		// RFC 9110 §6.6.1: a response received without Date gets the time it
		// was received.
		h.Set("Date", e.responseTime.UTC().Format(http.TimeFormat))
	}
	h.Set("Age", strconv.FormatInt(int64(age/time.Second), 10))
	status.ttl = lifetime - age
	h.Add("Cache-Status", status.String())
	if e.status == http.StatusOK && notModified(r.Header, e.header, e.responseTime) {
		notModifiedHeader(h)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if bodyAllowed(e.status) {
		h.Set("Content-Length", strconv.FormatInt(e.size, 10))
	}
	if len(r.Header["Range"]) > 0 && e.status == http.StatusOK && r.Method == http.MethodGet && wantsRange(r.Header, h) {
		serveRange(w, r, e)
		return
	}
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead && bodyAllowed(e.status) {
		_, _ = w.Write(e.body)
	}
}

func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

// sendsBody reports whether serve sends the body of e to answer r.
func sendsBody(r *http.Request, e *entry) bool {
	if r.Method == http.MethodHead || !bodyAllowed(e.status) {
		return false
	}
	return e.status != http.StatusOK || !notModified(r.Header, e.header, e.responseTime)
}

// forward proxies the request to the backend and stores the response when
// allowed. stored is the entry of field that could not be served, if any: the
// backend is asked whether it is still current (§4.3), and it answers in place
// of a backend error when stale-if-error allows it (RFC 5861 §4).
func (c *cache) forward(w http.ResponseWriter, r *http.Request, key, field string, reqCC cacheControl, status cacheStatus, mayStore bool, stored *entry) {
	if reqCC.onlyIfCached {
		// §5.2.1.7: no stored response may be used. This response is our own
		// and the request is not forwarded: no Cache-Status (RFC 9211 §2).
		w.WriteHeader(http.StatusGatewayTimeout)
		return
	}

	m := &missHook{c: c, req: r, reqCC: reqCC, key: key, field: field, status: status, requestTime: time.Now(), stored: stored}
	m.store = mayStore && r.Method == http.MethodGet
	m.head = mayStore && r.Method == http.MethodHead
	var before http.Header
	if stored != nil {
		// What the header holds now, without what the backend will add to it.
		before = w.Header().Clone()
	}
	// The request goes to the backend as it is, with its context and trace
	// headers, unless a stored response is to be validated: its validators
	// then take the place of the conditions of the client (§4.3.1).
	out, validating := backendRequest(r, stored)
	m.validating = validating
	rec := newRecorder(w, m, c.cfg.maxBodyBytes)
	c.next.ServeHTTP(rec, out)
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}

	if stored != nil && m.staleIfError {
		// Nothing of the backend's error was sent: the stored response replaces it.
		resetHeader(w.Header(), before)
		status.fwdStatus = rec.status
		status.served = true
		status.detail = "stale-if-error"
		c.serve(w, r, stored, m.age, m.lifetime, status)
		return
	}
	if m.validated != nil {
		// Nothing of the backend's 304 was sent: the stored response it
		// validated is (§4.3.3), with its updated fields if it gets some
		// (§4.3.4), and with what the 304 has for this client only.
		h := w.Header()
		resetHeader(h, before)
		for name, values := range m.own {
			h[name] = values
		}
		e := m.validated
		status.fwdStatus = rec.status
		status.served = true
		c.serve(w, r, e, currentAge(e, time.Now()), freshnessLifetime(e, c.cfg.defaultTTL), status)
		if m.updated {
			req := r.Clone(context.Background())
			c.later(key, func(ctx context.Context) { c.freshen(ctx, req, reqCC, key, field, e) })
		}
		return
	}
	if m.unusable {
		// The 304 was about another response than the stored one (§4.3.4), which
		// cannot answer: the backend is asked again, without its validators.
		resetHeader(w.Header(), before)
		c.forward(w, r, key, field, reqCC, status, mayStore, nil)
		return
	}
	if m.headHeader != nil {
		req := r.Clone(context.Background())
		c.later(key, func(ctx context.Context) { c.freshenWithHead(ctx, req, reqCC, key, field, stored, m) })
		return
	}
	if fields, ok := c.toStore(rec, m); ok {
		c.later(key, func(ctx context.Context) { c.put(ctx, key, m.variant, fields, m.ttl) })
	}
}

// later runs write, the store writes that the response to a request for key
// calls for, once the request is served: a response is not complete for its
// client until ServeHTTP returns, which must not wait for Redis. write only
// uses what is its own by then: neither the request nor its writer.
//
// The requests for key that follow wait for it (see ServeHTTP and unstore), so
// that a response is stored before the next one is looked for. Writes are
// bounded: one per key, and no more than Redis has connections. Past that,
// write is not run: a later response is stored instead.
func (c *cache) later(key string, write func(ctx context.Context)) {
	p := &pendingWrite{done: make(chan struct{})}
	c.writeMu.Lock()
	busy := c.writing[key] != nil || len(c.writing) >= c.cfg.redis.poolSize
	if !busy {
		c.writing[key] = p
	}
	c.writeMu.Unlock()
	if !busy {
		go c.write(key, p, write)
	}
}

func (c *cache) write(key string, p *pendingWrite, write func(ctx context.Context)) {
	defer c.written(key, p)
	write(context.Background())
}

// written ends the write to key. Nothing else recovers a panic in a goroutine
// of ours: it must not take Traefik down.
func (c *cache) written(key string, p *pendingWrite) {
	if r := recover(); r != nil {
		logf(os.Stderr, c.name, "panic while storing a response: %v", r)
	}
	c.writeMu.Lock()
	delete(c.writing, key)
	invalidated := p.invalidated
	c.writeMu.Unlock()
	if invalidated {
		_ = c.store.invalidate(context.Background(), key)
	}
	close(p.done)
}

// unstore removes key from the store (§4.4), after the write to it that is
// running, if any: were it to end later, it is told to remove the key too.
func (c *cache) unstore(ctx context.Context, key string) error {
	c.writeMu.Lock()
	pending := c.writing[key]
	if pending != nil {
		pending.invalidated = true
	}
	c.writeMu.Unlock()
	if pending != nil {
		await(ctx, pending.done, c.cfg.redis.dialTimeout)
	}
	return c.store.invalidate(ctx, key)
}

// await waits for done to be closed, no longer than timeout nor than ctx.
// Without a select statement: see redisClient.
func await(ctx context.Context, done chan struct{}, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reflect.Select([]reflect.SelectCase{
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(done)},
		{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
	})
}

// freshenWithHead applies the 200 response to a forwarded HEAD to the stored
// GET response that could have answered it (§4.3.5): stored, the entry of
// field, or the one the cache did not read because the request said no-cache.
// The same response gets its fields; another one makes it stale, which a
// fresh entry is made by deleting it. A response with another content coding
// is another variant: it says nothing of this one. Neither does a response
// without validator: some backends answer a HEAD with next to no header, which
// must not make a stale entry fresh.
func (c *cache) freshenWithHead(ctx context.Context, r *http.Request, reqCC cacheControl, key, field string, stored *entry, m *missHook) {
	h := m.headHeader
	if stored == nil {
		if lookupAllowed(r, reqCC) {
			return // the cache was read: it has no such response
		}
		e, f, _, err := c.find(ctx, key, r.Header, false)
		if err != nil || e == nil {
			return
		}
		stored = e
		field = f
	}
	if stored.header.Get("Content-Encoding") != h.Get("Content-Encoding") {
		return
	}
	if headMatches(stored, h) {
		if h.Get("ETag") == "" && h.Get("Last-Modified") == "" {
			return
		}
		// Stored again as the response to a GET, which it is.
		get := r.WithContext(ctx)
		get.Method = http.MethodGet
		c.freshen(ctx, get, reqCC, key, field, refreshed(stored, h, m.requestTime, m.responseTime))
		return
	}
	if currentAge(stored, m.responseTime) < freshnessLifetime(stored, c.cfg.defaultTTL) {
		c.drop(ctx, key, field)
	}
}

// resetHeader makes h what it was before the backend wrote to it.
func resetHeader(h, before http.Header) {
	for name := range h {
		delete(h, name)
	}
	for name, values := range before {
		h[name] = values
	}
}

// freshen stores e, the entry of field as a 304 or the response to a HEAD
// refreshed it (§4.3.4, §4.3.5), in its field: its body stays where it is, for
// as long. One that may no longer be stored is deleted instead, unless it is
// only refused because of the request (see supersedes).
func (c *cache) freshen(ctx context.Context, req *http.Request, reqCC cacheControl, key, field string, e *entry) {
	cc := parseCacheControl(e.header.Values("Cache-Control"))
	ttl, ok := storeTTL(req, reqCC, e, cc, c.cfg)
	if !ok {
		if supersedes(e, cc, c.cfg) {
			c.drop(ctx, key, field)
		}
		return
	}
	data := encodeEntry(e)
	if len(data) > entryOverhead {
		return
	}
	v := variant{}
	if field != "" {
		v = variantOf(req, e, field)
	}
	if c.put(ctx, key, v, []fieldValue{{name: field, value: data}}, ttl) {
		_ = c.store.expire(ctx, key, bodyField(field), ttl)
	}
}

// toStore returns the fields to store the recorded response in, when the hook
// allowed it and its body was fully captured: its entry and its body.
func (c *cache) toStore(rec *recorder, m *missHook) ([]fieldValue, bool) {
	body, complete := rec.body()
	if !complete || !m.store {
		return nil, false
	}
	e := m.entry
	e.size = int64(len(body))
	data := encodeEntry(e)
	if len(data) > entryOverhead {
		return nil, false
	}
	field := m.variant.field
	// The body goes to Redis as the recorder holds it: it is not copied.
	return []fieldValue{{name: field, value: data}, {name: bodyField(field), value: body}}, true
}

// put stores fields in key: what there is to write of the response that v
// tells where to store, along with the marker of the key when it is a variant.
// It reports whether Redis took them. Any Redis error ends it: nothing is
// written to a failing Redis.
func (c *cache) put(ctx context.Context, key string, v variant, fields []fieldValue, ttl time.Duration) bool {
	if v.field == "" {
		return c.store.set(ctx, key, fields, ttl, false) == nil
	}
	mk := marker{names: v.names}
	if v.coding != "" {
		// The marker lists the codings stored so far. Reading it and writing it
		// back is not atomic: a coding lost to a concurrent write is a miss for
		// the requests it would serve, whose response lists it again.
		mk.codings = []string{v.coding}
		values, err := c.store.get(ctx, key, []string{""})
		if err != nil {
			return false
		}
		if old, ok := decodeMarker(values[0]); ok && sameStrings(old.names, v.names) {
			mk.codings = v.listed(old.codings)
		}
	}
	n, err := c.store.count(ctx, key)
	if err != nil {
		return false
	}
	// A full key (the marker and the two fields of each of maxVariants
	// variants) only gets its variants replaced. The count is not atomic
	// either: concurrent writes may exceed the limit by a few fields.
	raw := encodeMarker(mk)
	fields = append([]fieldValue{{name: "", value: raw}}, fields...)
	if c.store.set(ctx, key, fields, ttl, n > 2*c.cfg.maxVariants) != nil {
		return false
	}
	c.markers.set(key, knownMarker{raw: string(raw), marker: mk}, true)
	return true
}

// drop removes the entry of field from key, with its body.
func (c *cache) drop(ctx context.Context, key, field string) {
	_ = c.store.del(ctx, key, []string{field, bodyField(field)})
}

// missHook decides, when the backend sends its final status, what becomes of
// the response (sent, stored, replaced by a stale entry), and adds Cache-Status.
type missHook struct {
	c            *cache
	req          *http.Request
	reqCC        cacheControl // Cache-Control of req
	key          string
	field        string // the one of stored
	status       cacheStatus
	store        bool
	requestTime  time.Time
	responseTime time.Time
	entry        *entry // response to store, without its body nor our Cache-Status
	ttl          time.Duration
	variant      variant // where to store it

	// stored is the entry that could not be served, if any.
	// Validation (§4.3): validating is set when the backend got its
	// validators. Its 304 gives validated, the entry to serve: with the
	// fields of the 304 when updated is set (§4.3.4), as it was otherwise.
	// own holds the fields of the 304 that are for this client only. A 304
	// about another response is unusable.
	stored     *entry
	validating bool
	validated  *entry
	updated    bool
	own        http.Header
	unusable   bool

	// head is set for a HEAD that may update the stored GET response
	// (§4.3.5): headHeader is then the header of its 200 response.
	head       bool
	headHeader http.Header

	// stale-if-error: once staleIfError is set, stored replaces the backend
	// error, with this age and freshness lifetime.
	staleIfError bool
	age          time.Duration
	lifetime     time.Duration

	// Background revalidation: superseded is set when the entry must not
	// outlive a response that ends up not being stored.
	revalidation bool
	superseded   bool
}

func (m *missHook) beforeHeader(code int, h http.Header) disposition {
	m.responseTime = time.Now()
	if m.validating && code == http.StatusNotModified {
		if current, update := validates(m.stored, h); current {
			e := m.stored
			if update {
				e = refreshed(m.stored, h, m.requestTime, m.responseTime)
			}
			// In the background, nobody is answered: the body is not needed.
			if m.revalidation || m.c.ready(m.req, m.key, m.field, e) {
				m.own = ownFields(m.stored, h)
				m.validated = e
				m.updated = update
				return withhold
			}
		}
		// Not the stored response, or its body is gone: in the background, it
		// is deleted.
		m.unusable = true
		m.superseded = m.revalidation
		return withhold
	}
	if m.stored != nil && !m.revalidation && staleIfErrorStatus(code) {
		cc := parseCacheControl(m.stored.header.Values("Cache-Control"))
		m.age = currentAge(m.stored, m.responseTime)
		m.lifetime = freshnessLifetime(m.stored, m.c.cfg.defaultTTL)
		if servableOnError(code, cc, m.age-m.lifetime, m.c.cfg) && m.c.ready(m.req, m.key, m.field, m.stored) {
			m.staleIfError = true
			return withhold
		}
	}
	if m.store {
		// The Cache-Control of the response is parsed once, for all it decides.
		cc := parseCacheControl(h.Values("Cache-Control"))
		e := &entry{status: code, header: h, requestTime: m.requestTime, responseTime: m.responseTime}
		ttl, ok := storeTTL(m.req, m.reqCC, e, cc, m.c.cfg)
		m.store = ok
		if ok {
			m.ttl = ttl
			e.header = headerToStore(h, cc)
			m.variant = newVariant(m.req, h)
			e.vary = m.variant.selected
			m.entry = e
		}
		if m.revalidation {
			// A storable response whose body turns out to be too large supersedes too.
			m.superseded = m.store || supersedes(e, cc, m.c.cfg)
		}
	}
	if m.head && code == http.StatusOK {
		m.headHeader = h.Clone()
	}
	m.status.fwdStatus = code
	h.Add("Cache-Status", m.status.String())
	if m.store {
		return keepCopy
	}
	return passOn
}
