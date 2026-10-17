# Stale responses (RFC 5861): /whoami-stale answers with
# `Cache-Control: max-age=2, stale-while-revalidate=10, stale-if-error=60`, and the status of its /health can be
# changed with a POST. That status is shared: only this file requests /whoami-stale/health.
# /whoami-stale-default is the same backend without these directives: the cache is configured with
# `defaultTtl: 2s`, `defaultStaleWhileRevalidate: 10s` and `defaultStaleIfError: 1m`.

POST {{base_url}}/whoami-stale/health
```
200
```
HTTP 200

# stale-while-revalidate (§3): once stale, the response is still served, and refreshed in the background

GET {{base_url}}/whoami-stale?swr={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Captures]
body_hash: sha256
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/whoami-stale?swr={{run}}
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/
header "Age" toInt >= 2
sha256 == {{body_hash}}

# The background revalidation replaced the entry: fresh again
GET {{base_url}}/whoami-stale?swr={{run}}
[Options]
delay: 500ms
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=\d+$/

# A client cannot remove it through the background refresh, which is its request: whoami echoes its header fields, and
# large ones get a response too large to be stored (maxBodyBytes: 2 KiB). The stale response is kept for the others

GET {{base_url}}/whoami-stale?evict={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/whoami-stale?evict={{run}}
X-Padding: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

GET {{base_url}}/whoami-stale?evict={{run}}
[Options]
delay: 500ms
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/
body not contains "X-Padding"

# Refreshed by this request in turn: fresh again
GET {{base_url}}/whoami-stale?evict={{run}}
[Options]
delay: 500ms
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=\d+$/

# stale-if-error (§4): the stored response replaces a backend error

GET {{base_url}}/whoami-stale/health?sie={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

POST {{base_url}}/whoami-stale/health
```
503
```
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=method"

# Even for a client refusing the stored response (max-age=0, a browser reload)
GET {{base_url}}/whoami-stale/health?sie={{run}}
Cache-Control: max-age=0
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; fwd=(stale|request); fwd-status=503; ttl=-?\d+; detail=stale-if-error$/
header "Age" matches /^\d+$/

# no-cache (RFC 9111 §5.2.1.4): the cache is not looked at, the error goes through
GET {{base_url}}/whoami-stale/health?sie={{run}}
Cache-Control: no-cache
HTTP 503
[Asserts]
header "Cache-Status" contains "traedis; fwd=request; fwd-status=503"

# A background revalidation answered by an error keeps the entry
GET {{base_url}}/whoami-stale/health?sie={{run}}
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

GET {{base_url}}/whoami-stale/health?sie={{run}}
[Options]
delay: 500ms
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

# Nor does a client error (401): the backend may be rejecting the request that triggered the revalidation (its
# Authorization, its cookies…), which another client would not see

POST {{base_url}}/whoami-stale/health
```
401
```
HTTP 200

GET {{base_url}}/whoami-stale/health?sie={{run}}
Authorization: Bearer invalid
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

GET {{base_url}}/whoami-stale/health?sie={{run}}
[Options]
delay: 500ms
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

# A background revalidation answered by a response that cannot be stored (404 is not in statusCodes) deletes it

POST {{base_url}}/whoami-stale/health
```
404
```
HTTP 200

GET {{base_url}}/whoami-stale/health?sie={{run}}
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/

GET {{base_url}}/whoami-stale/health?sie={{run}}
[Options]
delay: 500ms
HTTP 404
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=404"

# defaultStaleIfError and defaultStaleWhileRevalidate: the same for a backend without the directives

POST {{base_url}}/whoami-stale-default/health
```
200
```
HTTP 200

GET {{base_url}}/whoami-stale-default/health?default={{run}}
HTTP 200
[Asserts]
header "Cache-Control" not exists
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

POST {{base_url}}/whoami-stale-default/health
```
503
```
HTTP 200

GET {{base_url}}/whoami-stale-default/health?default={{run}}
Cache-Control: max-age=0
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; fwd=(stale|request); fwd-status=503; ttl=-?\d+; detail=stale-if-error$/

GET {{base_url}}/whoami-stale-default/health?default={{run}}
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=stale-while-revalidate$/
# The Cache-Control sent downstream is never rewritten
header "Cache-Control" not exists

POST {{base_url}}/whoami-stale/health
```
200
```
HTTP 200
