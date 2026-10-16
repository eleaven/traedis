package traedis

import (
	"net/http"
	"sort"
	"strings"
)

const keyPrefix = "traedis:"

// cacheURI returns the normalized URI identifying a resource: scheme, lowercased
// host (port kept), escaped path and raw query. With sortQuery, the query is
// sorted by parameter name: URIs that only differ by the order of their
// parameters, which are not the same for RFC 9111 (§4), then share their stored
// responses. The method is not part of it. This format is stable: changing it
// invalidates the cache.
func cacheURI(r *http.Request, sortQuery bool) string {
	// Scheme: the one of X-Forwarded-Proto, which Traefik sets to the one of
	// the connection, unless a proxy it trusts (forwardedHeaders) sent it:
	// behind a proxy that terminates TLS, https requests reach Traefik over
	// http too, and must not share their stored responses with http ones (a
	// redirection to https would answer them, in a loop). A client cannot
	// choose it: Traefik removes what an untrusted one sends. Without it (the
	// handler is not behind Traefik), the one of the connection.
	scheme := "http"
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "https":
		scheme = "https"
	case "http":
	default:
		if r.TLS != nil {
			scheme = "https"
		}
	}

	// Host
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	host = strings.ToLower(host)

	// Path
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	// Query
	query := r.URL.RawQuery
	if sortQuery {
		query = sortedQuery(query)
	}

	// URI
	uri := scheme + "://" + host + path
	if query != "" {
		uri += "?" + query
	}

	return uri
}

// sortedQuery sorts raw query parameters by name, without decoding them, and
// drops the empty ones. The sort is stable so repeated names keep their
// relative order.
func sortedQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var params []string
	for _, p := range strings.Split(raw, "&") {
		if p != "" {
			params = append(params, p)
		}
	}
	sort.SliceStable(params, func(i, j int) bool {
		return paramName(params[i]) < paramName(params[j])
	})
	return strings.Join(params, "&")
}

// paramName returns the name of a raw query parameter: what comes before its
// first "=", or all of it when it has no value.
func paramName(p string) string {
	if i := strings.IndexByte(p, '='); i >= 0 {
		return p[:i]
	}
	return p
}

// redisKey returns the Redis key of a URI for the middleware of the given
// name: each middleware has its own stored responses, whatever the database
// they are in. This format is stable: changing it invalidates the cache.
func redisKey(name, uri string) string {
	return keyPrefix + name + ":" + hashHex(uri)
}
