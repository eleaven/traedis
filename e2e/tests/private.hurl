# Nothing personal is shared. whoami echoes the request headers in its body.

# Authorization without explicit freshness: defaultTtl is not applied, not stored
GET {{base_url}}/whoami-cache?auth={{run}}
Authorization: Bearer e2e-secret
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body contains "e2e-secret"

GET {{base_url}}/whoami-cache?auth={{run}}
Authorization: Bearer e2e-secret
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# Another client does not get the personal response
GET {{base_url}}/whoami-cache?auth={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body not contains "e2e-secret"

# Cookie without explicit freshness: defaultTtl is not applied, not stored
GET {{base_url}}/whoami-cache?cookie={{run}}
Cookie: sid=e2e-secret
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body contains "e2e-secret"

GET {{base_url}}/whoami-cache?cookie={{run}}
Cookie: sid=e2e-secret
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/whoami-cache?cookie={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body not contains "e2e-secret"

# Authorization with `public` (RFC 9111 §3.5): placeholder allows sharing its response
GET {{base_url}}/placeholder-cache/600x600?auth={{run}}
Authorization: Bearer e2e-secret
HTTP 200
[Asserts]
header "Cache-Control" contains "public"
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/placeholder-cache/600x600?auth={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# Set-Cookie: a cookie is never shared. The response is stored without it when the backend asks for that with
# `private="Set-Cookie"` (§5.2.2.7): only the client that caused the miss gets the cookie
GET {{base_url}}/whoami-cookie?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
header "Set-Cookie" == "sid=e2e-secret"

GET {{base_url}}/whoami-cookie?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Set-Cookie" not exists
header "Cache-Control" contains "private="

# Otherwise a response with Set-Cookie is not stored, even `public`
GET {{base_url}}/whoami-cookie-public?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
header "Set-Cookie" == "sid=e2e-secret"

GET {{base_url}}/whoami-cookie-public?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
header "Set-Cookie" == "sid=e2e-secret"

# authorizationHeaders: basicAuth replaced Authorization with X-WebAuth-User, which the cache is told about. Without
# explicit freshness, defaultTtl is not applied: the page of alice is not stored, and bob gets his own
GET {{base_url}}/whoami-authorization?run={{run}}
[BasicAuth]
alice: alice-secret
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body contains "X-Webauth-User: alice"
body not contains "Authorization"

GET {{base_url}}/whoami-authorization?run={{run}}
[BasicAuth]
bob: bob-secret
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
body contains "X-Webauth-User: bob"
