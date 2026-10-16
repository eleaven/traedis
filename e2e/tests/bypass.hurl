# Requests that are never answered from the cache. But for unsafe methods (see invalidation.hurl), the
# response writer is not wrapped (Yaegi would hide http.Flusher from the backend).

# Stored first: none of the requests below may be answered from it
GET {{base_url}}/whoami-cache?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# Event streams
GET {{base_url}}/whoami-cache?run={{run}}
Accept: text/event-stream
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=bypass"

# Upgrade requests (WebSockets)
GET {{base_url}}/whoami-cache?run={{run}}
Connection: Upgrade
Upgrade: websocket
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=bypass"

# Requests with content (RFC 9110 §9.3.1): the backend may read parameters in it, which the cache key does not have
GET {{base_url}}/whoami-cache?run={{run}}
```
q=other
```
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=bypass"

# Unsafe methods, last: a successful one invalidates what was stored
POST {{base_url}}/whoami-cache?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=method"
