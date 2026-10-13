# Cache key: scheme + lowercased host + path + query.

GET {{base_url}}/placeholder-cache/600x600?b=2&a=1&run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

GET {{base_url}}/placeholder-cache/600x600?b=2&a=1&run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit"
# exposeKey is false: the Redis key is not disclosed
header "Cache-Status" not contains "key="

# Another query parameter order is another resource (RFC 9111 §4)
GET {{base_url}}/placeholder-cache/600x600?a=1&b=2&run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

# Unless sortQuery is true
GET {{base_url}}/whoami-sort-query?b=2&a=1&run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

GET {{base_url}}/whoami-sort-query?run={{run}}&a=1&b=2
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit"

# Another query value is another resource
GET {{base_url}}/placeholder-cache/600x600?a=1&b=3&run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

# Another path is another resource
GET {{base_url}}/placeholder-cache/601x600?a=1&b=2&run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

# Host is case-insensitive
GET {{base_url}}/whoami-cache?host={{run}}
Host: Traedis.Example
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

GET {{base_url}}/whoami-cache?host={{run}}
Host: traedis.example
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit"

# Another host is another resource
GET {{base_url}}/whoami-cache?host={{run}}
Host: other.example
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"

# The key has the name of the middleware: two middlewares on the way to the same URL don't share what they store
GET {{base_url}}/whoami-key?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"
header "Cache-Status" matches /key="traedis:whoami-key@file:[0-9a-f]{32}"/

GET {{base_url}}/whoami-key?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit"

GET {{base_url}}/whoami-key?run={{run}}
X-Tenant: b
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss"
header "Cache-Status" matches /key="traedis:whoami-key-tenant@file:[0-9a-f]{32}"/
