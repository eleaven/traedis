package traedis

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCacheURI(t *testing.T) {
	tests := []struct {
		name   string
		target string
		host   string
		tls    bool
		proto  string // X-Forwarded-Proto
		sort   bool   // sortQuery
		want   string
	}{
		{name: "host is lowercased", target: "/a", host: "Example.COM", want: "http://example.com/a"},
		{name: "port is kept", target: "/a", host: "example.com:8080", want: "http://example.com:8080/a"},
		{name: "https when TLS", target: "/a", host: "example.com", tls: true, want: "https://example.com/a"},
		{name: "X-Forwarded-Proto: https over http", target: "/a", host: "example.com", proto: "https", want: "https://example.com/a"},
		{
			name:   "X-Forwarded-Proto: http over TLS",
			target: "/a",
			host:   "example.com",
			tls:    true,
			proto:  "http",
			want:   "http://example.com/a",
		},
		{name: "X-Forwarded-Proto is case-insensitive", target: "/a", host: "example.com", proto: " HTTPS ", want: "https://example.com/a"},
		{
			name:   "X-Forwarded-Proto: the first of a list",
			target: "/a",
			host:   "example.com",
			proto:  "https, http",
			want:   "https://example.com/a",
		},
		{
			name:   "X-Forwarded-Proto: another value is ignored",
			target: "/a",
			host:   "example.com",
			tls:    true,
			proto:  "wss",
			want:   "https://example.com/a",
		},
		{name: "path case is kept", target: "/A/b", host: "example.com", want: "http://example.com/A/b"},
		{name: "escaped path is kept", target: "/a%2Fb", host: "example.com", want: "http://example.com/a%2Fb"},
		{name: "§4 query is kept as it is", target: "/?b=2&a=1&c=3", host: "example.com", want: "http://example.com/?b=2&a=1&c=3"},
		{name: "query is not decoded", target: "/?b=%41&a=A", host: "example.com", want: "http://example.com/?b=%41&a=A"},
		{name: "empty query is dropped", target: "/a?", host: "example.com", want: "http://example.com/a"},
		{name: "empty parameters are kept", target: "/?b=2&&a=1&", host: "example.com", want: "http://example.com/?b=2&&a=1&"},
		{
			name:   "sortQuery: query sorted by name",
			target: "/?b=2&a=1&c=3",
			host:   "example.com",
			sort:   true,
			want:   "http://example.com/?a=1&b=2&c=3",
		},
		{
			name:   "sortQuery: repeated names keep their order",
			target: "/?b=1&a=2&b=0&a=1",
			host:   "example.com",
			sort:   true,
			want:   "http://example.com/?a=2&a=1&b=1&b=0",
		},
		{
			name:   "sortQuery: query is not decoded",
			target: "/?b=%41&a=A",
			host:   "example.com",
			sort:   true,
			want:   "http://example.com/?a=A&b=%41",
		},
		{name: "sortQuery: empty query is dropped", target: "/a?", host: "example.com", sort: true, want: "http://example.com/a"},
		{
			name:   "sortQuery: empty parameters are dropped",
			target: "/?b=2&&a=1&",
			host:   "example.com",
			sort:   true,
			want:   "http://example.com/?a=1&b=2",
		},
		{name: "sortQuery: parameter without value", target: "/?b&a=1", host: "example.com", sort: true, want: "http://example.com/?a=1&b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.target, nil)
			r.Host = tt.host
			r.TLS = nil
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			if got := cacheURI(r, tt.sort); got != tt.want {
				t.Errorf("cacheURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedirectsToItself(t *testing.T) {
	tests := []struct {
		name     string
		target   string // of the request, on host example.com
		proto    string // X-Forwarded-Proto
		sort     bool   // sortQuery
		location string
		want     bool
	}{
		{name: "no Location", target: "/a", location: ""},
		{name: "same URI", target: "/a", location: "http://example.com/a", want: true},
		{name: "relative path", target: "/a", location: "/a", want: true},
		{name: "relative reference", target: "/a/b", location: "b", want: true},
		{name: "scheme-relative", target: "/a", location: "//example.com/a", want: true},
		{name: "fragment ignored", target: "/a", location: "/a#top", want: true},
		{name: "host case ignored", target: "/a", location: "http://EXAMPLE.com/a", want: true},
		{name: "scheme case ignored", target: "/a", location: "HTTP://example.com/a", want: true},
		{name: "default port of http", target: "/a", location: "http://example.com:80/a", want: true},
		{name: "default port of https", target: "/a", proto: "https", location: "https://example.com:443/a", want: true},
		{name: "same URI over https", target: "/a", proto: "https", location: "https://example.com/a", want: true},
		{name: "same query", target: "/a?x=1", location: "/a?x=1", want: true},
		{name: "empty path is /", target: "/", location: "http://example.com", want: true},
		{name: "other scheme", target: "/a", location: "https://example.com/a"},
		{name: "other host", target: "/a", location: "http://www.example.com/a"},
		{name: "other port", target: "/a", location: "http://example.com:8080/a"},
		{name: "https port over http", target: "/a", location: "http://example.com:443/a"},
		{name: "other path", target: "/a", location: "/b"},
		{name: "path case kept", target: "/a", location: "/A"},
		{name: "trailing slash", target: "/a", location: "/a/"},
		{name: "escaped path kept", target: "/a/b", location: "/a%2Fb"},
		{name: "other query", target: "/a?x=1", location: "/a?x=2"},
		{name: "query dropped", target: "/a?x=1", location: "/a"},
		{name: "query order kept", target: "/a?x=1&y=2", location: "/a?y=2&x=1"},
		{name: "sortQuery: query order ignored", target: "/a?x=1&y=2", sort: true, location: "/a?y=2&x=1", want: true},
		{name: "unparsable Location", target: "/a", location: "http://[::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.target, nil)
			r.Host = "example.com"
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			h := http.Header{}
			if tt.location != "" {
				h.Set("Location", tt.location)
			}
			if got := redirectsToItself(r, h, tt.sort); got != tt.want {
				t.Errorf("redirectsToItself(%q) = %v, want %v", tt.location, got, tt.want)
			}
		})
	}
}

func TestCacheURIIgnoresMethod(t *testing.T) {
	get := httptest.NewRequest("GET", "http://example.com/a", nil)
	head := httptest.NewRequest("HEAD", "http://example.com/a", nil)
	if cacheURI(get, false) != cacheURI(head, false) {
		t.Error("GET and HEAD must share the same URI")
	}
}

// The key format is stable: a change invalidates every cached entry.
func TestRedisKeyIsStable(t *testing.T) {
	got := redisKey("cache@file", "http://example.com/path?a=1&b=2")
	want := "traedis:cache@file:29846eb9ec45655b03283e847145495f"
	if got != want {
		t.Errorf("redisKey() = %q, want %q", got, want)
	}
}
