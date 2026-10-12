# The first requests through a Traefik that has just started are sometimes too slow for `redis.timeout`:
# resolving the name of Redis alone can then take more than its 50 ms. The cache is bypassed for them,
# as it must be, but the tests expect it to answer.

# Retried until the cache answers
GET {{base_url}}/whoami-cache?warmup={{newUuid}}
[Options]
retry: 20
retry-interval: 500ms
HTTP 200
[Asserts]
header "Cache-Status" contains "traedis; fwd=uri-miss; fwd-status=200"
