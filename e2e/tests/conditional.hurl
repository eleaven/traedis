# Conditional requests (RFC 9111 §4.3). imgproxy sends an `ETag` and answers `304` to a matching `If-None-Match`.
# The cache answers the conditions of the client from the response it has stored, and validates its stale
# responses with the backend: /imaging-validate is the same backend with `Cache-Control: max-age=2`.

# The client's conditions (§4.3.2)

GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Captures]
etag: header "ETag"
body_hash: sha256
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

# The client already has the stored response: 304, without its body
GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-None-Match: {{etag}}
HTTP 304
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "ETag" == {{etag}}
header "Cache-Control" exists
header "Content-Type" not exists
bytes count == 0

# Weak comparison, in a list
GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-None-Match: "other", W/{{etag}}
HTTP 304
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# It has another one: the stored response
GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-None-Match: "other"
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Content-Type" == "image/avif"
sha256 == {{body_hash}}

# If-Modified-Since: the stored response has no Last-Modified, it is as old as its Date
GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-Modified-Since: Fri, 01 Jan 2100 00:00:00 GMT
HTTP 304
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-Modified-Since: Sat, 01 Jan 2000 00:00:00 GMT
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# Without a stored response, the conditions are for the backend: the request reaches it as it is, and its 304
# goes to the client (fwd-status=304). Nothing is stored. imgproxy ignores the query: same image, same ETag.
GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?miss={{run}}
If-None-Match: {{etag}}
HTTP 304
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=304"
header "ETag" == {{etag}}
bytes count == 0

GET {{base_url}}/imaging-cache/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?miss={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
header "Content-Type" == "image/avif"
sha256 == {{body_hash}}


# Validation of a stale response (§4.3.1, §4.3.3)

GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
header "Cache-Control" == "max-age=2"
header "ETag" == {{etag}}

# Once stale, the backend is asked with the stored ETag: its 304 has no body, the stored image is served
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; fwd=stale; fwd-status=304; ttl=\d+$/
header "Age" toInt <= 1
header "Content-Type" == "image/avif"
sha256 == {{body_hash}}

# The 304 refreshed the entry: fresh again
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; hit; ttl=\d+$/
sha256 == {{body_hash}}

# Stale again, for a client that has it: validated by the backend, then 304 from the cache
GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
If-None-Match: {{etag}}
[Options]
delay: 3s
HTTP 304
[Asserts]
header "Cache-Status" matches /^traedis; fwd=stale; fwd-status=304; ttl=\d+$/
header "ETag" == {{etag}}
bytes count == 0

# A HEAD is forwarded as it is. imgproxy answers it without validator: it says nothing of the stored image,
# which stays stale (§4.3.5 only refreshes a response that the HEAD response identifies)
HEAD {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
[Options]
delay: 3s
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=stale; fwd-status=200"

GET {{base_url}}/imaging-validate/B_7JSu2g6c6gISXWevLdHUvpMHw_HJm5zrr9TOlxVSk/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" matches /^traedis; fwd=stale; fwd-status=304; ttl=\d+$/
sha256 == {{body_hash}}
