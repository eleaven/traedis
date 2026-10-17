package traedis

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVaryNames(t *testing.T) {
	tests := []struct {
		name   string
		vary   []string
		want   []string
		refuse bool
	}{
		{name: "no Vary"},
		{name: "empty Vary", vary: []string{" ", ","}},
		{name: "one name", vary: []string{"Accept-Encoding"}, want: []string{"Accept-Encoding"}},
		{name: "§4.1 names are case-insensitive, sorted and unique", vary: []string{"accept-language , ACCEPT-ENCODING", "Accept-Language"}, want: []string{"Accept-Encoding", "Accept-Language"}},
		{name: "§4.1 * never matches", vary: []string{"*"}, refuse: true},
		{name: "§4.1 * among names", vary: []string{"Accept-Encoding, *"}, refuse: true},
		{name: "Cookie is one variant per user", vary: []string{"Accept-Encoding, cookie"}, refuse: true},
		{name: "Authorization is one variant per user", vary: []string{"Authorization"}, refuse: true},
		{name: "not a token", vary: []string{"Accept Language"}, refuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := varyNames(http.Header{"Vary": tt.vary})
			if ok == tt.refuse || !sameStrings(got, tt.want) {
				t.Errorf("varyNames() = %q, %v; want %q, %v", got, ok, tt.want, !tt.refuse)
			}
		})
	}
}

func TestResponseCoding(t *testing.T) {
	tests := []struct {
		name     string
		encoding []string
		want     string // "" when the coding is not one a variant is stored by
	}{
		{name: "no Content-Encoding", want: "identity"},
		{name: "identity", encoding: []string{"identity"}, want: "identity"},
		{name: "gzip", encoding: []string{"gzip"}, want: "gzip"},
		{name: "RFC 9110 §8.4.1 codings are case-insensitive", encoding: []string{" BR "}, want: "br"},
		{name: "zstd", encoding: []string{"zstd"}, want: "zstd"},
		{name: "deflate", encoding: []string{"deflate"}, want: "deflate"},
		{name: "unknown coding", encoding: []string{"compress"}},
		{name: "several codings", encoding: []string{"gzip, br"}},
		{name: "several lines", encoding: []string{"gzip", "br"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := responseCoding(http.Header{"Content-Encoding": tt.encoding})
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("responseCoding() = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestAcceptedCodings(t *testing.T) {
	tests := []struct {
		name   string
		accept []string
		want   []string
	}{
		{name: "no Accept-Encoding"},
		{name: "empty Accept-Encoding", accept: []string{""}},
		{name: "a browser", accept: []string{"gzip, deflate, br, zstd"}, want: []string{"zstd", "br", "gzip", "deflate"}},
		{name: "order and case of the request don't matter", accept: []string{"BR,GZip"}, want: []string{"br", "gzip"}},
		{name: "several lines", accept: []string{"gzip", "br"}, want: []string{"br", "gzip"}},
		{name: "unknown codings are ignored", accept: []string{"compress, gzip, identity"}, want: []string{"gzip"}},
		{name: "RFC 9110 §12.4.2 q=0 refuses", accept: []string{"gzip;q=0, br;q=0.000, zstd; Q=0.5"}, want: []string{"zstd"}},
		{name: "a weight only matters when zero", accept: []string{"gzip;q=1.0, br;q=0.1"}, want: []string{"br", "gzip"}},
		{name: "RFC 9110 §12.5.3 * is every coding not listed", accept: []string{"*, gzip;q=0"}, want: []string{"zstd", "br", "deflate"}},
		{name: "RFC 9110 §12.5.3 *;q=0 refuses the others", accept: []string{"gzip, *;q=0"}, want: []string{"gzip"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acceptedCodings(http.Header{"Accept-Encoding": tt.accept}); !sameStrings(got, tt.want) {
				t.Errorf("acceptedCodings() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVariantField(t *testing.T) {
	names := []string{"Accept-Encoding", "Accept-Language"}
	fr := http.Header{"Accept-Language": {"fr"}, "Accept-Encoding": {"gzip, br"}}
	field := variantField(names, fr, "br")
	// The format is stable: changing it takes a new markerVersion.
	if want := hashHex("Accept-Encoding: br\nAccept-Language: fr\n"); field != want || len(field) != 32 {
		t.Errorf("variantField() = %q, want %q", field, want)
	}

	same := []struct {
		name   string
		header http.Header
	}{
		{name: "the request Accept-Encoding is not part of it", header: http.Header{"Accept-Language": {"fr"}, "Accept-Encoding": {"br"}}},
		{name: "§4.1 whitespace around the value", header: http.Header{"Accept-Language": {"  fr "}}},
		{name: "headers that are not listed by Vary", header: http.Header{"Accept-Language": {"fr"}, "User-Agent": {"curl"}}},
	}
	for _, tt := range same {
		t.Run(tt.name, func(t *testing.T) {
			if got := variantField(names, tt.header, "br"); got != field {
				t.Errorf("variantField() = %q, want the same field %q", got, field)
			}
		})
	}

	other := []struct {
		name   string
		header http.Header
		coding string
	}{
		{name: "another value", header: http.Header{"Accept-Language": {"en"}}, coding: "br"},
		{name: "another content coding", header: fr, coding: "gzip"},
		{name: "value case is kept", header: http.Header{"Accept-Language": {"FR"}}, coding: "br"},
		{name: "§4.1 an absent header is not an empty one", header: http.Header{}, coding: "br"},
		{name: "empty value", header: http.Header{"Accept-Language": {""}}, coding: "br"},
	}
	seen := map[string]string{field: "fr"}
	for _, tt := range other {
		t.Run(tt.name, func(t *testing.T) {
			got := variantField(names, tt.header, tt.coding)
			if previous, ok := seen[got]; ok {
				t.Errorf("variantField() is the field of %q", previous)
			}
			seen[got] = tt.name
		})
	}

	// §4.1: several lines are one combined value.
	lines := variantField(names, http.Header{"Accept-Language": {"fr", "en"}}, "br")
	if combined := variantField(names, http.Header{"Accept-Language": {"fr, en"}}, "br"); lines != combined {
		t.Error("§4.1 field lines must be combined")
	}
}

func TestMarker(t *testing.T) {
	m := marker{names: []string{"Accept-Encoding", "Accept-Language"}, codings: []string{"br", "identity"}}
	raw := encodeMarker(m)
	if string(raw) != "TRV1Accept-Encoding,Accept-Language\nbr,identity" {
		t.Errorf("encodeMarker() = %q", raw)
	}
	got, ok := decodeMarker(raw)
	if !ok || !reflect.DeepEqual(got, m) {
		t.Errorf("decodeMarker() = %+v, %v", got, ok)
	}

	plain := marker{names: []string{"Accept-Language"}}
	if got, ok := decodeMarker(encodeMarker(plain)); !ok || !reflect.DeepEqual(got, plain) {
		t.Errorf("decodeMarker() without codings = %+v, %v", got, ok)
	}
}

func TestDecodeMarkerUntrusted(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "nothing", raw: ""},
		{name: "an entry", raw: string(encodeEntry(&entry{status: 200, requestTime: t0, responseTime: t0}))},
		{name: "another version", raw: "TRV2Accept-Language\n"},
		{name: "truncated", raw: "TRV"},
		{name: "no codings line", raw: "TRV1Accept-Language"},
		{name: "no names", raw: "TRV1\n"},
		{name: "empty name", raw: "TRV1Accept-Language,\n"},
		{name: "name that is not a token", raw: "TRV1Accept Language\n"},
		{name: "unknown coding", raw: "TRV1Accept-Encoding\nbr,compress"},
		{name: "empty coding", raw: "TRV1Accept-Encoding\nbr,"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, ok := decodeMarker([]byte(tt.raw)); ok {
				t.Errorf("decodeMarker() = %+v, want it refused", m)
			}
		})
	}
}

func TestMarkerField(t *testing.T) {
	tests := []struct {
		name    string
		codings []string
		accept  string
		want    string // coding of the variant to read; "" when none can suit
	}{
		{name: "the stored coding the request accepts", codings: []string{"br", "gzip"}, accept: "gzip", want: "gzip"},
		{name: "the first of contentCodings when several suit", codings: []string{"br", "gzip"}, accept: "gzip, br", want: "br"},
		{name: "identity when no stored coding is accepted", codings: []string{"br", "identity"}, accept: "gzip", want: "identity"},
		{name: "identity last", codings: []string{"gzip", "identity"}, accept: "gzip", want: "gzip"},
		{name: "nothing suits", codings: []string{"br"}, accept: "gzip"},
		{name: "no Accept-Encoding only suits identity", codings: []string{"br", "gzip"}},
		{name: "no variant listed", accept: "gzip"},
	}
	names := []string{"Accept-Encoding"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.accept != "" {
				h.Set("Accept-Encoding", tt.accept)
			}
			want := ""
			if tt.want != "" {
				want = variantField(names, h, tt.want)
			}
			if got := (marker{names: names, codings: tt.codings}).field(h, &accepts{header: h}); got != want {
				t.Errorf("field() = %q, want the field of %q", got, tt.want)
			}
		})
	}

	// Without Accept-Encoding in Vary, the field only depends on the request.
	m := marker{names: []string{"Accept-Language"}}
	h := http.Header{"Accept-Language": {"fr"}}
	if got := m.field(h, &accepts{header: h}); got != variantField(m.names, h, "") {
		t.Errorf("field() = %q", got)
	}
}

func TestVariantListed(t *testing.T) {
	tests := []struct {
		name     string
		old      []string
		coding   string
		accepted []string
		want     []string
	}{
		{name: "first variant", coding: "br", accepted: []string{"br", "gzip"}, want: []string{"br"}},
		{name: "added in the order of contentCodings", old: []string{"gzip", "identity"}, coding: "br", accepted: []string{"br"}, want: []string{"br", "gzip", "identity"}},
		{name: "already listed", old: []string{"br", "gzip"}, coding: "gzip", accepted: []string{"gzip"}, want: []string{"br", "gzip"}},
		{name: "a coding the request does not accept is kept", old: []string{"zstd", "gzip"}, coding: "br", accepted: []string{"br", "gzip"}, want: []string{"zstd", "br", "gzip"}},
		{name: "an accepted coding picked before this one is dropped", old: []string{"zstd", "br", "gzip"}, coding: "gzip", accepted: []string{"zstd", "gzip"}, want: []string{"br", "gzip"}},
		{name: "an accepted coding picked after this one is kept", old: []string{"gzip", "identity"}, coding: "zstd", accepted: []string{"zstd", "gzip"}, want: []string{"zstd", "gzip", "identity"}},
		{name: "identity drops every accepted coding", old: []string{"zstd", "br", "gzip"}, coding: "identity", accepted: []string{"br", "gzip"}, want: []string{"zstd", "identity"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := variant{coding: tt.coding, accepted: tt.accepted}
			if got := v.listed(tt.old); !sameStrings(got, tt.want) {
				t.Errorf("listed() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelects(t *testing.T) {
	tests := []struct {
		name     string
		vary     string
		encoding string
		selected http.Header // request values stored with the entry
		reqHdr   http.Header
		want     bool
	}{
		{name: "no Vary", reqHdr: http.Header{"Accept-Language": {"fr"}}, want: true},
		{name: "§4.1 same value", vary: "Accept-Language", selected: http.Header{"Accept-Language": {"fr"}}, reqHdr: http.Header{"Accept-Language": {" fr"}}, want: true},
		{name: "§4.1 another value", vary: "Accept-Language", selected: http.Header{"Accept-Language": {"fr"}}, reqHdr: http.Header{"Accept-Language": {"en"}}},
		{name: "§4.1 absent in both", vary: "Accept-Language", selected: http.Header{}, reqHdr: http.Header{}, want: true},
		{name: "§4.1 absent in the request only", vary: "Accept-Language", selected: http.Header{"Accept-Language": {"fr"}}, reqHdr: http.Header{}},
		{name: "§4.1 absent in the stored request only", vary: "Accept-Language", selected: http.Header{}, reqHdr: http.Header{"Accept-Language": {""}}},
		{name: "§4.1 every name must match", vary: "Accept-Language, X-Device", selected: http.Header{"Accept-Language": {"fr"}, "X-Device": {"tv"}}, reqHdr: http.Header{"Accept-Language": {"fr"}, "X-Device": {"phone"}}},
		{name: "§4.1 * never matches", vary: "*", reqHdr: http.Header{}},
		{name: "coding the request accepts", vary: "Accept-Encoding", encoding: "br", selected: http.Header{"Accept-Encoding": {"br,gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"br"}}, want: true},
		{name: "coding the request does not accept", vary: "Accept-Encoding", encoding: "br", selected: http.Header{"Accept-Encoding": {"br,gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip"}}},
		{name: "coding refused by a zero weight", vary: "Accept-Encoding", encoding: "br", selected: http.Header{"Accept-Encoding": {"br"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip, br;q=0"}}},
		{name: "compressed without Accept-Encoding", vary: "Accept-Encoding", encoding: "gzip", selected: http.Header{"Accept-Encoding": {"gzip"}}, reqHdr: http.Header{}},
		{name: "coding the cache does not know", vary: "Accept-Encoding", encoding: "compress", selected: http.Header{"Accept-Encoding": {""}}, reqHdr: http.Header{"Accept-Encoding": {"compress"}}},
		{name: "identity: the request accepts the same codings", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"br,gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip, br"}}, want: true},
		{name: "identity: the request accepts fewer codings", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"br,gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip"}}, want: true},
		{name: "identity: the request accepts no coding", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"br,gzip"}}, reqHdr: http.Header{}, want: true},
		{name: "identity: the request accepts a coding the backend was not offered", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip, br"}}},
		{name: "identity stored for a request without Accept-Encoding", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {""}}, reqHdr: http.Header{"Accept-Encoding": {"gzip"}}},
		{name: "identity stored without its request codings", vary: "Accept-Encoding", selected: http.Header{}, reqHdr: http.Header{"Accept-Encoding": {"gzip"}}},
		{name: "RFC 9110 §12.5.3 identity refused", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip, identity;q=0"}}},
		{name: "RFC 9110 §12.5.3 identity refused by *;q=0", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip, *;q=0"}}},
		{name: "RFC 9110 §12.5.3 identity listed with *;q=0", vary: "Accept-Encoding", selected: http.Header{"Accept-Encoding": {"gzip"}}, reqHdr: http.Header{"Accept-Encoding": {"identity, *;q=0"}}, want: true},
		{name: "RFC 9110 §12.5.3 identity refused, coding accepted", vary: "Accept-Encoding", encoding: "br", selected: http.Header{"Accept-Encoding": {"br"}}, reqHdr: http.Header{"Accept-Encoding": {"br, identity;q=0"}}, want: true},
		{name: "coding and another header", vary: "Accept-Encoding, Accept-Language", encoding: "gzip", selected: http.Header{"Accept-Encoding": {"gzip"}, "Accept-Language": {"fr"}}, reqHdr: http.Header{"Accept-Encoding": {"gzip"}, "Accept-Language": {"en"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &entry{status: 200, header: http.Header{}, vary: tt.selected}
			if tt.vary != "" {
				e.header.Set("Vary", tt.vary)
			}
			if tt.encoding != "" {
				e.header.Set("Content-Encoding", tt.encoding)
			}
			if got := selects(e, tt.reqHdr, &accepts{header: tt.reqHdr}); got != tt.want {
				t.Errorf("selects() = %v, want %v", got, tt.want)
			}
		})
	}
}

// negotiator is an origin whose response depends on the request headers named
// by its Vary: it echoes Accept-Language and compresses with the first of its
// codings that the request accepts.
type negotiator struct {
	vary    string
	codings []string
	calls   int
	lastReq *http.Request
}

func (b *negotiator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.calls++
	b.lastReq = r
	h := w.Header()
	h.Set("Cache-Control", "max-age=60")
	if b.vary != "" {
		h.Set("Vary", b.vary)
	}
	body := "lang=" + r.Header.Get("Accept-Language")
	accepted := acceptedCodings(r.Header)
	for _, coding := range b.codings {
		if slices.Contains(accepted, coding) {
			h.Set("Content-Encoding", coding)
			body += " coding=" + coding
			break
		}
	}
	_, _ = io.WriteString(w, body)
}

const (
	varyURL  = "http://example.com/page"
	uriMiss  = "traedis; fwd=uri-miss; fwd-status=200"
	varyMiss = "traedis; fwd=vary-miss; fwd-status=200"
)

func isHit(status string) bool {
	return strings.HasPrefix(status, "traedis; hit; ttl=")
}

// storedMarker returns the marker of varyURL.
func storedMarker(t *testing.T, st *memStore) marker {
	t.Helper()
	v, _ := st.lookup(cacheKey(varyURL), "")
	m, ok := decodeMarker(v.value)
	if !ok {
		t.Fatalf("field \"\" must hold a marker, got %q", v.value)
	}
	return m
}

func TestVariants(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, st := newTestCache(t, b, nil)
	steps := []struct {
		name       string
		reqHdr     http.Header
		wantStatus string // "hit" for a hit
		wantBody   string
	}{
		{name: "first response of the URI", reqHdr: http.Header{"Accept-Language": {"fr"}}, wantStatus: uriMiss, wantBody: "lang=fr"},
		{name: "§4.1 same value", reqHdr: http.Header{"Accept-Language": {"fr"}}, wantStatus: "hit", wantBody: "lang=fr"},
		{name: "§4.1 another value", reqHdr: http.Header{"Accept-Language": {"en"}}, wantStatus: varyMiss, wantBody: "lang=en"},
		{name: "§4.1 each value has its variant", reqHdr: http.Header{"Accept-Language": {"en"}}, wantStatus: "hit", wantBody: "lang=en"},
		{name: "the first variant is still there", reqHdr: http.Header{"Accept-Language": {"fr"}}, wantStatus: "hit", wantBody: "lang=fr"},
		{name: "§4.1 absent header", reqHdr: http.Header{}, wantStatus: varyMiss, wantBody: "lang="},
		{name: "§4.1 absent header has its variant", reqHdr: http.Header{}, wantStatus: "hit", wantBody: "lang="},
		{name: "§4.1 whitespace is not part of the value", reqHdr: http.Header{"Accept-Language": {" fr "}}, wantStatus: "hit", wantBody: "lang=fr"},
		{name: "other request headers don't matter", reqHdr: http.Header{"Accept-Language": {"en"}, "Accept-Encoding": {"gzip"}, "User-Agent": {"curl"}}, wantStatus: "hit", wantBody: "lang=en"},
	}
	for _, step := range steps {
		rec := doRequest(c, http.MethodGet, varyURL, step.reqHdr)
		got := lastCacheStatus(rec)
		if step.wantStatus == "hit" && !isHit(got) || step.wantStatus != "hit" && got != step.wantStatus {
			t.Errorf("%s: Cache-Status = %q, want %q", step.name, got, step.wantStatus)
		}
		if rec.Body.String() != step.wantBody {
			t.Errorf("%s: body = %q, want %q", step.name, rec.Body.String(), step.wantBody)
		}
		if rec.Header().Get("Vary") != "Accept-Language" {
			t.Errorf("%s: Vary = %q: it must be sent as the backend did", step.name, rec.Header().Get("Vary"))
		}
	}
	if b.calls != 3 {
		t.Errorf("backend calls = %d, want 3", b.calls)
	}

	// One hash per URI: the marker in field "", two fields per variant (its
	// entry and its body).
	key := cacheKey(varyURL)
	if m := storedMarker(t, st); !sameStrings(m.names, []string{"Accept-Language"}) || len(m.codings) != 0 {
		t.Errorf("marker = %+v", m)
	}
	if n, _ := st.count(context.Background(), key); n != 7 {
		t.Errorf("fields = %d, want the marker and the 2 fields of 3 variants", n)
	}
	field := variantField([]string{"Accept-Language"}, http.Header{"Accept-Language": {"fr"}}, "")
	v, ok := st.lookup(key, field)
	if !ok {
		t.Fatal("variant not stored in its field")
	}
	if want := time.Minute + time.Hour; !closeTo(v.ttl, want) {
		t.Errorf("stored ttl = %v, want %v", v.ttl, want)
	}
	if mk, _ := st.lookup(key, ""); mk.ttl != v.ttl && !closeTo(mk.ttl, time.Minute+time.Hour) {
		t.Errorf("marker ttl = %v, want the one of its variant", mk.ttl)
	}
	if body, _ := st.lookup(key, bodyField(field)); body.ttl != v.ttl {
		t.Errorf("body ttl = %v, want the one of its entry (%v)", body.ttl, v.ttl)
	}
	e := st.entry(key, field)
	if e == nil {
		t.Fatal("the field of the variant must hold an entry")
	}
	if !reflect.DeepEqual(e.vary, http.Header{"Accept-Language": {"fr"}}) || string(e.body) != "lang=fr" {
		t.Errorf("stored entry: vary = %v, body = %q", e.vary, e.body)
	}
}

func TestHeadIsServedFromVariant(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, _ := newTestCache(t, b, nil)
	fr := http.Header{"Accept-Language": {"fr"}}
	doRequest(c, http.MethodGet, varyURL, fr)

	rec := doRequest(c, http.MethodHead, varyURL, fr)
	if b.calls != 1 || !isHit(lastCacheStatus(rec)) || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "7" {
		t.Errorf("HEAD: Cache-Status %q, Content-Length %q (backend calls %d)", lastCacheStatus(rec), rec.Header().Get("Content-Length"), b.calls)
	}
	rec = doRequest(c, http.MethodHead, varyURL, http.Header{"Accept-Language": {"en"}})
	if b.calls != 2 || lastCacheStatus(rec) != varyMiss {
		t.Errorf("HEAD of another variant: Cache-Status %q (backend calls %d)", lastCacheStatus(rec), b.calls)
	}
}

func TestOnlyIfCachedVaryMissIs504(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, _ := newTestCache(t, b, honourClientCacheControl)
	doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"fr"}})

	rec := doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"en"}, "Cache-Control": {"only-if-cached"}})
	if rec.Code != http.StatusGatewayTimeout || b.calls != 1 || lastCacheStatus(rec) != "" {
		t.Errorf("§5.2.1.7 got %d %q with %d backend calls", rec.Code, lastCacheStatus(rec), b.calls)
	}
}

// Variants of a response varying by Accept-Encoding are stored by content
// coding and shared by all the requests accepting it.
func TestVariantsByContentCoding(t *testing.T) {
	b := &negotiator{vary: "Accept-Encoding", codings: []string{"br", "gzip"}}
	c, st := newTestCache(t, b, nil)
	steps := []struct {
		name        string
		accept      string
		wantStatus  string // "hit" for a hit
		wantCoding  string
		wantCodings []string // listed by the marker afterwards
	}{
		{name: "a browser gets what the backend chose", accept: "gzip, deflate, br, zstd", wantStatus: uriMiss, wantCoding: "br", wantCodings: []string{"br"}},
		{name: "same request", accept: "gzip, deflate, br, zstd", wantStatus: "hit", wantCoding: "br", wantCodings: []string{"br"}},
		{name: "another Accept-Encoding accepting the stored coding", accept: "br", wantStatus: "hit", wantCoding: "br", wantCodings: []string{"br"}},
		{name: "a request that does not accept the stored coding", accept: "gzip", wantStatus: varyMiss, wantCoding: "gzip", wantCodings: []string{"br", "gzip"}},
		{name: "second coding stored", accept: "gzip", wantStatus: "hit", wantCoding: "gzip", wantCodings: []string{"br", "gzip"}},
		{name: "first of contentCodings among the accepted ones", accept: "gzip, br", wantStatus: "hit", wantCoding: "br", wantCodings: []string{"br", "gzip"}},
		{name: "q=0 refuses a coding", accept: "br;q=0, gzip", wantStatus: "hit", wantCoding: "gzip", wantCodings: []string{"br", "gzip"}},
		{name: "no Accept-Encoding never gets a compressed variant", wantStatus: varyMiss, wantCodings: []string{"br", "gzip", "identity"}},
		{name: "uncompressed variant stored", wantStatus: "hit", wantCodings: []string{"br", "gzip", "identity"}},
		{name: "compressed variants are picked before identity", accept: "zstd, gzip", wantStatus: "hit", wantCoding: "gzip", wantCodings: []string{"br", "gzip", "identity"}},
	}
	for _, step := range steps {
		h := http.Header{}
		if step.accept != "" {
			h.Set("Accept-Encoding", step.accept)
		}
		rec := doRequest(c, http.MethodGet, varyURL, h)
		got := lastCacheStatus(rec)
		if step.wantStatus == "hit" && !isHit(got) || step.wantStatus != "hit" && got != step.wantStatus {
			t.Errorf("%s: Cache-Status = %q, want %q", step.name, got, step.wantStatus)
		}
		if coding := rec.Header().Get("Content-Encoding"); coding != step.wantCoding {
			t.Errorf("%s: Content-Encoding = %q, want %q", step.name, coding, step.wantCoding)
		}
		if m := storedMarker(t, st); !sameStrings(m.codings, step.wantCodings) {
			t.Errorf("%s: marker codings = %q, want %q", step.name, m.codings, step.wantCodings)
		}
	}
	if b.calls != 3 {
		t.Errorf("backend calls = %d, want one per content coding", b.calls)
	}
	if n, _ := st.count(context.Background(), cacheKey(varyURL)); n != 7 {
		t.Errorf("fields = %d, want the marker and the 2 fields of 3 variants: clients cannot make variants", n)
	}
	if b.lastReq.Header.Get("Accept-Encoding") != "" {
		t.Error("the backend must get the request as the client sent it")
	}
}

// An uncompressed response is only served to the requests that accept no more
// codings than the one it was stored for.
func TestUncompressedVariant(t *testing.T) {
	gzipBr := http.Header{"Accept-Encoding": {"gzip, br"}}

	t.Run("a request without Accept-Encoding does not decide for the others", func(t *testing.T) {
		b := &negotiator{vary: "Accept-Encoding", codings: []string{"br"}}
		c, _ := newTestCache(t, b, nil)
		if rec := doRequest(c, http.MethodGet, varyURL, nil); rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
		}
		rec := doRequest(c, http.MethodGet, varyURL, gzipBr)
		if lastCacheStatus(rec) != varyMiss || rec.Header().Get("Content-Encoding") != "br" {
			t.Errorf("the backend may compress for this request: got %q, Content-Encoding %q", lastCacheStatus(rec), rec.Header().Get("Content-Encoding"))
		}
		rec = doRequest(c, http.MethodGet, varyURL, gzipBr)
		if !isHit(lastCacheStatus(rec)) || rec.Header().Get("Content-Encoding") != "br" || b.calls != 2 {
			t.Errorf("got %q, Content-Encoding %q (backend calls %d)", lastCacheStatus(rec), rec.Header().Get("Content-Encoding"), b.calls)
		}
		if rec := doRequest(c, http.MethodGet, varyURL, nil); !isHit(lastCacheStatus(rec)) || rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("the uncompressed variant must still be there: got %q", lastCacheStatus(rec))
		}
	})

	t.Run("a backend that does not compress", func(t *testing.T) {
		b := &negotiator{vary: "Accept-Encoding"}
		c, st := newTestCache(t, b, nil)
		steps := []struct {
			accept string
			hit    bool
		}{
			{accept: ""},
			{accept: "", hit: true},
			{accept: "gzip, br"}, // more codings than the backend was offered: it is asked
			{accept: "gzip, br", hit: true},
			{accept: "br", hit: true},
			{accept: "", hit: true},
			{accept: "gzip;q=0, br", hit: true},
			{accept: "gzip, br, zstd"},
			{accept: "gzip, br, zstd", hit: true},
			{accept: "gzip, br", hit: true},
		}
		for i, step := range steps {
			h := http.Header{}
			if step.accept != "" {
				h.Set("Accept-Encoding", step.accept)
			}
			rec := doRequest(c, http.MethodGet, varyURL, h)
			if isHit(lastCacheStatus(rec)) != step.hit || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "lang=" {
				t.Errorf("step %d (Accept-Encoding %q): Cache-Status = %q, want hit = %v", i, step.accept, lastCacheStatus(rec), step.hit)
			}
		}
		if n, _ := st.count(context.Background(), cacheKey(varyURL)); n != 3 || b.calls != 3 {
			t.Errorf("fields = %d, backend calls = %d: want the marker and one variant, stored 3 times", n, b.calls)
		}
	})
}

// A coding that the backend no longer answers with must not hide the variant
// it answers with now.
func TestOutdatedCodingIsUnlisted(t *testing.T) {
	tests := []struct {
		name string
		// what happens to the zstd variant before the backend stops sending it
		outdate    func(t *testing.T, st *memStore, field string)
		wantStatus string
	}{
		{name: "variant expired in Redis", wantStatus: varyMiss, outdate: func(t *testing.T, st *memStore, field string) {
			if err := st.del(context.Background(), cacheKey(varyURL), []string{field, bodyField(field)}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stale variant", wantStatus: "traedis; fwd=stale; fwd-status=200", outdate: func(t *testing.T, st *memStore, field string) {
			at := time.Now().Add(-2 * time.Minute)
			e := &entry{
				status:      200,
				header:      http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}, "Content-Encoding": {"zstd"}},
				vary:        http.Header{"Accept-Encoding": {"zstd,br"}},
				requestTime: at, responseTime: at,
			}
			if err := st.put(cacheKey(varyURL), field, e, nil, time.Hour); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &negotiator{vary: "Accept-Encoding", codings: []string{"zstd", "br"}}
			c, st := newTestCache(t, b, nil)
			browser := http.Header{"Accept-Encoding": {"zstd, br"}}
			doRequest(c, http.MethodGet, varyURL, browser)
			doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Encoding": {"gzip"}}) // identity
			tt.outdate(t, st, variantField([]string{"Accept-Encoding"}, nil, "zstd"))
			b.codings = []string{"br"}

			rec := doRequest(c, http.MethodGet, varyURL, browser)
			if lastCacheStatus(rec) != tt.wantStatus || rec.Header().Get("Content-Encoding") != "br" {
				t.Errorf("got %q, Content-Encoding %q", lastCacheStatus(rec), rec.Header().Get("Content-Encoding"))
			}
			if m := storedMarker(t, st); !sameStrings(m.codings, []string{"br", "identity"}) {
				t.Errorf("marker codings = %q, want zstd dropped", m.codings)
			}
			rec = doRequest(c, http.MethodGet, varyURL, browser)
			if !isHit(lastCacheStatus(rec)) || rec.Header().Get("Content-Encoding") != "br" || b.calls != 3 {
				t.Errorf("the new variant must be served: got %q (backend calls %d)", lastCacheStatus(rec), b.calls)
			}
		})
	}
}

func TestVaryAcceptEncodingAndAnotherHeader(t *testing.T) {
	b := &negotiator{vary: "Accept-Language, Accept-Encoding", codings: []string{"gzip"}}
	c, st := newTestCache(t, b, nil)
	steps := []struct {
		lang, accept string
		hit          bool
		wantCoding   string
	}{
		{lang: "fr", accept: "gzip, br", wantCoding: "gzip"},
		{lang: "fr", accept: "gzip", hit: true, wantCoding: "gzip"},
		{lang: "en", accept: "gzip", wantCoding: "gzip"},
		{lang: "en", accept: "gzip, br", hit: true, wantCoding: "gzip"},
		{lang: "en", accept: "br"},
		{lang: "en", accept: "br", hit: true},
		{lang: "fr", accept: "br"},
	}
	for i, step := range steps {
		rec := doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {step.lang}, "Accept-Encoding": {step.accept}})
		want := "lang=" + step.lang
		if step.wantCoding != "" {
			want += " coding=" + step.wantCoding
		}
		if isHit(lastCacheStatus(rec)) != step.hit || rec.Body.String() != want || rec.Header().Get("Content-Encoding") != step.wantCoding {
			t.Errorf("step %d: got %q %q, want %q (hit = %v)", i, lastCacheStatus(rec), rec.Body.String(), want, step.hit)
		}
	}
	if m := storedMarker(t, st); !sameStrings(m.names, []string{"Accept-Encoding", "Accept-Language"}) || !sameStrings(m.codings, []string{"gzip", "identity"}) {
		t.Errorf("marker = %+v", m)
	}
}

func TestVariantsAreCapped(t *testing.T) {
	for _, limit := range []int{16, 1, 3} {
		t.Run("maxVariants "+strconv.Itoa(limit), func(t *testing.T) {
			b := &negotiator{vary: "Accept-Language"}
			c, st := newTestCache(t, b, func(cfg *Config) { cfg.MaxVariants = limit; honourClientCacheControl(cfg) })
			key := cacheKey(varyURL)
			lang := func(i int) http.Header {
				return http.Header{"Accept-Language": {"l" + strconv.Itoa(i)}}
			}
			for i := 0; i < limit; i++ {
				doRequest(c, http.MethodGet, varyURL, lang(i))
			}
			if n, _ := st.count(context.Background(), key); n != 2*limit+1 {
				t.Fatalf("fields = %d, want the marker and the 2 fields of %d variants", n, limit)
			}

			// A new variant is not stored, and evicts none.
			for i := 0; i < 2; i++ {
				rec := doRequest(c, http.MethodGet, varyURL, lang(limit))
				if lastCacheStatus(rec) != varyMiss || rec.Body.String() != "lang=l"+strconv.Itoa(limit) {
					t.Errorf("variant over the limit: got %q %q", lastCacheStatus(rec), rec.Body.String())
				}
			}
			if n, _ := st.count(context.Background(), key); n != 2*limit+1 || st.sets != limit {
				t.Errorf("fields = %d, sets = %d: nothing must be stored past the limit", n, st.sets)
			}
			for i := 0; i < limit; i++ {
				if rec := doRequest(c, http.MethodGet, varyURL, lang(i)); !isHit(lastCacheStatus(rec)) {
					t.Errorf("variant %d was evicted: %q", i, lastCacheStatus(rec))
				}
			}

			// A stored variant can still be replaced.
			b.vary = "accept-language" // same names: same marker, same fields
			refresh := lang(0)
			refresh.Set("Cache-Control", "no-cache")
			doRequest(c, http.MethodGet, varyURL, refresh)
			rec := doRequest(c, http.MethodGet, varyURL, lang(0))
			if st.sets != limit+1 || !isHit(lastCacheStatus(rec)) || rec.Header().Get("Vary") != "accept-language" {
				t.Errorf("a stored variant must be replaced in a full key: sets = %d, Vary = %q", st.sets, rec.Header().Get("Vary"))
			}
		})
	}
}

// Lowering maxVariants evicts nothing: the key takes no new variant until
// enough of its fields expire.
func TestMaxVariantsLowered(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, st := newTestCache(t, b, nil)
	lang := func(l string) http.Header {
		return http.Header{"Accept-Language": {l}}
	}
	for _, l := range []string{"fr", "en", "de"} {
		doRequest(c, http.MethodGet, varyURL, lang(l))
	}

	c.cfg.maxVariants = 2
	for _, l := range []string{"fr", "en", "de"} {
		if rec := doRequest(c, http.MethodGet, varyURL, lang(l)); !isHit(lastCacheStatus(rec)) {
			t.Errorf("variant %s was evicted: %q", l, lastCacheStatus(rec))
		}
	}
	doRequest(c, http.MethodGet, varyURL, lang("es"))
	if rec := doRequest(c, http.MethodGet, varyURL, lang("es")); lastCacheStatus(rec) != varyMiss || st.sets != 3 {
		t.Errorf("no new variant over the limit: got %q (sets %d)", lastCacheStatus(rec), st.sets)
	}
}

func TestMaxVariantsZero(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, st := newTestCache(t, b, func(cfg *Config) { cfg.MaxVariants = 0 })
	fr := http.Header{"Accept-Language": {"fr"}}
	for i := 0; i < 2; i++ {
		rec := doRequest(c, http.MethodGet, varyURL, fr)
		if lastCacheStatus(rec) != uriMiss || rec.Body.String() != "lang=fr" {
			t.Errorf("a response with Vary must not be stored: got %q %q", lastCacheStatus(rec), rec.Body.String())
		}
	}
	if st.sets != 0 {
		t.Error("nothing must be stored, not even a marker")
	}

	// Responses without Vary are not variants.
	b.vary = ""
	doRequest(c, http.MethodGet, varyURL, fr)
	if rec := doRequest(c, http.MethodGet, varyURL, nil); !isHit(lastCacheStatus(rec)) || st.sets != 1 {
		t.Errorf("a response without Vary must be stored: got %q (sets %d)", lastCacheStatus(rec), st.sets)
	}
}

func TestVaryNotStored(t *testing.T) {
	tests := []struct {
		name string
		vary string
	}{
		{name: "§4.1 Vary: *", vary: "*"},
		{name: "Vary: Cookie", vary: "Accept-Language, Cookie"},
		{name: "Vary: Authorization", vary: "authorization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &negotiator{vary: tt.vary}
			c, st := newTestCache(t, b, nil)
			for i := 0; i < 2; i++ {
				rec := doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"fr"}})
				if lastCacheStatus(rec) != uriMiss || rec.Body.String() != "lang=fr" {
					t.Errorf("got %q %q", lastCacheStatus(rec), rec.Body.String())
				}
			}
			if st.sets != 0 {
				t.Error("nothing must be stored, not even a marker")
			}
		})
	}
}

// The Vary of a URI may change: the last response tells where its variants are.
func TestVaryChanges(t *testing.T) {
	fr := http.Header{"Accept-Language": {"fr"}}
	en := http.Header{"Accept-Language": {"en"}}
	refresh := http.Header{"Accept-Language": {"fr"}, "Cache-Control": {"no-cache"}}

	t.Run("a response without Vary becomes varying", func(t *testing.T) {
		b := &negotiator{}
		c, st := newTestCache(t, b, honourClientCacheControl)
		doRequest(c, http.MethodGet, varyURL, fr)
		if rec := doRequest(c, http.MethodGet, varyURL, en); !isHit(lastCacheStatus(rec)) || rec.Body.String() != "lang=fr" {
			t.Fatalf("without Vary, the entry is served to all: got %q %q", lastCacheStatus(rec), rec.Body.String())
		}
		b.vary = "Accept-Language"
		doRequest(c, http.MethodGet, varyURL, refresh)
		storedMarker(t, st)
		if rec := doRequest(c, http.MethodGet, varyURL, en); lastCacheStatus(rec) != varyMiss || rec.Body.String() != "lang=en" {
			t.Errorf("got %q %q", lastCacheStatus(rec), rec.Body.String())
		}
		if rec := doRequest(c, http.MethodGet, varyURL, fr); !isHit(lastCacheStatus(rec)) || rec.Body.String() != "lang=fr" {
			t.Errorf("got %q %q", lastCacheStatus(rec), rec.Body.String())
		}
	})

	t.Run("a varying response loses its Vary", func(t *testing.T) {
		b := &negotiator{vary: "Accept-Language"}
		c, _ := newTestCache(t, b, honourClientCacheControl)
		doRequest(c, http.MethodGet, varyURL, fr)
		b.vary = ""
		doRequest(c, http.MethodGet, varyURL, refresh)
		if rec := doRequest(c, http.MethodGet, varyURL, en); !isHit(lastCacheStatus(rec)) || rec.Body.String() != "lang=fr" {
			t.Errorf("the entry of field \"\" must be served to all: got %q %q", lastCacheStatus(rec), rec.Body.String())
		}
	})

	t.Run("other Vary names", func(t *testing.T) {
		b := &negotiator{vary: "Accept-Language"}
		c, st := newTestCache(t, b, honourClientCacheControl)
		doRequest(c, http.MethodGet, varyURL, fr)
		b.vary = "Accept-Language, X-Device"
		doRequest(c, http.MethodGet, varyURL, refresh)
		if m := storedMarker(t, st); !sameStrings(m.names, []string{"Accept-Language", "X-Device"}) {
			t.Errorf("marker names = %q", m.names)
		}
		if rec := doRequest(c, http.MethodGet, varyURL, fr); !isHit(lastCacheStatus(rec)) || b.calls != 2 {
			t.Errorf("got %q (backend calls %d)", lastCacheStatus(rec), b.calls)
		}
		tv := http.Header{"Accept-Language": {"fr"}, "X-Device": {"tv"}}
		if rec := doRequest(c, http.MethodGet, varyURL, tv); lastCacheStatus(rec) != varyMiss {
			t.Errorf("got %q", lastCacheStatus(rec))
		}
	})
}

// Stored data is untrusted: what is not the variant of the request is a miss.
func TestUntrustedVariant(t *testing.T) {
	fr := http.Header{"Accept-Language": {"fr"}}
	key := cacheKey(varyURL)
	field := variantField([]string{"Accept-Language"}, fr, "")
	now := time.Now()
	tests := []struct {
		name       string
		tamper     func(st *memStore) error
		wantStatus string
	}{
		{name: "corrupt marker", wantStatus: uriMiss, tamper: func(st *memStore) error {
			return st.putRaw(key, "", []byte("TRV1\x00"))
		}},
		{name: "marker of another version", wantStatus: uriMiss, tamper: func(st *memStore) error {
			return st.putRaw(key, "", []byte("TRV0Accept-Language\n"))
		}},
		{name: "corrupt variant", wantStatus: varyMiss, tamper: func(st *memStore) error {
			return st.putRaw(key, field, []byte(entryVersion+"garbage"))
		}},
		{name: "§4.1 variant selected by other request values", wantStatus: varyMiss, tamper: func(st *memStore) error {
			e := &entry{
				status:      200,
				header:      http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}},
				body:        []byte("lang=en"),
				vary:        http.Header{"Accept-Language": {"en"}},
				requestTime: now, responseTime: now,
			}
			return st.put(key, field, e, nil, time.Hour)
		}},
		{name: "§4.1 variant with another Vary", wantStatus: varyMiss, tamper: func(st *memStore) error {
			e := &entry{
				status:      200,
				header:      http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language, X-Device"}},
				body:        []byte("lang=fr device=tv"),
				vary:        http.Header{"Accept-Language": {"fr"}, "X-Device": {"tv"}},
				requestTime: now, responseTime: now,
			}
			return st.put(key, field, e, nil, time.Hour)
		}},
		{name: "variant missing", wantStatus: varyMiss, tamper: func(st *memStore) error {
			return st.del(context.Background(), key, []string{field, bodyField(field)})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &negotiator{vary: "Accept-Language"}
			c, st := newTestCache(t, b, nil)
			doRequest(c, http.MethodGet, varyURL, fr)
			if err := tt.tamper(st); err != nil {
				t.Fatal(err)
			}
			rec := doRequest(c, http.MethodGet, varyURL, fr)
			if lastCacheStatus(rec) != tt.wantStatus || rec.Body.String() != "lang=fr" || b.calls != 2 {
				t.Errorf("got %q %q (backend calls %d), want %q", lastCacheStatus(rec), rec.Body.String(), b.calls, tt.wantStatus)
			}
			if rec := doRequest(c, http.MethodGet, varyURL, fr); !isHit(lastCacheStatus(rec)) {
				t.Errorf("the response must have been stored again: got %q", lastCacheStatus(rec))
			}
		})
	}
}

func TestFailOpenWhenVariantReadFails(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, st := newTestCache(t, b, nil)
	fr := http.Header{"Accept-Language": {"fr"}}
	doRequest(c, http.MethodGet, varyURL, fr)

	st.variantErr = errors.New("redis down")
	rec := doRequest(c, http.MethodGet, varyURL, fr)
	if rec.Code != 200 || rec.Body.String() != "lang=fr" || b.calls != 2 {
		t.Fatalf("fail open: got %d %q (backend calls %d)", rec.Code, rec.Body.String(), b.calls)
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=bypass; fwd-status=200; detail=redis" {
		t.Errorf("Cache-Status = %q", got)
	}
	if st.sets != 1 {
		t.Error("nothing must be written to a failing Redis")
	}
}

// failingStore fails the calls made to store a variant, one kind at a time.
type failingStore struct {
	*memStore
	failGet, failCount bool
}

func (s *failingStore) get(ctx context.Context, key string, fields []string) ([][]byte, error) {
	if s.failGet {
		return nil, errors.New("redis down")
	}
	return s.memStore.get(ctx, key, fields)
}

func (s *failingStore) count(ctx context.Context, key string) (int, error) {
	if s.failCount {
		return 0, errors.New("redis down")
	}
	return s.memStore.count(ctx, key)
}

func TestSaveVariantStopsOnRedisError(t *testing.T) {
	tests := []struct {
		name string
		fail func(s *failingStore)
	}{
		{name: "reading the marker", fail: func(s *failingStore) { s.failGet = true }},
		{name: "counting the fields", fail: func(s *failingStore) { s.failCount = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, mem := newTestCache(t, &negotiator{}, nil)
			st := &failingStore{memStore: mem}
			c.store = st
			tt.fail(st)

			v := variant{names: []string{"Accept-Encoding"}, field: "f", coding: "br", accepted: []string{"br"}}
			stored := c.put(context.Background(), cacheKey(varyURL), v, []fieldValue{{name: "f", value: []byte("data")}}, time.Minute)
			if stored || st.sets != 0 {
				t.Error("nothing must be written to a failing Redis")
			}
		})
	}
}

func TestStaleVariantIsRevalidated(t *testing.T) {
	b := &negotiator{vary: "Accept-Language, Accept-Encoding", codings: []string{"gzip"}}
	c, st := newTestCache(t, b, nil)
	names := []string{"Accept-Encoding", "Accept-Language"}
	key := cacheKey(varyURL)
	at := time.Now().Add(-70 * time.Second)
	stale := func(lang string) (string, *entry) {
		e := &entry{
			status:      200,
			header:      http.Header{"Cache-Control": {"max-age=60, stale-while-revalidate=30"}, "Vary": {b.vary}, "Content-Encoding": {"gzip"}},
			body:        []byte("old " + lang),
			vary:        http.Header{"Accept-Language": {lang}, "Accept-Encoding": {"gzip"}},
			requestTime: at, responseTime: at,
		}
		return variantField(names, http.Header{"Accept-Language": {lang}}, "gzip"), e
	}
	mk := encodeMarker(marker{names: names, codings: []string{"gzip"}})
	frField, frEntry := stale("fr")
	enField, enEntry := stale("en")
	for field, e := range map[string]*entry{frField: frEntry, enField: enEntry} {
		if err := st.put(key, field, e, mk, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	enData, _ := st.lookup(key, enField)

	req := httptest.NewRequest(http.MethodGet, varyURL, nil)
	req.Header.Set("Accept-Language", "fr")
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	settle(c)
	if rec.Body.String() != "old fr" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Fatalf("RFC 5861 §3 the stale variant must be served: got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	waitRevalidations(t, c)
	if b.calls != 1 || b.lastReq.Header.Get("Accept-Language") != "fr" || b.lastReq.Header.Get("Accept-Encoding") != "gzip, br" {
		t.Fatalf("the revalidation must select the same variant (backend calls %d): %v", b.calls, b.lastReq.Header)
	}

	rec = doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"fr"}, "Accept-Encoding": {"gzip"}})
	if b.calls != 1 || rec.Body.String() != "lang=fr coding=gzip" || !isHit(lastCacheStatus(rec)) || isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Errorf("after revalidation: %q %q (backend calls %d)", rec.Body.String(), lastCacheStatus(rec), b.calls)
	}
	// The other variant was left as it was.
	if v, _ := st.lookup(key, enField); string(v.value) != string(enData.value) {
		t.Error("the revalidation of a variant must not touch the others")
	}
	if body, _ := st.lookup(key, bodyField(enField)); string(body.value) != "old en" {
		t.Errorf("the revalidation of a variant must not touch the body of the others: %q", body.value)
	}
}

func TestSupersededVariantIsDeleted(t *testing.T) {
	b := &backend{header: http.Header{"Cache-Control": {"no-store"}}, body: "hello"}
	c, st := newTestCache(t, b, nil)
	names := []string{"Accept-Language"}
	key := cacheKey(varyURL)
	at := time.Now().Add(-70 * time.Second)
	mk := encodeMarker(marker{names: names})
	fields := map[string]string{}
	for _, lang := range []string{"fr", "en"} {
		e := &entry{
			status:      200,
			header:      http.Header{"Cache-Control": {"max-age=60, stale-while-revalidate=30"}, "Vary": {"Accept-Language"}},
			body:        []byte("old " + lang),
			vary:        http.Header{"Accept-Language": {lang}},
			requestTime: at, responseTime: at,
		}
		fields[lang] = variantField(names, http.Header{"Accept-Language": {lang}}, "")
		if err := st.put(key, fields[lang], e, mk, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	rec := doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"fr"}})
	if rec.Body.String() != "old fr" || !isStaleWhileRevalidate(lastCacheStatus(rec)) {
		t.Fatalf("got %q %q", rec.Body.String(), lastCacheStatus(rec))
	}
	waitRevalidations(t, c)
	if _, kept := st.lookup(key, fields["fr"]); kept {
		t.Error("the superseded variant must be deleted")
	}
	if _, kept := st.lookup(key, fields["en"]); !kept {
		t.Error("the other variant must be kept")
	}
	if _, kept := st.lookup(key, ""); !kept || st.dels != 1 {
		t.Errorf("the marker must be kept (dels %d)", st.dels)
	}
	if rec := doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"en"}}); rec.Body.String() != "old en" {
		t.Errorf("got %q", rec.Body.String())
	}
}

func TestStaleIfErrorServesTheVariant(t *testing.T) {
	b := &backend{status: 503, body: "error"}
	c, st := newTestCache(t, b, nil)
	names := []string{"Accept-Language"}
	at := time.Now().Add(-70 * time.Second)
	fr := http.Header{"Accept-Language": {"fr"}}
	e := &entry{
		status:      200,
		header:      http.Header{"Cache-Control": {"max-age=60, stale-if-error=30"}, "Vary": {"Accept-Language"}},
		body:        []byte("old fr"),
		vary:        fr,
		requestTime: at, responseTime: at,
	}
	mk := encodeMarker(marker{names: names})
	if err := st.put(cacheKey(varyURL), variantField(names, fr, ""), e, mk, time.Hour); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(c, http.MethodGet, varyURL, fr)
	if rec.Code != 200 || rec.Body.String() != "old fr" || !strings.HasSuffix(lastCacheStatus(rec), "; detail=stale-if-error") {
		t.Errorf("RFC 5861 §4 got %d %q %q", rec.Code, rec.Body.String(), lastCacheStatus(rec))
	}
	rec = doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"en"}})
	if rec.Code != 503 || lastCacheStatus(rec) != "traedis; fwd=vary-miss; fwd-status=503" {
		t.Errorf("another variant has nothing to be served instead: got %d %q", rec.Code, lastCacheStatus(rec))
	}
}

// One round trip per hit, variants included: the marker last seen in a key
// tells which variant to ask for along with it.
func TestVariantIsReadInOneRoundTrip(t *testing.T) {
	b := &negotiator{vary: "Accept-Language"}
	c, st := newTestCache(t, b, honourClientCacheControl)
	fr := http.Header{"Accept-Language": {"fr"}}
	key := cacheKey(varyURL)
	field := variantField([]string{"Accept-Language"}, fr, "")
	guessed := []string{"", field, bodyField(field)}
	unknown := []string{"", bodyField("")}
	hit := func(step string, c *cache, wantReads [][]string) {
		t.Helper()
		st.reads = nil
		rec := doRequest(c, http.MethodGet, varyURL, fr)
		if !isHit(lastCacheStatus(rec)) || rec.Body.String() != "lang=fr" {
			t.Errorf("%s: got %q %q, want a hit", step, lastCacheStatus(rec), rec.Body.String())
		}
		if !reflect.DeepEqual(st.reads, wantReads) {
			t.Errorf("%s: fields read = %q, want %q", step, st.reads, wantReads)
		}
	}

	doRequest(c, http.MethodGet, varyURL, fr)
	hit("the marker was just stored", c, [][]string{guessed})

	// Another Traefik instance, or this one restarted: the marker is learnt.
	other := newCache(b, "cache", c.cfg, st)
	hit("marker never seen", other, [][]string{unknown, guessed})
	hit("marker seen", other, [][]string{guessed})

	// The other instance stores a response with another Vary: another marker.
	b.vary = "Accept-Language, X-Device"
	doRequest(c, http.MethodGet, varyURL, http.Header{"Accept-Language": {"fr"}, "Cache-Control": {"no-cache"}})
	changed := variantField([]string{"Accept-Language", "X-Device"}, fr, "")
	hit("marker changed", other, [][]string{guessed, {"", changed, bodyField(changed)}})
	hit("marker seen again", other, [][]string{{"", changed, bodyField(changed)}})

	// The key is gone: nothing more is read, and the marker is forgotten.
	if err := st.invalidate(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	st.reads = nil
	e, _, miss, err := other.find(context.Background(), key, fr, true)
	if e != nil || miss != "uri-miss" || err != nil || len(st.reads) != 1 {
		t.Errorf("key gone: find() = %v, %q, %v after reading %q", e, miss, err, st.reads)
	}
	if _, known := other.markers.known[key]; known {
		t.Error("key gone: its marker must be forgotten")
	}
}

func TestMarkersAreBounded(t *testing.T) {
	m := &markers{}
	for i := 0; i <= maxMarkers; i++ {
		m.set("k"+strconv.Itoa(i), knownMarker{raw: "TRV1Accept-Language\n"}, true)
	}
	if len(m.known) == 0 || len(m.known) > maxMarkers {
		t.Errorf("markers kept = %d, want at most %d", len(m.known), maxMarkers)
	}
	last := "k" + strconv.Itoa(maxMarkers)
	if _, ok := m.known[last]; !ok {
		t.Error("the last marker seen must be kept")
	}
	m.set(last, knownMarker{}, false)
	if _, ok := m.known[last]; ok {
		t.Error("the marker of a key that no longer varies must be forgotten")
	}
}
