package traedis

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestCacheURI(t *testing.T) {
	tests := []struct {
		name   string
		target string
		host   string
		tls    bool
		sort   bool // sortQuery
		want   string
	}{
		{name: "host is lowercased", target: "/a", host: "Example.COM", want: "http://example.com/a"},
		{name: "port is kept", target: "/a", host: "example.com:8080", want: "http://example.com:8080/a"},
		{name: "https when TLS", target: "/a", host: "example.com", tls: true, want: "https://example.com/a"},
		{name: "path case is kept", target: "/A/b", host: "example.com", want: "http://example.com/A/b"},
		{name: "escaped path is kept", target: "/a%2Fb", host: "example.com", want: "http://example.com/a%2Fb"},
		{name: "§4 query is kept as it is", target: "/?b=2&a=1&c=3", host: "example.com", want: "http://example.com/?b=2&a=1&c=3"},
		{name: "query is not decoded", target: "/?b=%41&a=A", host: "example.com", want: "http://example.com/?b=%41&a=A"},
		{name: "empty query is dropped", target: "/a?", host: "example.com", want: "http://example.com/a"},
		{name: "empty parameters are kept", target: "/?b=2&&a=1&", host: "example.com", want: "http://example.com/?b=2&&a=1&"},
		{name: "sortQuery: query sorted by name", target: "/?b=2&a=1&c=3", host: "example.com", sort: true, want: "http://example.com/?a=1&b=2&c=3"},
		{name: "sortQuery: repeated names keep their order", target: "/?b=1&a=2&b=0&a=1", host: "example.com", sort: true, want: "http://example.com/?a=2&a=1&b=1&b=0"},
		{name: "sortQuery: query is not decoded", target: "/?b=%41&a=A", host: "example.com", sort: true, want: "http://example.com/?a=A&b=%41"},
		{name: "sortQuery: empty query is dropped", target: "/a?", host: "example.com", sort: true, want: "http://example.com/a"},
		{name: "sortQuery: empty parameters are dropped", target: "/?b=2&&a=1&", host: "example.com", sort: true, want: "http://example.com/?a=1&b=2"},
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
			if got := cacheURI(r, tt.sort); got != tt.want {
				t.Errorf("cacheURI() = %q, want %q", got, tt.want)
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
