# exposeCacheStatus: false hides Cache-Status from clients. The cache works as before: Age, which RFC 9111 §4 requires
# on a stored response, still tells a hit

GET {{base_url}}/whoami-hidden?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" not exists
header "Age" not exists

GET {{base_url}}/whoami-hidden?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" not exists
header "Age" exists

POST {{base_url}}/whoami-hidden?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" not exists
