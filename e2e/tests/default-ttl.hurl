# defaultTtl (0 by default, 5m on /whoami-cache): heuristic freshness (RFC 9111 §4.2.2), whoami sends no Cache-Control.

GET {{base_url}}/whoami-cache?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Captures]
body_hash: sha256
[Asserts]
header "Cache-Control" not exists
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/whoami-cache?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Cache-Status" regex /ttl=(\d+)/ toInt <= 300
header "Cache-Status" regex /ttl=(\d+)/ toInt > 240
# The Cache-Control sent downstream is never rewritten
header "Cache-Control" not exists
sha256 == {{body_hash}}
