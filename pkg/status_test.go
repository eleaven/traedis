package traedis

import (
	"testing"
	"time"
)

func TestCacheStatusString(t *testing.T) {
	tests := []struct {
		name string
		s    cacheStatus
		want string
	}{
		{name: "RFC 9211 hit with ttl", s: cacheStatus{hit: true, ttl: 59900 * time.Millisecond}, want: "traedis; hit; ttl=59"},
		{
			name: "RFC 9211 miss with fwd-status",
			s:    cacheStatus{fwd: "uri-miss", fwdStatus: 200},
			want: "traedis; fwd=uri-miss; fwd-status=200",
		},
		{name: "RFC 9211 method", s: cacheStatus{fwd: "method"}, want: "traedis; fwd=method"},
		{
			name: "RFC 9211 bypass with detail",
			s:    cacheStatus{fwd: "bypass", fwdStatus: 502, detail: "redis"},
			want: "traedis; fwd=bypass; fwd-status=502; detail=redis",
		},
		{
			name: "RFC 9211 stale hit has a negative ttl",
			s:    cacheStatus{hit: true, ttl: -12500 * time.Millisecond, detail: "stale-while-revalidate"},
			want: "traedis; hit; ttl=-12; detail=stale-while-revalidate",
		},
		{
			name: "RFC 9211 stale-if-error is forwarded, not a hit",
			s:    cacheStatus{fwd: "stale", fwdStatus: 503, served: true, ttl: -12 * time.Second, detail: "stale-if-error"},
			want: "traedis; fwd=stale; fwd-status=503; ttl=-12; detail=stale-if-error",
		},
		{
			name: "RFC 9211 key",
			s:    cacheStatus{hit: true, ttl: time.Second, key: "traedis:abc"},
			want: `traedis; hit; ttl=1; key="traedis:abc"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
