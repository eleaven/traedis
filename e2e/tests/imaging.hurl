# A costly backend behind the cache: imgproxy fetches the image from placeholder, converts it
# and sends `Cache-Control: max-age, public`. Its URL is all path, with `:` and `@`:
# /<signature>/<processing options>/plain/<source URL>@<format> (it ignores the query).

# First request is a miss
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Captures]
cache_control: header "Cache-Control"
body_hash: sha256
[Asserts]
header "Content-Type" == "image/avif"
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# Second request is a hit: the converted image, as stored
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Content-Type" == "image/avif"
header "Cache-Control" == {{cache_control}}
sha256 == {{body_hash}}

# Another format is another resource
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@webp?run={{run}}
HTTP 200
[Asserts]
header "Content-Type" == "image/webp"
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# Other processing options are another resource
GET {{base_url}}/imaging-cache/unsafe/rs:fill:301:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# statusCodes (200-399 by default): the error of a missing source image is not stored
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/missing@avif?run={{run}}
HTTP 404
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=404"

GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/missing@avif?run={{run}}
HTTP 404
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=404"

# statusCodes with 404 listed: the same error is stored, and served from the cache
GET {{base_url}}/imaging-cache-errors/unsafe/rs:fill:300:200/plain/http://placeholder:3000/missing@avif?run={{run}}
HTTP 404
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=404"

GET {{base_url}}/imaging-cache-errors/unsafe/rs:fill:300:200/plain/http://placeholder:3000/missing@avif?run={{run}}
HTTP 404
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
