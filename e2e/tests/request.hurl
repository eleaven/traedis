# Request directives (RFC 9111 §5.2.1).

# no-store (§5.2.1.5): the response is not stored
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: no-store
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# only-if-cached (§5.2.1.7): 504 when nothing is stored
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: only-if-cached
HTTP 504
[Asserts]
# RFC 9211 §2: a response of the cache's own, to a request that was not forwarded
header "Cache-Status" not exists

GET {{base_url}}/whoami-cache?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# only-if-cached (§5.2.1.7): served when stored
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: only-if-cached
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# no-cache (§5.2.1.4): the stored response is not used
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: no-cache
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=request; fwd-status=200"

# max-age=0 (§5.2.1.1): the stored response is fresh, but too old for this client (RFC 9211 §2.2: fwd=request)
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: max-age=0
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=request; fwd-status=200"

# min-fresh (§5.2.1.3) beyond defaultTtl (5m): not fresh for long enough
GET {{base_url}}/whoami-cache?run={{run}}
Cache-Control: min-fresh=600
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=request; fwd-status=200"

# Without directives, the stored response is still served
GET {{base_url}}/whoami-cache?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# max-stale (§5.2.1.2): the client accepts a stale response. /imaging-validate is fresh for 2 seconds
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?maxstale={{run}}
HTTP 200
[Captures]
body_hash: sha256
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?maxstale={{run}}
Cache-Control: max-stale
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=max-stale$/
header "Age" toInt >= 2
sha256 == {{body_hash}}

# With only-if-cached: what an offline client asks
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?maxstale={{run}}
Cache-Control: only-if-cached, max-stale=60
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=-\d+; detail=max-stale$/

# Staler than the client accepts: the backend is asked
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?maxstale={{run}}
Cache-Control: max-stale=0
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; fwd=stale; fwd-status=304; ttl=\d+$/
