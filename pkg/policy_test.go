package traedis

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func testSettings() settings {
	s, err := parseConfig(CreateConfig())
	if err != nil {
		panic(err)
	}
	return s
}

func TestParseCacheControl(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		check  func(cc cacheControl) bool
	}{
		{name: "directive names are case-insensitive", values: []string{"No-Store, MAX-AGE=60"}, check: func(cc cacheControl) bool { return cc.noStore && cc.maxAge == 60 }},
		{name: "directives across several lines", values: []string{"public", "s-maxage=10"}, check: func(cc cacheControl) bool { return cc.public && cc.sMaxAge == 10 }},
		{name: "§5.2.2.4 qualified no-cache lists fields that are not stored", values: []string{`no-cache="Set-Cookie, X-Foo", max-age=5`}, check: func(cc cacheControl) bool {
			return !cc.noCache && sameStrings(cc.unstored, []string{"Set-Cookie", "X-Foo"}) && cc.maxAge == 5 && !cc.invalid
		}},
		{name: "§5.2.2.7 qualified private lists fields that are not stored", values: []string{`private="Set-Cookie"`}, check: func(cc cacheControl) bool {
			return !cc.private && sameStrings(cc.unstored, []string{"Set-Cookie"})
		}},
		{name: "§5.2.2.7 field names are case-insensitive", values: []string{`private="set-cookie", no-cache="X-USER"`}, check: func(cc cacheControl) bool {
			return !cc.private && !cc.noCache && sameStrings(cc.unstored, []string{"Set-Cookie", "X-User"})
		}},
		{name: "§5.2.2.7 token form of the argument", values: []string{"private=Set-Cookie, max-age=5"}, check: func(cc cacheControl) bool {
			return !cc.private && sameStrings(cc.unstored, []string{"Set-Cookie"}) && cc.maxAge == 5
		}},
		{name: "§5.2.2.7 unqualified and qualified private", values: []string{`private, private="Set-Cookie"`}, check: func(cc cacheControl) bool { return cc.private }},
		{name: "§5.2.2.7 empty argument is the unqualified form", values: []string{`private=""`}, check: func(cc cacheControl) bool { return cc.private && len(cc.unstored) == 0 }},
		{name: "§5.2.2.4 argument that is not a list of field names is the unqualified form", values: []string{`no-cache="Set Cookie"`}, check: func(cc cacheControl) bool { return cc.noCache && len(cc.unstored) == 0 }},
		{name: "§5.2.2.4 empty field name is the unqualified form", values: []string{`no-cache="Set-Cookie,"`}, check: func(cc cacheControl) bool { return cc.noCache && len(cc.unstored) == 0 }},
		{name: "quoted delta-seconds accepted", values: []string{`max-age="30"`}, check: func(cc cacheControl) bool { return cc.maxAge == 30 }},
		{name: "§4.2.1 duplicate max-age is invalid", values: []string{"max-age=10", "max-age=20"}, check: func(cc cacheControl) bool { return cc.invalid && cc.maxAge == 10 }},
		{name: "§4.2.1 malformed max-age is invalid", values: []string{"max-age=-1"}, check: func(cc cacheControl) bool { return cc.invalid }},
		{name: "§1.2.2 delta-seconds overflow is capped", values: []string{"max-age=99999999999999999999"}, check: func(cc cacheControl) bool { return cc.maxAge == maxDeltaSeconds }},
		{name: "unknown directives are ignored", values: []string{"immutable, stale-while-revalidate=5, max-age=1"}, check: func(cc cacheControl) bool { return cc.maxAge == 1 && !cc.invalid }},
		{name: "absent values are -1", values: nil, check: func(cc cacheControl) bool {
			return cc.maxAge == -1 && cc.sMaxAge == -1 && cc.minFresh == -1 && cc.maxStale == -1 && cc.staleWhileRevalidate == -1 && cc.staleIfError == -1
		}},
		{name: "§5.2.1.2 max-stale with a value", values: []string{"max-stale=30"}, check: func(cc cacheControl) bool { return cc.maxStale == 30 && !cc.invalid }},
		{name: "§5.2.1.2 max-stale without value is any staleness", values: []string{"max-age=5, Max-Stale"}, check: func(cc cacheControl) bool { return cc.maxStale == maxDeltaSeconds && cc.maxAge == 5 }},
		{name: "§5.2.1.2 malformed max-stale is ignored", values: []string{"max-stale=soon"}, check: func(cc cacheControl) bool { return cc.maxStale == -1 && !cc.invalid }},
		{name: "§5.2.1.2 max-stale with an empty value is ignored", values: []string{"max-stale="}, check: func(cc cacheControl) bool { return cc.maxStale == -1 }},
		{name: "§5.2.1.2 first max-stale wins", values: []string{"max-stale=5, max-stale"}, check: func(cc cacheControl) bool { return cc.maxStale == 5 }},
		{name: "RFC 5861 stale-while-revalidate and stale-if-error", values: []string{"max-age=600, stale-while-revalidate=30, Stale-If-Error=1200"}, check: func(cc cacheControl) bool {
			return cc.staleWhileRevalidate == 30 && cc.staleIfError == 1200 && !cc.invalid
		}},
		{name: "RFC 5861 malformed window is ignored, freshness is kept", values: []string{"max-age=60, stale-if-error=soon"}, check: func(cc cacheControl) bool { return cc.staleIfError == -1 && cc.maxAge == 60 && !cc.invalid }},
		{name: "RFC 5861 first window wins", values: []string{"stale-while-revalidate=5, stale-while-revalidate=50"}, check: func(cc cacheControl) bool { return cc.staleWhileRevalidate == 5 && !cc.invalid }},
		{name: "§5.2.2.3 must-understand", values: []string{"Must-Understand, no-store"}, check: func(cc cacheControl) bool { return cc.mustUnderstand && cc.noStore }},
		{name: "§5.2.2.8 proxy-revalidate", values: []string{"proxy-revalidate"}, check: func(cc cacheControl) bool { return cc.proxyRevalidate && !cc.mustRevalidate }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if cc := parseCacheControl(tt.values); !tt.check(cc) {
				t.Errorf("parseCacheControl(%q) = %+v", tt.values, cc)
			}
		})
	}
}

func TestExplicitFreshness(t *testing.T) {
	date := t0.Format(http.TimeFormat)
	tests := []struct {
		name         string
		header       http.Header
		want         time.Duration
		wantExplicit bool
	}{
		{name: "§4.2.1 s-maxage wins over max-age", header: http.Header{"Cache-Control": {"max-age=10, s-maxage=20"}}, want: 20 * time.Second, wantExplicit: true},
		{name: "§4.2.1 max-age wins over Expires", header: http.Header{"Cache-Control": {"max-age=10"}, "Date": {date}, "Expires": {t0.Add(time.Hour).Format(http.TimeFormat)}}, want: 10 * time.Second, wantExplicit: true},
		{name: "§4.2.1 Expires minus Date", header: http.Header{"Date": {date}, "Expires": {t0.Add(time.Hour).Format(http.TimeFormat)}}, want: time.Hour, wantExplicit: true},
		{name: "§4.2.1 Expires without Date uses response time", header: http.Header{"Expires": {t0.Add(time.Minute).Format(http.TimeFormat)}}, want: time.Minute, wantExplicit: true},
		{name: "§5.3 invalid Expires means already expired", header: http.Header{"Expires": {"0"}}, want: 0, wantExplicit: true},
		{name: "§4.2.1 duplicate Expires is stale", header: http.Header{"Expires": {date, date}}, want: 0, wantExplicit: true},
		{name: "§4.2.1 Expires in the past", header: http.Header{"Date": {date}, "Expires": {t0.Add(-time.Hour).Format(http.TimeFormat)}}, want: 0, wantExplicit: true},
		{name: "§4.2.1 invalid max-age is stale", header: http.Header{"Cache-Control": {"max-age=abc"}}, want: 0, wantExplicit: true},
		{name: "no explicit freshness", header: http.Header{"Cache-Control": {"public"}}, want: 0, wantExplicit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := parseCacheControl(tt.header.Values("Cache-Control"))
			got, explicit := explicitFreshness(tt.header, cc, t0)
			if got != tt.want || explicit != tt.wantExplicit {
				t.Errorf("explicitFreshness() = %v, %v; want %v, %v", got, explicit, tt.want, tt.wantExplicit)
			}
		})
	}
}

func TestCurrentAge(t *testing.T) {
	tests := []struct {
		name                      string
		header                    http.Header
		requestTime, responseTime time.Time
		now                       time.Time
		want                      time.Duration
	}{
		{name: "§4.2.3 resident time", header: http.Header{"Date": {t0.Format(http.TimeFormat)}}, requestTime: t0, responseTime: t0, now: t0.Add(30 * time.Second), want: 30 * time.Second},
		{name: "§4.2.3 Age header is added", header: http.Header{"Date": {t0.Format(http.TimeFormat)}, "Age": {"100"}}, requestTime: t0, responseTime: t0, now: t0.Add(10 * time.Second), want: 110 * time.Second},
		{name: "§4.2.3 response delay is added to Age", header: http.Header{"Age": {"100"}}, requestTime: t0, responseTime: t0.Add(2 * time.Second), now: t0.Add(2 * time.Second), want: 102 * time.Second},
		{name: "§4.2.3 apparent age from an old Date", header: http.Header{"Date": {t0.Add(-time.Minute).Format(http.TimeFormat)}}, requestTime: t0, responseTime: t0, now: t0, want: time.Minute},
		{name: "§4.2.3 Date in the future is clamped", header: http.Header{"Date": {t0.Add(time.Hour).Format(http.TimeFormat)}}, requestTime: t0, responseTime: t0, now: t0, want: 0},
		{name: "stored by an instance whose clock is ahead", header: http.Header{"Age": {"100"}}, requestTime: t0, responseTime: t0, now: t0.Add(-5 * time.Second), want: 100 * time.Second},
		{name: "§5.1 invalid Age is ignored", header: http.Header{"Age": {"-5"}}, requestTime: t0, responseTime: t0, now: t0, want: 0},
		{name: "§5.1 first member of an Age list", header: http.Header{"Age": {"100, 5"}}, requestTime: t0, responseTime: t0, now: t0, want: 100 * time.Second},
		{name: "§5.1 invalid first member of an Age list", header: http.Header{"Age": {"x, 5"}}, requestTime: t0, responseTime: t0, now: t0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := parseCacheControl(tt.header.Values("Cache-Control"))
			e := &entry{status: 200, header: tt.header, requestTime: tt.requestTime, responseTime: tt.responseTime}
			setFreshness(e, cc)
			if got := currentAge(e, tt.now); got != tt.want {
				t.Errorf("currentAge() = %v, want %v", got, tt.want)
			}
			// What the age is worked out from is stored with the entry.
			stored, err := decodeEntry(encodeEntry(e), 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := currentAge(stored, tt.now); got != tt.want {
				t.Errorf("currentAge() of the stored entry = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoreTTL(t *testing.T) {
	date := t0.Format(http.TimeFormat)
	tests := []struct {
		name    string
		method  string
		reqHdr  http.Header
		status  int
		respHdr http.Header
		mutate  func(s *settings)
		want    time.Duration // 0 means not stored
	}{
		{name: "§3 explicit max-age stored with remaining freshness + staleTtl", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, want: time.Minute + time.Hour},
		{name: "TTL subtracts the initial age", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=300"}, "Age": {"250"}}, want: 50*time.Second + time.Hour},
		{name: "already stale on arrival is not stored", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=300"}, "Age": {"300"}}},
		{name: "§4.2.2 defaultTtl when no explicit freshness", status: 200, respHdr: http.Header{"Date": {date}}, want: 5*time.Minute + time.Hour},
		{name: "defaultTtl 0 means not stored", status: 200, respHdr: http.Header{}, mutate: func(s *settings) { s.defaultTTL = 0 }},
		{name: "§3 only GET is stored", method: http.MethodHead, status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "§5.2.1.5 request no-store", reqHdr: http.Header{"Cache-Control": {"no-store"}}, status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "§5.2.2.5 response no-store", status: 200, respHdr: http.Header{"Cache-Control": {"no-store, max-age=60"}}},
		{name: "§5.2.2.7 private", status: 200, respHdr: http.Header{"Cache-Control": {"private, max-age=60"}}},
		{name: "§5.2.2.4 no-cache not stored without revalidation", status: 200, respHdr: http.Header{"Cache-Control": {"no-cache, max-age=60"}}},
		{name: "§5.2.2.3 must-understand with a status the cache knows", status: 200, respHdr: http.Header{"Cache-Control": {"must-understand, max-age=60"}}, want: time.Minute + time.Hour},
		{name: "§5.2.2.3 must-understand with an unknown status", status: 299, respHdr: http.Header{"Cache-Control": {"must-understand, max-age=60"}}, mutate: func(s *settings) { s.statusCodes = []int{299} }},
		{name: "§3 unknown status without must-understand", status: 299, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.statusCodes = []int{299} }, want: time.Minute + time.Hour},
		{name: "§5.2.2.3 no-store still wins over must-understand", status: 200, respHdr: http.Header{"Cache-Control": {"must-understand, no-store, max-age=60"}}},
		{name: "statusCodes narrows: 404 not listed", status: 404, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "statusCodes narrows: 404 listed", status: 404, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.statusCodes = []int{200, 404} }, want: time.Minute + time.Hour},
		{name: "default statusCodes: no error stored", status: 500, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "default statusCodes: §4.2.2 defaultTtl for a 301", status: 301, respHdr: http.Header{"Location": {"/b"}}, want: 5*time.Minute + time.Hour},
		{name: "default statusCodes: §4.2.2 defaultTtl only for heuristically cacheable codes", status: 302, respHdr: http.Header{"Location": {"/b"}}},
		{name: "default statusCodes: §3 explicit freshness makes a 302 storable", status: 302, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Location": {"/b"}}, want: time.Minute + time.Hour},
		{name: "206 never stored", status: 206, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.statusCodes = []int{206} }},
		{name: "Content-Range never stored", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Content-Range": {"bytes 0-1/2"}}},
		{name: "304 never stored", status: 304, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.statusCodes = []int{304} }},
		{name: "§3.5 Authorization without permission", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "§3.5 Authorization with public", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 200, respHdr: http.Header{"Cache-Control": {"public, max-age=60"}}, want: time.Minute + time.Hour},
		{name: "§3.5 Authorization with s-maxage", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 200, respHdr: http.Header{"Cache-Control": {"s-maxage=60"}}, want: time.Minute + time.Hour},
		{name: "§3.5 Authorization with must-revalidate", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 200, respHdr: http.Header{"Cache-Control": {"must-revalidate, max-age=60"}}, want: time.Minute + time.Hour},
		{name: "no defaultTtl for Authorization even if public", reqHdr: http.Header{"Authorization": {"Bearer x"}}, status: 200, respHdr: http.Header{"Cache-Control": {"public"}}},
		{name: "no defaultTtl for requests with Cookie", reqHdr: http.Header{"Cookie": {"sid=1"}}, status: 200, respHdr: http.Header{}},
		{name: "explicit freshness for requests with Cookie", reqHdr: http.Header{"Cookie": {"sid=1"}}, status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, want: time.Minute + time.Hour},
		{name: "Set-Cookie", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Set-Cookie": {"sid=1"}}},
		{name: "Set-Cookie, even with public", status: 200, respHdr: http.Header{"Cache-Control": {"public, max-age=60"}, "Set-Cookie": {"sid=1"}}},
		{name: `§5.2.2.7 Set-Cookie with private="Set-Cookie"`, status: 200, respHdr: http.Header{"Cache-Control": {`max-age=60, private="Set-Cookie"`}, "Set-Cookie": {"sid=1"}}, want: time.Minute + time.Hour},
		{name: `§5.2.2.4 Set-Cookie with no-cache="Set-Cookie"`, status: 200, respHdr: http.Header{"Cache-Control": {`max-age=60, no-cache="Set-Cookie"`}, "Set-Cookie": {"sid=1"}}, want: time.Minute + time.Hour},
		{name: "§5.2.2.7 Set-Cookie with a qualified private naming another field", status: 200, respHdr: http.Header{"Cache-Control": {`max-age=60, private="X-User"`}, "Set-Cookie": {"sid=1"}}},
		{name: "§5.2.2.7 qualified private is not private", status: 200, respHdr: http.Header{"Cache-Control": {`max-age=60, private="X-User"`}, "X-User": {"bob"}}, want: time.Minute + time.Hour},
		{name: "§5.2.2.4 qualified no-cache is not no-cache", status: 200, respHdr: http.Header{"Cache-Control": {`max-age=60, no-cache="X-User"`}}, want: time.Minute + time.Hour},
		{name: `no defaultTtl for Set-Cookie, even with private="Set-Cookie"`, status: 200, respHdr: http.Header{"Cache-Control": {`private="Set-Cookie"`}, "Set-Cookie": {"sid=1"}}},
		{name: "§4.1 Vary", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}}, want: time.Minute + time.Hour},
		{name: "§4.1 Vary: *", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"*"}}},
		{name: "maxVariants 0: Vary not stored", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}}, mutate: func(s *settings) { s.maxVariants = 0 }},
		{name: "maxVariants 0: stored without Vary", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.maxVariants = 0 }, want: time.Minute + time.Hour},
		{name: "Vary: Cookie", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language, Cookie"}}},
		{name: "Vary: Authorization", status: 200, respHdr: http.Header{"Cache-Control": {"public, max-age=60"}, "Vary": {"Authorization"}}},
		{name: "Vary: Accept-Encoding without Content-Encoding", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}}, want: time.Minute + time.Hour},
		{name: "Vary: Accept-Encoding with a known coding", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}, "Content-Encoding": {"br"}}, want: time.Minute + time.Hour},
		{name: "Vary: Accept-Encoding with an unknown coding", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Encoding"}, "Content-Encoding": {"compress"}}},
		{name: "unknown coding without Vary: Accept-Encoding", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Content-Encoding": {"compress"}}, want: time.Minute + time.Hour},
		{name: "trailers not stored", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}, "Trailer": {"X-Checksum"}}},
		{name: "§4.2.1 Expires-based freshness", status: 200, respHdr: http.Header{"Date": {date}, "Expires": {t0.Add(10 * time.Minute).Format(http.TimeFormat)}}, want: 10*time.Minute + time.Hour},
		{name: "§5.3 invalid Expires not stored", status: 200, respHdr: http.Header{"Expires": {"0"}}},
		{name: "staleTtl 0", status: 200, respHdr: http.Header{"Cache-Control": {"max-age=60"}}, mutate: func(s *settings) { s.staleTTL = 0 }, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, "http://example.com/", nil)
			for k, v := range tt.reqHdr {
				req.Header[k] = v
			}
			cfg := testSettings()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			reqCC := parseCacheControl(req.Header.Values("Cache-Control"))
			cc := parseCacheControl(tt.respHdr.Values("Cache-Control"))
			e := &entry{status: tt.status, header: tt.respHdr, requestTime: t0, responseTime: t0}
			got, ok := storeTTL(req, reqCC, e, cc, cfg)
			if ok != (tt.want > 0) || got != tt.want {
				t.Errorf("storeTTL() = %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
}

func TestLookupAllowed(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{name: "plain request", header: http.Header{}, want: true},
		{name: "§5.2.1.4 request no-cache", header: http.Header{"Cache-Control": {"no-cache"}}, want: false},
		{name: "§5.2.1.5 request no-store may still be served", header: http.Header{"Cache-Control": {"no-store"}}, want: true},
		{name: "§5.2.1.4 request no-cache has no qualified form", header: http.Header{"Cache-Control": {`no-cache="Set-Cookie"`}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookupAllowed(parseCacheControl(tt.header.Values("Cache-Control"))); got != tt.want {
				t.Errorf("lookupAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServable(t *testing.T) {
	tests := []struct {
		name          string
		reqCC         string
		age, lifetime time.Duration
		want          bool
	}{
		{name: "§4.2 fresh", age: 10 * time.Second, lifetime: time.Minute, want: true},
		{name: "§4.2 stale when age equals lifetime", age: time.Minute, lifetime: time.Minute, want: false},
		{name: "§5.2.1.1 request max-age satisfied", reqCC: "max-age=10", age: 10 * time.Second, lifetime: time.Minute, want: true},
		{name: "§5.2.1.1 request max-age exceeded", reqCC: "max-age=10", age: 11 * time.Second, lifetime: time.Minute, want: false},
		{name: "§5.2.1.1 request max-age=0", reqCC: "max-age=0", age: time.Second, lifetime: time.Minute, want: false},
		{name: "§5.2.1.3 min-fresh satisfied", reqCC: "min-fresh=30", age: 30 * time.Second, lifetime: time.Minute, want: true},
		{name: "§5.2.1.3 min-fresh not satisfied", reqCC: "min-fresh=31", age: 30 * time.Second, lifetime: time.Minute, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := parseCacheControl([]string{tt.reqCC})
			if got := servable(cc, tt.age, tt.lifetime); got != tt.want {
				t.Errorf("servable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStaleAllowed(t *testing.T) {
	tests := []struct {
		name      string
		cc        string
		staleness time.Duration
		staleTTL  time.Duration
		fallback  time.Duration
		want      bool
	}{
		{name: "RFC 5861 within the window", cc: "max-age=60, stale-if-error=30", staleness: 29 * time.Second, staleTTL: time.Hour, want: true},
		{name: "RFC 5861 just stale", cc: "max-age=60, stale-if-error=30", staleness: 0, staleTTL: time.Hour, want: true},
		{name: "RFC 5861 window over", cc: "max-age=60, stale-if-error=30", staleness: 30 * time.Second, staleTTL: time.Hour},
		{name: "§4.2.4 no directive, no stale", cc: "max-age=60", staleness: time.Second, staleTTL: time.Hour},
		{name: "window of 0", cc: "max-age=60, stale-if-error=0", staleness: -time.Second, staleTTL: time.Hour},
		{name: "window capped at staleTtl", cc: "max-age=60, stale-if-error=86400", staleness: 10 * time.Minute, staleTTL: 5 * time.Minute},
		{name: "window within staleTtl", cc: "max-age=60, stale-if-error=86400", staleness: 4 * time.Minute, staleTTL: 5 * time.Minute, want: true},
		{name: "staleTtl 0 never serves stale", cc: "max-age=60, stale-if-error=30", staleness: 0, staleTTL: 0},
		{name: "§5.2.2.2 must-revalidate forbids", cc: "max-age=60, stale-if-error=30, must-revalidate", staleness: time.Second, staleTTL: time.Hour},
		{name: "§5.2.2.8 proxy-revalidate forbids", cc: "max-age=60, stale-if-error=30, proxy-revalidate", staleness: time.Second, staleTTL: time.Hour},
		{name: "§5.2.2.10 s-maxage forbids", cc: "s-maxage=60, stale-if-error=30", staleness: time.Second, staleTTL: time.Hour},
		{name: "§5.2.2.4 no-cache forbids", cc: "max-age=60, stale-if-error=30, no-cache", staleness: time.Second, staleTTL: time.Hour},
		{name: "§4.2.4 configured window when the directive is absent", cc: "max-age=60", staleness: 29 * time.Second, staleTTL: time.Hour, fallback: 30 * time.Second, want: true},
		{name: "configured window over", cc: "max-age=60", staleness: 30 * time.Second, staleTTL: time.Hour, fallback: 30 * time.Second},
		{name: "configured window without Cache-Control (defaultTtl)", cc: "", staleness: time.Second, staleTTL: time.Hour, fallback: 30 * time.Second, want: true},
		{name: "the directive wins over a longer configured window", cc: "max-age=60, stale-if-error=5", staleness: 10 * time.Second, staleTTL: time.Hour, fallback: 30 * time.Second},
		{name: "the directive wins over a shorter configured window", cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second, staleTTL: time.Hour, fallback: 5 * time.Second, want: true},
		{name: "a directive of 0 turns the configured window off", cc: "max-age=60, stale-if-error=0", staleness: time.Second, staleTTL: time.Hour, fallback: 30 * time.Second},
		{name: "configured window capped at staleTtl", cc: "max-age=60", staleness: 10 * time.Minute, staleTTL: 5 * time.Minute, fallback: time.Hour},
		{name: "§5.2.2.2 must-revalidate forbids the configured window", cc: "max-age=60, must-revalidate", staleness: time.Second, staleTTL: time.Hour, fallback: 30 * time.Second},
		{name: "§5.2.2.10 s-maxage forbids the configured window", cc: "s-maxage=60", staleness: time.Second, staleTTL: time.Hour, fallback: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := parseCacheControl([]string{tt.cc})
			if got := staleAllowed(cc, cc.staleIfError, tt.fallback, tt.staleness, tt.staleTTL); got != tt.want {
				t.Errorf("staleAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServableWhileRevalidating(t *testing.T) {
	tests := []struct {
		name      string
		reqCC     string
		cc        string
		staleness time.Duration
		want      bool
	}{
		{name: "RFC 5861 §3 stale within the window", cc: "max-age=60, stale-while-revalidate=30", staleness: 10 * time.Second, want: true},
		{name: "RFC 5861 §3 window over", cc: "max-age=60, stale-while-revalidate=30", staleness: 30 * time.Second},
		{name: "stale-if-error is not stale-while-revalidate", cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second},
		{name: "a fresh response the client refused is not stale", reqCC: "max-age=5", cc: "max-age=60, stale-while-revalidate=30", staleness: -10 * time.Second},
		{name: "§5.2.1.1 request max-age wants no stale response", reqCC: "max-age=600", cc: "max-age=60, stale-while-revalidate=30", staleness: 10 * time.Second},
		{name: "§5.2.1.3 request min-fresh wants no stale response", reqCC: "min-fresh=0", cc: "max-age=60, stale-while-revalidate=30", staleness: 10 * time.Second},
		{name: "§5.2.1.7 only-if-cached takes it", reqCC: "only-if-cached", cc: "max-age=60, stale-while-revalidate=30", staleness: 10 * time.Second, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := servableWhileRevalidating(parseCacheControl([]string{tt.reqCC}), parseCacheControl([]string{tt.cc}), tt.staleness, testSettings())
			if got != tt.want {
				t.Errorf("servableWhileRevalidating() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServableOnRequest(t *testing.T) {
	tests := []struct {
		name      string
		reqCC     string
		respCC    string
		age       time.Duration // of a response fresh for a minute
		staleTTL  time.Duration
		want      bool
		wantFresh bool
	}{
		{name: "§5.2.1.2 max-stale", reqCC: "max-stale", age: 50 * time.Minute, want: true},
		{name: "§5.2.1.2 max-stale=N within N", reqCC: "max-stale=30", age: 89 * time.Second, want: true},
		{name: "§5.2.1.2 max-stale=N at N", reqCC: "max-stale=30", age: 90 * time.Second},
		{name: "§5.2.1.2 no max-stale", reqCC: "max-age=3600", age: 70 * time.Second},
		{name: "§5.2.1.2 fresh response is not its business", reqCC: "max-stale", age: 10 * time.Second},
		{name: "capped at staleTtl", reqCC: "max-stale", age: 80 * time.Second, staleTTL: 10 * time.Second},
		{name: "staleTtl 0", reqCC: "max-stale", age: 61 * time.Second, staleTTL: -1},
		{name: "§4.2.4 no-cache", reqCC: "max-stale", respCC: "no-cache", age: 70 * time.Second},
		{name: "§4.2.4 must-revalidate", reqCC: "max-stale", respCC: "must-revalidate", age: 70 * time.Second},
		{name: "§4.2.4 proxy-revalidate", reqCC: "max-stale", respCC: "proxy-revalidate", age: 70 * time.Second},
		{name: "§5.2.2.10 s-maxage", reqCC: "max-stale", respCC: "s-maxage=60", age: 70 * time.Second},
		{name: "§5.2.1.1 max-age satisfied", reqCC: "max-stale, max-age=70", age: 70 * time.Second, want: true},
		{name: "§5.2.1.1 max-age exceeded", reqCC: "max-stale, max-age=69", age: 70 * time.Second},
		{name: "§5.2.1.3 min-fresh", reqCC: "max-stale, min-fresh=0", age: 70 * time.Second},
		{name: "the stale windows of the response are not needed", reqCC: "max-stale", respCC: "stale-while-revalidate=0, stale-if-error=0", age: 70 * time.Second, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staleTTL := tt.staleTTL
			if staleTTL == 0 {
				staleTTL = time.Hour
			} else if staleTTL < 0 {
				staleTTL = 0
			}
			reqCC := parseCacheControl([]string{tt.reqCC})
			cc := parseCacheControl([]string{tt.respCC})
			if got := servableOnRequest(reqCC, cc, tt.age, time.Minute, staleTTL); got != tt.want {
				t.Errorf("servableOnRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServableOnError(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		cc        string
		staleness time.Duration
		want      bool
	}{
		{name: "RFC 5861 §4 500", status: 500, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second, want: true},
		{name: "RFC 5861 §4 502", status: 502, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second, want: true},
		{name: "RFC 5861 §4 503", status: 503, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second, want: true},
		{name: "RFC 5861 §4 504", status: 504, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second, want: true},
		{name: "RFC 5861 §4 501 is not an error", status: 501, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second},
		{name: "RFC 5861 §4 404 is not an error", status: 404, cc: "max-age=60, stale-if-error=30", staleness: 10 * time.Second},
		{name: "RFC 5861 §4 window over", status: 503, cc: "max-age=60, stale-if-error=30", staleness: 30 * time.Second},
		{name: "stale-while-revalidate is not stale-if-error", status: 503, cc: "max-age=60, stale-while-revalidate=30", staleness: 10 * time.Second},
		{name: "a fresh response the client refused", status: 503, cc: "max-age=60, stale-if-error=30", staleness: -10 * time.Second, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := servableOnError(tt.status, parseCacheControl([]string{tt.cc}), tt.staleness, testSettings()); got != tt.want {
				t.Errorf("servableOnError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupersedes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
		want   bool
	}{
		{name: "status not in statusCodes", status: 404, header: http.Header{"Cache-Control": {"max-age=60"}}, want: true},
		{name: "resource gone", status: 410, header: http.Header{}, want: true},
		{name: "request rejected: credentials", status: 401, header: http.Header{}},
		{name: "request rejected: forbidden", status: 403, header: http.Header{"Cache-Control": {"no-store"}}},
		{name: "request rejected: malformed", status: 400, header: http.Header{}},
		{name: "request rejected: header fields too large", status: 431, header: http.Header{}},
		{name: "§5.2.2.5 no-store", status: 200, header: http.Header{"Cache-Control": {"no-store"}}, want: true},
		{name: "§5.2.2.7 private", status: 200, header: http.Header{"Cache-Control": {"private, max-age=60"}}, want: true},
		{name: "already stale on arrival", status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"60"}}, want: true},
		{name: "§4.1 Vary: *", status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"*"}}, want: true},
		{name: "backend error", status: 503, header: http.Header{}},
		{name: "any 5xx", status: 507, header: http.Header{}},
		{name: "backend throttling", status: 429, header: http.Header{}},
		{name: "storable for a request without credentials: explicit freshness", status: 200, header: http.Header{"Cache-Control": {"max-age=60"}}},
		{name: "storable for a request without credentials: defaultTtl", status: 200, header: http.Header{}},
		{name: "storable as another variant", status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := parseCacheControl(tt.header.Values("Cache-Control"))
			e := &entry{status: tt.status, header: tt.header, requestTime: t0, responseTime: t0}
			if got := supersedes(e, cc, testSettings()); got != tt.want {
				t.Errorf("supersedes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeaderToStore(t *testing.T) {
	h := http.Header{
		"Content-Type":      {"text/plain"},
		"Connection":        {"X-Hop"},
		"X-Hop":             {"1"},
		"Keep-Alive":        {"timeout=5"},
		"Transfer-Encoding": {"chunked"},
		"Content-Length":    {"12"},
		// §3.1: specific to the proxy a response came through.
		"Proxy-Authenticate":        {"Basic"},
		"Proxy-Authentication-Info": {"nextnonce=1"},
		"Cache-Control":             {`max-age=60, private="Set-Cookie", no-cache="x-user"`},
		"Set-Cookie":                {"sid=1"},
		"X-User":                    {"bob"},
		"Age":                       {"3"},
	}
	got := headerToStore(h, parseCacheControl(h.Values("Cache-Control")))
	for _, name := range []string{"Connection", "X-Hop", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Proxy-Authenticate", "Proxy-Authentication-Info", "Set-Cookie", "X-User"} {
		if got.Get(name) != "" {
			t.Errorf("%s must not be stored", name)
		}
	}
	if got.Get("Content-Type") != "text/plain" || got.Get("Age") != "3" || got.Get("Cache-Control") == "" {
		t.Errorf("end-to-end fields must be stored: %v", got)
	}
	if h.Get("Set-Cookie") == "" || h.Get("X-User") == "" {
		t.Error("the original header must not be modified")
	}

	// §3.1: every field is stored unless the response says otherwise.
	got = headerToStore(http.Header{"Cache-Control": {"max-age=60"}, "Set-Cookie": {"sid=1"}, "X-User": {"bob"}}, parseCacheControl([]string{"max-age=60"}))
	if got.Get("Set-Cookie") != "sid=1" || got.Get("X-User") != "bob" {
		t.Errorf("§3.1 fields that are not excluded must be kept: %v", got)
	}
}
