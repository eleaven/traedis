package traedis

import (
	"bufio"
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Fuzzers of what parses data the cache does not trust: the replies of Redis,
// what it stores, and the header fields of clients and backends. Their seeds
// run with the tests; `make fuzz` looks for more.

// FuzzReadReply: whatever Redis sends, a reply is refused or within bounds,
// and nothing is read past it.
func FuzzReadReply(f *testing.F) {
	for _, seed := range []string{
		"+OK\r\n", "-ERR unknown command\r\n", ":3\r\n", "*1\r\n$5\r\nhello\r\n", "*3\r\n$5\r\nentry\r\n$-1\r\n$0\r\n\r\n",
		"*1\r\n:1\r\n", "*0\r\n", "*-1\r\n", "*1\r\n$999999999999\r\n", "*999999999999\r\n", "*1\r\n*1\r\n$1\r\na\r\n",
		"$5\r\nhello\r\n", "?what\r\n", "*1\n$5\nhello\n", "*2\r\n$1\r\na\r\n", "",
	} {
		f.Add([]byte(seed))
	}
	const maxBulk = 64
	f.Fuzz(func(t *testing.T, data []byte) {
		source := bytes.NewReader(data)
		r, err := readReply(bufio.NewReaderSize(source, maxLineBytes), maxBulk)
		if err != nil {
			return
		}
		if strings.IndexByte("+-:*", r.kind) < 0 {
			t.Fatalf("kind = %q", r.kind)
		}
		if len(r.items) > maxArrayItems || (r.kind != '*' && r.items != nil) {
			t.Fatalf("%d items in a %q reply", len(r.items), r.kind)
		}
		for _, item := range r.items {
			if len(item) > maxBulk {
				t.Fatalf("item of %d bytes, past the %d allowed", len(item), maxBulk)
			}
		}
	})
}

// FuzzDecodeEntry: whatever Redis holds, an entry is refused or one that can
// be stored again, and is from then on read as it was stored. Not always as
// it was first read: the standard library reads header fields that it writes
// otherwise (a name with a space, which it leaves out; a folded value, which
// it trims), and that our own entries never have.
func FuzzDecodeEntry(f *testing.F) {
	valid := encodeEntry(sampleEntry())
	f.Add(valid)
	f.Add(valid[:entryFixed])
	f.Add(valid[:len(valid)-1])
	f.Add(append(append([]byte{}, valid...), "X: y\r\n"...))
	f.Add([]byte(entryVersion + "garbage"))
	f.Add(encodeMarker(marker{names: []string{"Accept-Language"}}))
	f.Add([]byte{})
	const maxBody = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := decodeEntry(data, maxBody)
		if err != nil {
			return
		}
		if e.status < 100 || e.status > 599 || e.size < 0 || e.size > maxBody || e.initialAge < 0 || e.lifetime < -1 {
			t.Fatalf("entry out of bounds: %+v", e)
		}
		stored, err := decodeEntry(encodeEntry(e), maxBody)
		if err != nil {
			t.Fatalf("the entry is refused once encoded again: %v", err)
		}
		if stored.status != e.status || stored.size != e.size || stored.initialAge != e.initialAge || stored.lifetime != e.lifetime ||
			!stored.requestTime.Equal(e.requestTime) || !stored.responseTime.Equal(e.responseTime) {
			t.Fatalf("entry = %+v, encoded again = %+v", e, stored)
		}
		again, err := decodeEntry(encodeEntry(stored), maxBody)
		if err != nil || !reflect.DeepEqual(again.header, stored.header) || !reflect.DeepEqual(again.vary, stored.vary) {
			t.Fatalf("header = %q %q, encoded again = %q %q, %v", stored.header, stored.vary, again.header, again.vary, err)
		}
	})
}

// FuzzDecodeMarker: whatever Redis holds, a marker is refused or one that
// selects a variant for any request.
func FuzzDecodeMarker(f *testing.F) {
	for _, seed := range []string{
		"TRV1Accept-Language\n", "TRV1Accept-Encoding,Accept-Language\nbr,gzip,identity", "TRV1Accept-Encoding\nbr,compress",
		"TRV1Accept Language\n", "TRV1\n", "TRV2Accept-Language\n", "TRV", "",
	} {
		f.Add([]byte(seed), "br;q=1.0, gzip;q=0.8, *;q=0")
	}
	f.Fuzz(func(t *testing.T, data []byte, acceptEncoding string) {
		m, ok := decodeMarker(data)
		if !ok {
			return
		}
		for _, name := range m.names {
			if !isToken(name) {
				t.Fatalf("name %q is not a token", name)
			}
		}
		again, ok := decodeMarker(encodeMarker(m))
		if !ok || !reflect.DeepEqual(again, m) {
			t.Fatalf("marker = %+v, encoded again = %+v, %v", m, again, ok)
		}
		h := http.Header{"Accept-Encoding": {acceptEncoding}, "Accept-Language": {"fr"}}
		if field := m.field(h, &accepts{header: h}); field != "" && len(field) != 32 {
			t.Fatalf("field = %q", field)
		}
	})
}

// FuzzParseCacheControl: whatever a client or a backend sends, directives are
// within bounds and the fields they name are header names.
func FuzzParseCacheControl(f *testing.F) {
	for _, seed := range []string{
		"max-age=60", "public, max-age=60, s-maxage=\"120\"", "no-cache=\"Set-Cookie, X-A\", private=\"X-B\"", "max-age=99999999999999999999",
		"max-age=-1", "max-age=1, max-age=2", "max-stale", "max-stale=5, min-fresh=3, only-if-cached", "stale-while-revalidate=30, stale-if-error=x",
		"private=\"a,b", ",,=,\"\"", "no-cache=\"\"", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		for _, item := range splitList(value) {
			if item == "" || item != strings.TrimSpace(item) || !strings.Contains(value, item) {
				t.Fatalf("splitList(%q) has the item %q", value, item)
			}
		}
		cc := parseCacheControl([]string{value})
		for _, n := range []int64{cc.maxAge, cc.sMaxAge, cc.minFresh, cc.maxStale, cc.staleWhileRevalidate, cc.staleIfError} {
			if n < -1 || n > maxDeltaSeconds {
				t.Fatalf("parseCacheControl(%q): delta-seconds %d out of bounds", value, n)
			}
		}
		for _, name := range cc.unstored {
			if !isToken(name) || name != http.CanonicalHeaderKey(name) {
				t.Fatalf("parseCacheControl(%q): unstored field %q", value, name)
			}
		}
	})
}

// FuzzAcceptEncoding: whatever a client sends, it is only served codings the
// cache knows, and none that it refuses.
func FuzzAcceptEncoding(f *testing.F) {
	for _, seed := range []string{
		"gzip, br", "br;q=1.0, gzip;q=0.8, *;q=0.1", "*;q=0", "identity;q=0", "gzip;q=0, *", "zstd;q=0.000", "gzip;q=abc;q=0", ";;,,=", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		h := http.Header{"Accept-Encoding": {value}}
		listed := listedCodings(h)
		for _, coding := range acceptedCodings(h) {
			found := false
			for _, known := range contentCodings {
				found = found || known == coding
			}
			if !found {
				t.Fatalf("acceptedCodings(%q) has %q", value, coding)
			}
			if accepted, named := listed[coding]; named && !accepted {
				t.Fatalf("acceptedCodings(%q) has %q, which it refuses", value, coding)
			}
		}
	})
}
