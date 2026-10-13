# Range requests (RFC 9110 §14): the cache answers with the bytes asked for, from the whole response it stored.

GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Captures]
etag: header "ETag"
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"

GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
HTTP 200
[Captures]
length: header "Content-Length"
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
Range: bytes=4-11
HTTP 206
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Content-Range" == "bytes 4-11/{{length}}"
header "Content-Length" == "8"
header "Content-Type" == "image/avif"
header "ETag" == {{etag}}
# "ftypavif": what an AVIF file starts with, after its first four bytes
bytes == hex,6674797061766966;

# If-Range with the stored entity tag
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
Range: bytes=4-11
If-Range: {{etag}}
HTTP 206
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
bytes count == 8

# With another one: the whole response
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
Range: bytes=4-11
If-Range: "another"
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Content-Length" == "{{length}}"

# The condition of the client comes first
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
Range: bytes=4-11
If-None-Match: {{etag}}
HTTP 304
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="

# No such bytes
GET {{base_url}}/imaging-cache/unsafe/rs:fill:300:200/plain/http://placeholder:3000/600x400@avif?range={{run}}
Range: bytes=99999999-
HTTP 416
[Asserts]
header "Cache-Status" contains "traedis; hit; ttl="
header "Content-Range" == "bytes */{{length}}"
