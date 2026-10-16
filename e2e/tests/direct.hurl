# Routes without the middleware: the cache is not involved.

GET {{base_url}}/whoami?run={{run}}
[Options]
variable: run={{newUuid}}
HTTP 200
[Asserts]
header "Cache-Status" not exists

GET {{base_url}}/whoami?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" not exists
header "Age" not exists

GET {{base_url}}/placeholder/600x600?run={{run}}
HTTP 200
[Asserts]
header "Cache-Status" not exists

GET {{base_url}}/imaging/U6NZLzloNkMmeJshefxKRK36TMgQMhaHi6VKufelZJU/rs:fill:300:200/plain/http://placeholder:3000/600x600@avif?run={{run}}
HTTP 200
[Asserts]
header "Content-Type" == "image/avif"
header "Cache-Status" not exists
