package traedis

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// markerVersion prefixes every marker. Bump it on any change of the marker or
// of the variant fields: a marker with an unknown version is a miss.
const markerVersion = "TRV1"

const (
	acceptEncoding = "Accept-Encoding"
	identity       = "identity"
)

// contentCodings lists the content codings a variant may be selected by. When
// a request accepts several of the stored ones, it is served the first.
var contentCodings = []string{"zstd", "br", "gzip", "deflate"}

// varyNames returns the header names listed by the Vary of a response:
// canonical, sorted, without duplicates, and none without Vary. ok is false
// when no variant can be stored for it: "*" (§4.1), a name that is not a token,
// and Cookie or Authorization, which make one variant per user.
func varyNames(h http.Header) ([]string, bool) {
	lines := h.Values("Vary")
	if len(lines) == 0 {
		return nil, true
	}
	var names []string
	for _, line := range lines {
		for _, name := range strings.Split(line, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if name == "*" || !isToken(name) {
				return nil, false
			}
			name = http.CanonicalHeaderKey(name)
			if name == "Cookie" || name == "Authorization" {
				return nil, false
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, true
}

// isToken reports whether s is a token (RFC 9110 §5.6.2).
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alphanumeric && strings.IndexByte("!#$%&'*+-.^_`|~", c) < 0 {
			return false
		}
	}
	return s != ""
}

// storableVary reports whether a response has a variant to be stored as (see
// varyNames), which takes a maxVariants that allows some. One that varies by
// Accept-Encoding is stored by its content coding, which must be one the
// cache knows.
func storableVary(h http.Header, maxVariants int) bool {
	names, ok := varyNames(h)
	if !ok || (len(names) > 0 && maxVariants == 0) {
		return false
	}
	if slices.Contains(names, acceptEncoding) {
		if _, ok := responseCoding(h); !ok {
			return false
		}
	}
	return true
}

// responseCoding returns the content coding of a response: one of
// contentCodings, or identity without Content-Encoding. ok is false for any
// other coding and for several of them: the cache cannot tell who accepts it.
func responseCoding(h http.Header) (string, bool) {
	coding := ""
	for _, v := range h.Values("Content-Encoding") {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if coding != "" {
			return "", false
		}
		coding = v
	}
	if coding == "" || coding == identity {
		return identity, true
	}
	if !slices.Contains(contentCodings, coding) {
		return "", false
	}
	return coding, true
}

// accepts is what the Accept-Encoding of a request says, worked out when a
// variant is first selected by it and once per request.
type accepts struct {
	header     http.Header // of the request
	parsed     bool
	codings    []string // see acceptedCodings
	noIdentity bool     // see identityRefused
}

// parseAccepts works out a, unless it is done already.
func parseAccepts(a *accepts) {
	if a.parsed {
		return
	}
	listed := listedCodings(a.header)
	a.codings = acceptedOf(listed)
	a.noIdentity = identityRefused(listed)
	a.parsed = true
}

// acceptedCodings returns the codings of contentCodings that a request accepts
// (RFC 9110 §12.5.3), in the same order. A weight only matters when it is zero:
// the coding is refused. A request without Accept-Encoding is taken as
// accepting none of them, as backends do.
func acceptedCodings(h http.Header) []string {
	return acceptedOf(listedCodings(h))
}

// acceptedOf returns the codings of contentCodings that listed accepts (see
// listedCodings).
func acceptedOf(listed map[string]bool) []string {
	var accepted []string
	for _, coding := range contentCodings {
		ok, found := listed[coding]
		if !found {
			ok = listed["*"]
		}
		if ok {
			accepted = append(accepted, coding)
		}
	}
	return accepted
}

// listedCodings returns the codings listed by the Accept-Encoding of a request
// ("*" is one of them), and whether each one is accepted.
func listedCodings(h http.Header) map[string]bool {
	listed := map[string]bool{}
	for _, line := range h.Values(acceptEncoding) {
		for _, item := range strings.Split(line, ",") {
			coding, params, _ := strings.Cut(item, ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "" {
				listed[coding] = !zeroWeight(params)
			}
		}
	}
	return listed
}

// identityRefused reports whether a request refuses an uncompressed response
// (RFC 9110 §12.5.3): "identity;q=0", or "*;q=0" when identity is not listed.
// listed is what its Accept-Encoding lists (see listedCodings).
func identityRefused(listed map[string]bool) bool {
	if ok, found := listed[identity]; found {
		return !ok
	}
	ok, found := listed["*"]
	return found && !ok
}

// zeroWeight reports whether the parameters of an Accept-Encoding item hold q=0.
func zeroWeight(params string) bool {
	for _, p := range strings.Split(params, ";") {
		name, value, _ := strings.Cut(p, "=")
		if strings.EqualFold(strings.TrimSpace(name), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return err == nil && q == 0
		}
	}
	return false
}

// requestValue returns the value of a request header as variants are compared
// by (§4.1): its lines trimmed and joined. ok is false when the request does
// not have the header, which is not the same as an empty value.
func requestValue(h http.Header, name string) (string, bool) {
	values := h.Values(name)
	if len(values) == 0 {
		return "", false
	}
	trimmed := make([]string, len(values))
	for i, v := range values {
		trimmed[i] = strings.TrimSpace(v)
	}
	value := strings.Join(trimmed, ", ")
	return value, true
}

// variantField returns the field of a variant: the hash of the Vary header
// names and of the values of the request header h they select. Accept-Encoding
// is the exception: its value is the content coding of the response, so that
// all the requests accepting a coding share its variant, and clients cannot
// make variants of their own.
func variantField(names []string, h http.Header, coding string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		if name == acceptEncoding {
			b.WriteString(": " + coding)
		} else if value, ok := requestValue(h, name); ok {
			b.WriteString(": " + value)
		}
		b.WriteByte('\n')
	}
	return hashHex(b.String())
}

// marker is what the field "" of a URI key holds when the responses of the URI
// vary: what it takes to find the field of the variant selected by a request.
//
// Encoding: "TRV1" | Vary header names, comma separated | "\n" | content
// codings, comma separated.
type marker struct {
	names []string // as returned by varyNames
	// codings holds the content codings of the stored variants, in the order of
	// contentCodings then identity, when names has Accept-Encoding.
	codings []string
}

func encodeMarker(m marker) []byte {
	return []byte(markerVersion + strings.Join(m.names, ",") + "\n" + strings.Join(m.codings, ","))
}

// decodeMarker parses a marker. ok is false for anything else: an entry,
// another version or corrupt data (stored data is untrusted).
func decodeMarker(b []byte) (marker, bool) {
	if len(b) < len(markerVersion) || string(b[:len(markerVersion)]) != markerVersion {
		return marker{}, false
	}
	names, codings, found := strings.Cut(string(b[len(markerVersion):]), "\n")
	if !found {
		return marker{}, false
	}
	m := marker{names: strings.Split(names, ",")}
	for _, name := range m.names {
		if !isToken(name) {
			return marker{}, false
		}
	}
	if codings != "" {
		m.codings = strings.Split(codings, ",")
	}
	for _, coding := range m.codings {
		if coding != identity && !slices.Contains(contentCodings, coding) {
			return marker{}, false
		}
	}
	return m, true
}

// field returns the field of the variant to serve a request with header h, or
// "" when no stored variant can suit it. a is what the request accepts.
func (m marker) field(h http.Header, a *accepts) string {
	coding := ""
	if slices.Contains(m.names, acceptEncoding) {
		parseAccepts(a)
		coding = m.pick(a.codings)
		if coding == "" {
			return ""
		}
	}
	return variantField(m.names, h, coding)
}

// pick returns the content coding of the variant to serve a request accepting
// the given codings: the first of contentCodings that is stored and accepted,
// then identity (selects tells whether that one suits the request).
func (m marker) pick(accepted []string) string {
	for _, coding := range contentCodings {
		if slices.Contains(accepted, coding) && slices.Contains(m.codings, coding) {
			return coding
		}
	}
	if slices.Contains(m.codings, identity) {
		return identity
	}
	return ""
}

// variant tells where a response is stored in its URI key, and what it was
// selected by.
type variant struct {
	names    []string    // Vary header names; none: the response is the entry of field ""
	field    string      // "" without names
	selected http.Header // request values of names, stored with the entry
	coding   string      // content coding of the response, when names has Accept-Encoding
	accepted []string    // codings accepted by the request, then
}

// newVariant returns the variant of a response with header h to req, which
// storableVary allowed.
func newVariant(req *http.Request, h http.Header) variant {
	names, _ := varyNames(h)
	v := variant{names: names, selected: http.Header{}}
	if len(names) == 0 {
		return v
	}
	for _, name := range names {
		if name == acceptEncoding {
			coding, _ := responseCoding(h)
			v.coding = coding
			v.accepted = acceptedCodings(req.Header)
			v.selected[name] = []string{strings.Join(v.accepted, ",")}
		} else if value, ok := requestValue(req.Header, name); ok {
			v.selected[name] = []string{value}
		}
	}
	v.field = variantField(names, req.Header, v.coding)
	return v
}

// variantOf returns the variant of e, a stored response read from field that
// is stored again for req: what it was selected by does not change.
func variantOf(req *http.Request, e *entry, field string) variant {
	names, _ := varyNames(e.header)
	v := variant{names: names, field: field, selected: e.vary}
	if slices.Contains(names, acceptEncoding) {
		coding, _ := responseCoding(e.header)
		v.coding = coding
		v.accepted = acceptedCodings(req.Header)
	}
	return v
}

// listed returns the codings of the marker once the variant is stored: those
// of the previous marker and its own. A coding that the request accepts and
// that pick would choose before the one the backend answered with is dropped:
// its variant is gone or outdated, and would hide this one from such requests.
func (v variant) listed(old []string) []string {
	var codings []string
	reached := false // past v.coding: what follows is picked after it
	for _, coding := range contentCodings {
		if coding == v.coding {
			reached = true
			codings = append(codings, coding)
		} else if slices.Contains(old, coding) && (reached || !slices.Contains(v.accepted, coding)) {
			codings = append(codings, coding)
		}
	}
	if v.coding == identity || slices.Contains(old, identity) {
		codings = append(codings, identity)
	}
	return codings
}

// selects reports whether a stored response may answer a request with header h
// (§4.1): the request headers named by its Vary are those of the request it was
// stored for. Accept-Encoding is compared by what it means instead (see suits):
// a is what the request accepts.
func selects(e *entry, h http.Header, a *accepts) bool {
	names, ok := varyNames(e.header)
	if !ok {
		return false
	}
	for _, name := range names {
		if name == acceptEncoding {
			if !suits(e, a) {
				return false
			}
			continue
		}
		stored, present := e.vary[name]
		value, ok := requestValue(h, name)
		if ok != present {
			return false
		}
		if ok && (len(stored) != 1 || stored[0] != value) {
			return false
		}
	}
	return true
}

// suits reports whether a stored response varying by Accept-Encoding may be
// sent to a request accepting a: its content coding is one of those the
// request accepts. An uncompressed response is acceptable to all but the
// requests refusing it (RFC 9110 §12.5.3), but only suits the requests
// accepting no other coding than the one it was stored for did: offered those,
// the backend did not compress. Any other request is forwarded, the backend
// may compress for it.
func suits(e *entry, a *accepts) bool {
	coding, ok := responseCoding(e.header)
	if !ok {
		return false
	}
	parseAccepts(a)
	if coding != identity {
		return slices.Contains(a.codings, coding)
	}
	if a.noIdentity {
		return false
	}
	offered := strings.Split(strings.Join(e.vary[acceptEncoding], ","), ",")
	for _, c := range a.codings {
		if !slices.Contains(offered, c) {
			return false
		}
	}
	return true
}

// maxMarkers bounds the markers kept in memory (see markers).
const maxMarkers = 4096

// markers remembers the marker last seen in the keys whose responses vary. It
// is only a guess of what Redis holds, which is what counts: it tells find
// which variant to ask for along with the marker, in one round trip instead of
// two. It is emptied when full.
type markers struct {
	mu    sync.Mutex // guards known, which find reads by itself
	known map[string]knownMarker
}

type knownMarker struct {
	raw    string // the encoded marker
	marker marker
}

// set remembers the marker of key, or forgets it when varies is false.
func (m *markers) set(key string, k knownMarker, varies bool) {
	m.mu.Lock()
	if !varies {
		delete(m.known, key)
	} else {
		if m.known == nil || len(m.known) >= maxMarkers {
			m.known = map[string]knownMarker{}
		}
		m.known[key] = k
	}
	m.mu.Unlock()
}
