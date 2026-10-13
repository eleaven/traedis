# TLS to Redis (`rediss://`): the cache works as with any Redis.

GET {{base_url}}/whoami-tls?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/whoami-tls?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# A certificate that no trusted authority signed is refused: nothing is asked of that Redis
GET {{base_url}}/whoami-tls-untrusted?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=bypass; fwd-status=200; detail=redis"
