package traedis

import (
	"strconv"
	"time"
)

const cacheName = "traedis"

// cacheStatus is our member of the Cache-Status response header (RFC 9211).
type cacheStatus struct {
	hit       bool          // answered from the cache without forwarding
	served    bool          // a stored response was sent although the request was forwarded
	ttl       time.Duration // remaining freshness of the stored response sent, negative when stale
	fwd       string        // uri-miss, vary-miss, stale, request, method or bypass
	fwdStatus int           // status received from the backend
	key       string        // only when exposeKey is set
	detail    string        // token: redis, stale-while-revalidate, stale-if-error, max-stale or invalidated
	// collapsed: the response sent is the one stored for another request that was forwarded, which this one waited for (see collapse).
	collapsed bool
}

func (s cacheStatus) String() string {
	v := cacheName
	if s.hit {
		v += "; hit"
	} else {
		v += "; fwd=" + s.fwd
		if s.fwdStatus > 0 {
			v += "; fwd-status=" + strconv.Itoa(s.fwdStatus)
		}
	}
	if s.hit || s.served {
		v += "; ttl=" + strconv.FormatInt(int64(s.ttl/time.Second), 10)
	}
	if s.collapsed {
		v += "; collapsed"
	}
	if s.key != "" {
		v += "; key=" + strconv.Quote(s.key)
	}
	if s.detail != "" {
		v += "; detail=" + s.detail
	}
	return v
}
