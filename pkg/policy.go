package traedis

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxDeltaSeconds is the cap for delta-seconds values (RFC 9111 §1.2.2).
const maxDeltaSeconds = 2147483648

// cacheControl holds the Cache-Control directives the cache acts upon. Delta-seconds fields are -1 when absent.
type cacheControl struct {
	noStore bool
	noCache bool // unqualified form (§5.2.2.4)
	private bool // unqualified form (§5.2.2.7)
	// unstored lists the header fields named by the qualified forms of no-cache and private: the response is kept without them (§3.1).
	// Canonical names.
	unstored        []string
	public          bool
	mustRevalidate  bool
	mustUnderstand  bool
	proxyRevalidate bool
	onlyIfCached    bool
	maxAge          int64
	sMaxAge         int64
	minFresh        int64
	// maxStale is the staleness a request accepts (§5.2.1.2): maxDeltaSeconds when the directive has no value, which is any staleness.
	maxStale int64
	// RFC 5861 windows. They don't affect freshness: a malformed value is ignored and the first one wins.
	staleWhileRevalidate int64
	staleIfError         int64
	// invalid is set by a malformed or duplicated delta-seconds directive: such a response is considered stale (§4.2.1).
	invalid bool
}

func parseCacheControl(values []string) cacheControl {
	cc := cacheControl{maxAge: -1, sMaxAge: -1, minFresh: -1, maxStale: -1, staleWhileRevalidate: -1, staleIfError: -1}
	for _, line := range values {
		for _, d := range splitList(line) {
			name, value := d, ""
			if i := strings.IndexByte(d, '='); i >= 0 {
				name = d[:i]
				value = unquote(strings.TrimSpace(d[i+1:]))
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-store":
				cc.noStore = true
			case "no-cache":
				if cc.unqualified(value) {
					cc.noCache = true
				}
			case "private":
				if cc.unqualified(value) {
					cc.private = true
				}
			case "public":
				cc.public = true
			case "must-revalidate":
				cc.mustRevalidate = true
			case "must-understand":
				cc.mustUnderstand = true
			case "proxy-revalidate":
				cc.proxyRevalidate = true
			case "only-if-cached":
				cc.onlyIfCached = true
			case "max-age":
				cc.maxAge = cc.seconds(cc.maxAge, value)
			case "s-maxage":
				cc.sMaxAge = cc.seconds(cc.sMaxAge, value)
			case "min-fresh":
				cc.minFresh = cc.seconds(cc.minFresh, value)
			case "max-stale":
				if name == d && cc.maxStale < 0 {
					// Without value: a stale response of any age.
					cc.maxStale = maxDeltaSeconds
				} else {
					cc.maxStale = staleSeconds(cc.maxStale, value)
				}
			case "stale-while-revalidate":
				cc.staleWhileRevalidate = staleSeconds(cc.staleWhileRevalidate, value)
			case "stale-if-error":
				cc.staleIfError = staleSeconds(cc.staleIfError, value)
			}
		}
	}
	return cc
}

// unqualified handles the argument of a no-cache or private directive. The qualified form lists header fields, which are added to unstored:
// it then reports false. Without argument, or with one that is not a list of field names, the directive is the unqualified one, about the
// whole response.
func (cc *cacheControl) unqualified(value string) bool {
	if value == "" {
		return true
	}
	var names []string
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if !isToken(name) {
			return true
		}
		names = append(names, http.CanonicalHeaderKey(name))
	}
	cc.unstored = append(cc.unstored, names...)
	return false
}

// seconds parses a delta-seconds value. A duplicate or malformed value marks the directives invalid and keeps the current value.
func (cc *cacheControl) seconds(current int64, value string) int64 {
	n, ok := parseDeltaSeconds(value)
	if !ok || current >= 0 {
		cc.invalid = true
		return current
	}
	return n
}

// staleSeconds parses the delta-seconds of an RFC 5861 directive, keeping the current value when there is one or when the new one is
// malformed.
func staleSeconds(current int64, value string) int64 {
	n, ok := parseDeltaSeconds(value)
	if !ok || current >= 0 {
		return current
	}
	return n
}

func parseDeltaSeconds(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n < maxDeltaSeconds {
			n = n*10 + int64(c-'0')
		}
	}
	if n > maxDeltaSeconds {
		n = maxDeltaSeconds
	}
	return n, true
}

// splitList splits a field line holding a list (RFC 9110 §5.6.1) on commas outside quoted strings: Cache-Control directives, entity tags.
func splitList(s string) []string {
	if strings.IndexByte(s, '"') < 0 {
		// Nothing is quoted: the standard library splits it, which Yaegi runs much faster than a loop of ours.
		items := strings.Split(s, ",")
		out := items[:0]
		for _, d := range items {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			quoted = !quoted
		}
		if s[i] == ',' && !quoted {
			out = appendItem(out, s[start:i])
			start = i + 1
		}
	}
	return appendItem(out, s[start:])
}

func appendItem(out []string, d string) []string {
	if d = strings.TrimSpace(d); d != "" {
		out = append(out, d)
	}
	return out
}

// explicitFreshness returns the freshness lifetime set by the response (§4.2.1): s-maxage, then max-age, then Expires minus Date. ok is
// false when the response has no explicit freshness.
func explicitFreshness(h http.Header, cc cacheControl, responseTime time.Time) (time.Duration, bool) {
	if cc.invalid {
		return 0, true
	}
	if cc.sMaxAge >= 0 {
		return seconds(cc.sMaxAge), true
	}
	if cc.maxAge >= 0 {
		return seconds(cc.maxAge), true
	}
	expires := h.Values("Expires")
	if len(expires) == 0 {
		return 0, false
	}
	exp, err := http.ParseTime(expires[0])
	if err != nil || len(expires) > 1 {
		// Invalid or duplicated Expires: already expired (§5.3, §4.2.1).
		return 0, true
	}
	if d := exp.Sub(dateValue(h, responseTime)); d > 0 {
		return d, true
	}
	return 0, true
}

// heuristicallyCacheable lists the status codes that may get heuristic freshness (RFC 9111 §4.2.2, RFC 9110 §15.1). defaultTtl is our
// heuristic.
func heuristicallyCacheable(status int) bool {
	switch status {
	case 200, 203, 204, 206, 300, 301, 308, 404, 405, 410, 414, 501:
		return true
	}
	return false
}

// freshnessLifetime returns the lifetime of a stored entry: explicit freshness, or defaultTtl. Entries only get stored when defaultTtl was
// allowed for them.
func freshnessLifetime(e *entry, defaultTTL time.Duration) time.Duration {
	if e.lifetime >= 0 {
		return e.lifetime
	}
	if heuristicallyCacheable(e.status) {
		return defaultTTL
	}
	return 0
}

// understood lists the final status codes the cache knows the caching requirements of: those of RFC 9110 §15. A response with
// must-understand and any other status is not stored (§3, §5.2.2.3).
func understood(status int) bool {
	return (status >= 200 && status <= 206) || (status >= 300 && status <= 308 && status != 306) ||
		(status >= 400 && status <= 417) || status == 421 || status == 422 || status == 426 ||
		(status >= 500 && status <= 505)
}

func dateValue(h http.Header, responseTime time.Time) time.Time {
	if t, err := http.ParseTime(h.Get("Date")); err == nil {
		return t
	}
	return responseTime
}

// setFreshness sets what the age and the freshness lifetime of e are worked out from, once, rather than on every hit: from its header, its
// times and its Cache-Control (cc).
//
// Like currentAge, it is a function rather than a method of entry: under Yaegi, calling a method costs several times what calling a
// function does.
func setFreshness(e *entry, cc cacheControl) {
	e.initialAge = initialAge(e.header, e.requestTime, e.responseTime)
	e.lifetime = -1
	if d, ok := explicitFreshness(e.header, cc, e.responseTime); ok {
		e.lifetime = d
	}
}

// currentAge returns the age of e at the given time (§4.2.3). An entry stored by an instance whose clock is ahead of ours was not received
// in the future: its resident time is never negative.
func currentAge(e *entry, now time.Time) time.Duration {
	resident := now.Sub(e.responseTime)
	if resident < 0 {
		resident = 0
	}
	return e.initialAge + resident
}

// initialAge returns the corrected initial age of a response (§4.2.3): its age when it was received.
func initialAge(h http.Header, requestTime, responseTime time.Time) time.Duration {
	apparentAge := responseTime.Sub(dateValue(h, responseTime))
	if apparentAge < 0 {
		apparentAge = 0
	}
	// §5.1: the first member of a list is used, an invalid value is ignored.
	var ageValue time.Duration
	first, _, _ := strings.Cut(h.Get("Age"), ",")
	if n, ok := parseDeltaSeconds(strings.TrimSpace(first)); ok {
		ageValue = seconds(n)
	}
	correctedAgeValue := ageValue + responseTime.Sub(requestTime)
	if correctedAgeValue > apparentAge {
		return correctedAgeValue
	}
	return apparentAge
}

// storeTTL decides whether e, the response to req, may be stored and returns its Redis TTL: remaining freshness + staleTtl. RFC 9111 §3,
// §3.5 and the shared-cache rules of the README apply; statusCodes only narrows what may be stored. reqCC and cc are the Cache-Control of
// req and of e, which has its status, its header and its times: its freshness is set here (see setFreshness), once the decision comes to
// depend on it.
func storeTTL(req *http.Request, reqCC cacheControl, e *entry, cc cacheControl, cfg settings) (time.Duration, bool) {
	status := e.status
	h := e.header
	if req.Method != http.MethodGet || reqCC.noStore {
		return 0, false
	}
	// statusCodes has none of the codes that are never stored (see storableStatus).
	if !slices.Contains(cfg.statusCodes, status) || h.Get("Content-Range") != "" {
		return 0, false
	}

	if cc.noStore || cc.private {
		return 0, false
	}
	// A no-cache response is never served without being validated first (§5.2.2.4): the cache does not keep responses for that only.
	if cc.noCache {
		return 0, false
	}
	// no-store still wins over must-understand: ignoring it is only a SHOULD.
	if cc.mustUnderstand && !understood(status) {
		return 0, false
	}
	// §3.5: a response to an authenticated request is not shared unless it says it may be. authorizationHeaders tell who the client is too,
	// when a middleware before the cache replaced Authorization with them (basicAuth, forwardAuth) or when the backend reads an API key
	// there. Any line of these fields counts, not only the first: a client must not hide its credentials behind an empty one, which the
	// backend may skip.
	hasAuth := hasHeaderValue(req.Header, "Authorization") || hasAuthorization(req.Header, cfg.authorizationHeaders)
	if hasAuth && !cc.public && cc.sMaxAge < 0 && !cc.mustRevalidate {
		return 0, false
	}
	// A cookie is never shared: the response is only stored without its Set-Cookie, which takes the backend to ask for it (§3.1, §5.2.2.7)
	// with private="Set-Cookie" or no-cache="Set-Cookie".
	hasSetCookie := len(h.Values("Set-Cookie")) > 0
	if hasSetCookie && !slices.Contains(cc.unstored, "Set-Cookie") {
		return 0, false
	}
	// §4.1: a response is stored as the variant its Vary selects, if it has one.
	if !storableVary(h, cfg.maxVariants) {
		return 0, false
	}
	// Trailers are not part of stored entries.
	if hasHeaderValue(h, "Trailer") || hasTrailerPrefix(h) {
		return 0, false
	}

	setFreshness(e, cc)
	lifetime := e.lifetime
	if lifetime < 0 {
		// defaultTtl is heuristic freshness: never for personalized exchanges.
		if !heuristicallyCacheable(status) || cfg.defaultTTL <= 0 ||
			hasAuth || hasHeaderValue(req.Header, "Cookie") || hasSetCookie {
			return 0, false
		}
		lifetime = cfg.defaultTTL
	}

	remaining := lifetime - e.initialAge
	if remaining <= 0 {
		return 0, false
	}
	return remaining + cfg.staleTTL, true
}

// hasAuthorization reports whether the request header h has one of the authorizationHeaders, with a value on any of its lines.
func hasAuthorization(h http.Header, authorizationHeaders []string) bool {
	for _, name := range authorizationHeaders {
		if hasHeaderValue(h, name) {
			return true
		}
	}
	return false
}

// lookupAllowed reports whether the request lets the cache answer from storage (§5.2.1.4). cc is its Cache-Control.
func lookupAllowed(cc cacheControl) bool {
	// no-cache has no qualified form in a request: an argument is ignored.
	return !cc.noCache && len(cc.unstored) == 0
}

// servable reports whether a stored response with the given age and lifetime is fresh and satisfies the request's max-age and min-fresh
// (§4.2, §5.2.1).
func servable(reqCC cacheControl, age, lifetime time.Duration) bool {
	if age >= lifetime {
		return false
	}
	if reqCC.maxAge >= 0 && age > seconds(reqCC.maxAge) {
		return false
	}
	if reqCC.minFresh >= 0 && lifetime-age < seconds(reqCC.minFresh) {
		return false
	}
	return true
}

// staleAllowed reports whether a stored response may be served stale under an RFC 5861 directive, for its delta-seconds past the freshness
// lifetime. When the response does not carry the directive (-1), the configured fallback applies instead (§4.2.4: stale responses may also
// be permitted by configuration). Either way the window is capped at staleTtl. staleness is negative while the response is fresh. No
// directive of the response may forbid serving stale (§4.2.4); s-maxage implies proxy-revalidate (§5.2.2.10).
func staleAllowed(cc cacheControl, directive int64, fallback, staleness, staleTTL time.Duration) bool {
	if cc.noCache || cc.mustRevalidate || cc.proxyRevalidate || cc.sMaxAge >= 0 {
		return false
	}
	window := fallback
	if directive >= 0 {
		window = seconds(directive)
	}
	if window > staleTTL {
		window = staleTTL
	}
	return window > 0 && staleness < window
}

// servableWhileRevalidating reports whether a stale response may be served while it is revalidated in the background (RFC 5861 §3). A
// request with max-age or min-fresh does not want a stale response (§5.2.1.1, §5.2.1.3).
func servableWhileRevalidating(reqCC, cc cacheControl, staleness time.Duration, cfg settings) bool {
	return staleness >= 0 && reqCC.maxAge < 0 && reqCC.minFresh < 0 &&
		staleAllowed(cc, cc.staleWhileRevalidate, cfg.defaultStaleWhileRevalidate, staleness, cfg.staleTTL)
}

// servableOnRequest reports whether a stale response may be served because the request accepts it with max-stale (§5.2.1.2), within
// staleTtl. Its max-age still bounds the age of the response (§5.2.1.1), and min-fresh asks for the opposite. As for any stale response, no
// directive of the response may forbid it (§4.2.4).
func servableOnRequest(reqCC, cc cacheControl, age, lifetime, staleTTL time.Duration) bool {
	if reqCC.maxStale < 0 || reqCC.minFresh >= 0 || age < lifetime {
		return false
	}
	if reqCC.maxAge >= 0 && age > seconds(reqCC.maxAge) {
		return false
	}
	return staleAllowed(cc, reqCC.maxStale, 0, age-lifetime, staleTTL)
}

// servableOnError reports whether a stored response may replace a backend response with the given status (RFC 5861 §4). The request
// directives are not looked at: it applies "regardless of other freshness information", and to a response that is still fresh but too old
// for the client.
func servableOnError(status int, cc cacheControl, staleness time.Duration, cfg settings) bool {
	return staleIfErrorStatus(status) &&
		staleAllowed(cc, cc.staleIfError, cfg.defaultStaleIfError, staleness, cfg.staleTTL)
}

// staleIfErrorStatus lists the errors of RFC 5861 §4.
func staleIfErrorStatus(status int) bool {
	switch status {
	case 500, 502, 503, 504:
		return true
	}
	return false
}

// gone reports whether the status of the response to a background revalidation says that the resource is gone (404, 410): the stored
// response is then deleted, unless this one is stored in its place. Any other response that is not stored leaves it as it is, to be served
// stale for as long as it is allowed. The revalidation is the request of a client, with its header fields, and clients must not be able to
// evict entries: with its cookies, its Authorization or a header field of its own, it gets a response that may not be stored (private,
// Set-Cookie, an error…), which says nothing of the stored one.
func gone(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// clientOnly lists the request header fields that only concern the client's own copy of the response: a background revalidation fetches the
// whole representation, whatever the client asked for.
var clientOnly = []string{
	"Cache-Control", "Range", "If-Range",
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since",
}

// hopByHop lists header fields never stored nor replayed (RFC 9110 §7.6.1), those of a proxy included (§3.1). Content-Length is recomputed
// when serving.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authentication-Info",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
}

// headerToStore copies the response header without hop-by-hop fields and without the fields its Cache-Control (cc) forbids to store (§3.1):
// only the client that caused the miss gets those.
func headerToStore(h http.Header, cc cacheControl) http.Header {
	out := endToEnd(h)
	for _, name := range cc.unstored {
		out.Del(name)
	}
	return out
}

// endToEnd copies a response header without hop-by-hop fields.
func endToEnd(h http.Header) http.Header {
	out := h.Clone()
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		out.Del(name)
	}
	return out
}

func hasHeaderValue(h http.Header, name string) bool {
	for _, v := range h.Values(name) {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func hasTrailerPrefix(h http.Header) bool {
	for name := range h {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			return true
		}
	}
	return false
}
