# Traedis specification

How Traedis decides what to cache and what to serve. The [README](README.md) covers installation, the
[configuration](README.md#configuration) and the [HTTP headers](README.md#http-headers) that Traedis reads and adds. Together, both
documents specify its behaviour.

- [Caching rules](#caching-rules): [cacheable responses](#cacheable-responses), [variants](#variants),
  [conditional requests](#conditional-requests), [range requests](#range-requests), [stale responses](#stale-responses),
  [collapsed requests](#collapsed-requests), [invalidation by unsafe methods](#invalidation-by-unsafe-methods),
  [Redis failures](#redis-failures)
- [Security](#security): [personalized responses](#personalized-responses), [middlewares and cache key](#middlewares-and-cache-key),
  [abuse by clients](#abuse-by-clients), [Redis](#redis)

## Caching rules

### Cacheable responses

Traedis caches responses per middleware and per URL (scheme, host with its case, path and query). A response is cached only when all of
these hold:

- the request is a `GET` (a `HEAD` is answered from the cached `GET`);
- the status is listed in `statusCodes`;
- the response is fresh, through `s-maxage`, `max-age`, `Expires` or, when set, `defaultTtl`. `defaultTtl` never applies when the request
  has `Authorization`, an `authorizationHeaders` header or `Cookie`, or when the response has `Set-Cookie`;
- no [header](README.md#http-headers) of the request or the response forbids it;
- it is not a redirection to its own URL (ignoring the default port and the host's case), which would send every client back to it in a
  loop;
- it is complete (a response that the backend cuts short is not cached) and fits in `maxBodyBytes`, and in `maxBufferedBytes` along with
  the other [bodies held in memory](#abuse-by-clients).

Conditional and range requests are forwarded as is when nothing is cached. The backend's `304` or `206` goes to the client without being
cached: a URL is cached once a client requests all of it, without already having it.

Responses are written to Redis after the client receives them, so a miss never waits for Redis. A request for the same URL that arrives
during the write waits for it, up to `redis.dialTimeout`, so that a miss is followed by a hit. At most `redis.poolSize` writes run at once,
and one per URL. Past that, the response is not cached, and the next one for its URL is.

`OPTIONS`, `TRACE`, WebSocket upgrades, `Accept: text/event-stream` requests and requests with a body are forwarded as is. So are unsafe
methods (`POST`, `PUT`, `DELETE`…), which [invalidate](#invalidation-by-unsafe-methods) the cached responses.

The Redis key is `traedis:<middleware>:<hash of the URL>`, where `<middleware>` is the name Traefik gives the middleware (`cache@file`).
All the routers that use the same middleware share its cached responses, in all the Traefik instances. Two middlewares never do, even in
the same Redis database and for the same URL. Renaming a middleware therefore starts it with an empty cache.

### Variants

With `Vary`, the cache stores one variant per value of the request headers it names ([RFC 9111 §4.1][rfc9111-4.1]). With
`Vary: Accept-Language`, `fr` and `en` requests each get their own cached response.

- The backend decides. No option adds or ignores a header, and the `Vary` sent to clients is never rewritten.
- Values are compared as they are, apart from surrounding spaces and repeated header lines. `fr-FR` and `fr-fr` are two variants, and a
  request without the header has its own.
- At most `maxVariants` variants are cached per URL. Further ones are forwarded and not cached. Cached variants are never evicted to make
  room, not even when `maxVariants` is lowered. Since clients choose these values, the worst case for one URL is `maxVariants` ×
  `maxBodyBytes` in Redis.

`Accept-Encoding` is the exception, because clients spell it in many ways (`gzip, br`, `br;q=1.0, gzip;q=0.8`…). The request is forwarded
untouched, and its response is cached by `Content-Encoding` (`zstd`, `br`, `gzip`, `deflate` or none). It is then served to every request
that accepts that coding.

- A request never gets a coding it does not accept. `q=0` and `*` are honoured, and a request without `Accept-Encoding` only gets an
  uncompressed response. Clients cannot create variants by changing their `Accept-Encoding`.
- Among the cached codings that a request accepts, the first of `zstd`, `br`, `gzip`, `deflate` is served, whatever its other weights. A
  browser gets the cached `gzip` variant until it is stale, even from a backend that would have sent it `br`.
- An uncompressed response is only served to requests that accept no more codings than the request it was cached for. A `curl` without
  `Accept-Encoding` cannot make browsers get it.
- A response with `Vary: Accept-Encoding` and another `Content-Encoding`, or several, is not cached.

> **Note:** Traefik's `compress` middleware only adds `Vary: Accept-Encoding` to the responses it compresses. Placed after the cache, its
> uncompressed responses have no `Vary` and are served to everyone. Put `compress` before the cache, or set `Vary: Accept-Encoding` on
> every response with a `headers` middleware, as [`example/dynamic/whoami.yml`](example/dynamic/whoami.yml) does.

### Conditional requests

The cache plays both sides of `If-None-Match`, `If-Modified-Since` and `304 Not Modified` ([RFC 9111 §4.3][rfc9111-4.3]).

**Towards clients**, it answers `304` from a cached response when its `ETag` is listed in `If-None-Match` (weak comparison), or when it was
not modified since `If-Modified-Since` (according to its `Last-Modified`, otherwise its `Date`). `If-Match` and `If-Unmodified-Since` are
always left to the backend.

**Towards the backend**, it validates a cached response that may no longer be served (stale, or too old for the request's `max-age`). It
sends the response's `ETag` and `Last-Modified` instead of the client's conditions. The background refresh of `stale-while-revalidate`
does the same.

- A `304` with the cached response's validator updates its headers and makes it fresh again. The response is served, as a `304` in turn
  to a client that already has it. The `Content-Type`, `Content-Encoding`, `Content-Language`, `Content-Range` and `Vary` of the `304` are
  ignored: they describe the cached body, which a `304` does not replace.
- A `304` without a validator: the cached response is served but stays stale. With a different validator: the backend is asked again for
  a whole response.
- Any other response is sent to the client as is and replaces the cached one.
- Without a validator, the cache asks for a whole response. This is the case for a cached response with neither `ETag` nor
  `Last-Modified`, and for a variant that is not cached (the other variants' `ETag` is not sent). Responses are not cached just to be
  validated either: `no-cache`, `max-age=0` and already expired responses are still not cached.

A `HEAD` request is never made conditional. A `HEAD` response with the `ETag` or the `Last-Modified` of the cached response (and its
length) refreshes it like a `304`. A different one leaves it as is: it answers one client's request, which must not remove what the others
get (RFC 9111 §4.3.5 only says that the cached response SHOULD be considered stale).

### Range requests

Only whole responses are cached. A `GET` for some bytes of one (`Range: bytes=0-1023`) is answered from it, as the backend would
([RFC 9110 §14][rfc9110-14]):

- with a `206 Partial Content` that holds these bytes, in several parts when several ranges are requested, or with a `416` when none
  exist;
- with the whole response when `If-Range` is neither the `ETag` nor the `Last-Modified` of the cached one, when the backend sent
  `Accept-Ranges: none`, or when more than 16 ranges are requested. Each range would be a part with its own headers, and many small ones
  (`bytes=0-0,2-2,…`) would make the answer many times larger than the request;
- with a `304` when `If-None-Match` or `If-Modified-Since` says that the client already has the response. These
  [conditions](#conditional-requests) come first.

### Stale responses

A cached response that is no longer fresh is only served when allowed, and for at most `staleTtl`:

- **`stale-while-revalidate`** ([RFC 5861][rfc5861]): it is served at once and refreshed in the background. There is at most one refresh
  per URL, and `maxRevalidations` at a time. Past that, the refresh is left to a later request. A request with `max-age` or `min-fresh`
  (a browser reload) waits for the backend instead.

  The refresh replays the request that got the stale response, with its headers. A response that may not be cached leaves the cached one
  as is, since it may only concern that request (`private` or `Set-Cookie` for its cookies, an error for its `Authorization`, a body too
  large for its headers…). Only a `404` or a `410` removes it: the resource is gone. The same goes for a `304` that may not be cached, or
  that is about another response.
- **`stale-if-error`**: it replaces a `500`, `502`, `503` or `504`, including Traefik's own when the backend is unreachable. A request
  with `max-age` or `min-fresh` still gets the stale response. One with `no-cache` does not.
- **`max-stale`** on the request ([RFC 9111 §5.2.1.2][rfc9111-5.2.1.2]), with `followClientCacheControl: true`: it is served as is,
  without asking the backend or refreshing it.

As [RFC 9111][rfc9111] requires, `must-revalidate`, `proxy-revalidate`, `s-maxage` and `no-cache` forbid all three.
`max-age=60, stale-while-revalidate=300` is served stale, `s-maxage=60, stale-while-revalidate=300` is not.

For backends that don't send these directives, `defaultStaleWhileRevalidate` and `defaultStaleIfError` set the windows instead. For
example, `defaultStaleIfError: 10m` serves the cached response rather than a `5xx`. A directive of the response always wins
(`stale-if-error=0` still turns it off), and the rules above still apply. With `defaultStaleWhileRevalidate`, clients can get a response
older than what the backend asked for. On a rarely requested URL, it can be older by the whole window.

An expired response is only kept in Redis while it may still be used:

- for `staleTtl`, when it has an `ETag` or a `Last-Modified`, which let the cache [validate](#conditional-requests) it, or with
  `followClientCacheControl: true`, for `max-stale`;
- otherwise, for the longest of its `stale-while-revalidate` and `stale-if-error` windows;
- without any of these, it is removed as soon as it expires.

### Collapsed requests

With `collapseTimeout`, requests for a URL that is being fetched wait for its response instead of all reaching the backend: a cold or
expired URL that many clients request at once costs the backend one request ([RFC 9211 §2.6][rfc9211-2.6]). It is off by default.

- **Which requests wait.** A `GET` or a `HEAD` that nothing cached can answer: a miss, or a stale response that may not be served. They
  wait for a `GET` already forwarded for the same URL, through the same middleware and Traefik instance. Without one, the request is
  forwarded, and the next ones wait for it, unless its response cannot be cached for them: a `HEAD`, a request with `no-store`
  (`followClientCacheControl: true`), or a conditional one that the cache has no validator to replace. Requests whose `max-age` or
  `min-fresh` refuse a fresh response, and `only-if-cached`, never wait.
- **Once the response is cached**, the requests that waited look for it, as any request does, with their own headers. The variant they
  select, their conditions and their `Range` apply: a request is never sent a response that it would not get from the cache.
  `Cache-Status` reads `fwd=uri-miss; ttl=N; collapsed` (or `fwd=vary-miss`, `fwd=stale`). One that selects another variant waits for the
  next request forwarded, or is forwarded and the next ones wait for it.
- **When the response will not be cached**, the requests that wait are forwarded at once, together, as soon as its status and header say
  so (`private`, `no-store`, `Set-Cookie`, a status outside `statusCodes`, a `304` that leaves the cached response as is…). A body larger
  than `maxBodyBytes`, a backend that fails or a client that goes away let them go once the request ends.
- **They wait for at most `collapseTimeout`**, whatever the request forwarded is doing, and are then forwarded. Its response is cached once
  its client has received all of it: a slow client makes the others wait, up to that bound. A few seconds is enough for most backends.

The cost is for URLs whose responses are never cached: the requests that arrive while one of them is forwarded wait for its status before
they are forwarded too. Leave `collapseTimeout` at `0` when concurrent requests for the same uncacheable URL are the norm.

### Invalidation by unsafe methods

When the backend answers an unsafe method (any method but `GET`, `HEAD`, `OPTIONS` and `TRACE`) with a `2xx` or a `3xx`, the cache
removes the URL's cached responses, with all their variants ([RFC 9111 §4.4][rfc9111-4.4]).

- They are removed before the client gets the answer, so its next `GET` is not served the old response. The answer itself is never held
  back.
- An error (`4xx`, `5xx`) changes nothing. The cached responses are kept, also for `stale-if-error`.
- Only the request's URL is invalidated, including its query and the case of its host, and only for the middleware that the request goes
  through. `POST /cart/add` does not invalidate `/cart`. A `PUT` to `API.example.com` does not invalidate `api.example.com`, which is what
  browsers always send. The response's `Location` and `Content-Location` are not followed.
- There is no guarantee. If Redis cannot be reached at that moment, the cached responses stay until they expire. A `GET` already on its
  way to the backend can also cache its response after the invalidation.
- Any client can trigger it, even without credentials, when the backend answers with a `2xx` or a `3xx`. See
  [abuse by clients](#abuse-by-clients).

### Redis failures

The cache never fails a request because of Redis. Whatever Redis cannot answer in time is forwarded to the backend with
`Cache-Status: traedis; fwd=bypass; detail=redis`, and its response is not cached.

- Getting a connection takes at most `redis.dialTimeout`, and the whole operation at most `redis.timeout`. Redis gets more time to send a
  large body than to show that it is there.
- A connection closed while idle (Redis restarted, a firewall dropped it) is replaced without a bypass.
- After `redis.maxFailures` consecutive failures, Redis is no longer asked. Requests are forwarded at once, instead of each one waiting for
  a timeout. One of them tries Redis again every `redis.retryAfter`, and the cache comes back as soon as Redis answers.
- Traefik's log reports it in lines that start with `traedis: <middleware>:`. It reports the failures at most once a minute, then when
  Redis is given up on and when it is back.

## Security

A shared cache serves the response made for one client to all the others. The defaults are careful, but the cache only knows what the
backend and the configuration tell it. The [README](README.md#security) sums up what to check.

### Personalized responses

- **`defaultTtl` caches what the backend said nothing about.** A backend that sends no `Cache-Control` has often not thought about caches,
  and the cache cannot always tell its personalized pages from the others. Set `defaultTtl` only for the backends whose responses without
  `Cache-Control` are the same for every client.
- **A cookie does not make a response private.** Cookies are not part of the cache key. A response with `max-age`, `s-maxage` or `Expires`
  is cached and shared even when its request had a `Cookie`. Whatever depends on the session must send `Cache-Control: private` or
  `no-store`. Likewise, `public`, `s-maxage` and `must-revalidate` allow sharing the response to a request with `Authorization` or an
  `authorizationHeaders` header.
- **`private="Set-Cookie"` only keeps the cookie out.** The other headers and the body are served to everyone. Don't use it when they
  depend on the cookie (CSRF token, user name…).
- **The cache only knows `Authorization`.** Some requests look anonymous to it: those from a middleware that replaces `Authorization` with
  its own header (`basicAuth` with `removeHeader` and `headerField`, `forwardAuth` with `authResponseHeaders`), and those with an API key
  in a header. With a `defaultTtl`, the response they get without `Cache-Control` is cached, and one user's page is served to the others.
  List these headers in `authorizationHeaders` (`[X-WebAuth-User]`), so that their requests are treated like those with `Authorization`.
- **Redirections are shared too.** Take a backend that sends the same `Cache-Control: public, max-age=…` on all its responses. Its
  redirection of anonymous clients to its login page is cached, then served to the clients that are logged in. Have it send `private` or
  `no-store` with whatever depends on the client, redirections included, or leave `3xx` out of `statusCodes` (`["200-299"]`).

### Middlewares and cache key

- **Middlewares listed after the cache don't run on a hit.** Put `basicAuth`, `forwardAuth`, `ipAllowList` and `rateLimit` before it. The
  cache does not know whom they let in: what it caches is shared by all of them.
- **The key is the URL that the cache sees, not the router.** The routers that use the same cache middleware share its cached responses.
  Say two of them answer the same URL with different backends, one restricted to some clients (by `ClientIP`, a header, an entry point…)
  and the other not. Each one then serves what the other cached. A middleware that rewrites the URL before the cache (`stripPrefix`,
  `replacePath`…) has the same effect: `/v1/users` and `/v2/users`, both stripped to `/users`, share their cached responses. Give such
  routers a cache middleware each, and rewrite the URL after it.
- **What the response depends on must be in `Vary`.** When the backend reads a request header that its `Vary` does not name (`Forwarded`,
  `X-Original-URL`, `User-Agent`, `Accept-Language`…), one client chooses what the others get. Traefik replaces the `X-Forwarded-*`
  headers of the clients it does not trust, but not these. With `followClientCacheControl: true`, that client also chooses when:
  `Cache-Control: no-cache` replaces the cached response with what the backend answers to its request.

  The cache only recognizes one such response: a redirection to its own URL, which it never caches. A backend sends one when it takes the
  scheme from a header that Traefik leaves to the clients (`X-Forwarded-Scheme` or `X-Forwarded-Ssl` for Rails' `force_ssl`). Cached for
  `defaultTtl`, it would send every client into a loop. With a `statusCodes` that lists errors, such a header can also get an error cached
  for everyone (`X-HTTP-Method-Override` makes some backends answer `405`). The host is not one of these headers: the key keeps its case,
  as the backend gets it.
- **Behind a proxy that terminates TLS, Traefik has to trust it.** The key's scheme comes from `X-Forwarded-Proto`. Traefik sets it to the
  connection's scheme, unless a proxy it trusts sent it: list that proxy in the entry point's `forwardedHeaders.trustedIPs`. Otherwise,
  `https` requests reach Traefik over `http` like the others, and share their cached responses. A redirection of the `http` requests to
  `https` then answers the `https` ones too, which follow it in a loop.

### Abuse by clients

- **Clients can always reach the backend.** A random query string is a miss, and `Cache-Control: no-cache` skips the cache with
  `followClientCacheControl: true`. Put a `rateLimit` before the cache.
- **Clients can tell what others requested.** A response served from the cache has a `Cache-Status` that says so, and an `Age`, which
  [RFC 9111 §4][rfc9111] requires. A client learns whether someone else requested a URL (`/export?user=42`), and when. With
  `exposeCacheStatus: false`, clients no longer see what the cache did (the backend's `fwd-status`, `detail=redis`…), but `Age` still
  reveals a hit. `only-if-cached` would tell it without caching the URL, which is why it is ignored by default.
- **Clients can make others wait**, with `collapseTimeout`: the request of a client that reads its response slowly is the one that the
  next requests for its URL [wait for](#collapsed-requests), for at most `collapseTimeout`.
- **Bodies are held in memory.** A response is copied in full to be cached, and a cached one is read in full from Redis to be sent. Each
  one stays in Traefik's memory until its client has received all of it, which a slow client makes last. Past `maxBufferedBytes` for all
  of them, the cache steps aside (`fwd=bypass; detail=memory`). A response is sent without being cached, a cached one is not sent, and the
  backend gets the requests. Bodies up to 32 KiB are not counted, since Traefik buffers that much anyway. Put an `inFlightReq` before the
  cache: `rateLimit` bounds how often a client sends requests, not how many it keeps waiting at once.
- **Clients can remove cached responses.** As [RFC 9111 §4.4][rfc9111-4.4] requires, a `2xx` or `3xx` answer to an unsafe method removes
  the URL's cached responses for everyone, even for a method that the backend does not know (`FOO`). Some backends answer any method like
  a `GET`, or redirect an anonymous `POST` to their login page. Any client can then empty the cache of any URL as often as it wants. It
  also chooses when the URL is cached again, and with what a header not named by `Vary` makes the backend answer. Have the backend refuse
  the methods it does not handle with a `405`, or let only those through with the router rule, as for a read-only site:
  ``Host(`app.example.com`) && (Method(`GET`) || Method(`HEAD`))``. Safe methods remove nothing, except a `404` or a `410` that the
  [background refresh](#stale-responses) of a stale response gets.
- **Cache pollution and cache flooding.** Each URL has its own cached response, and the cache cannot know which query parameters the
  backend ignores. `?x=1`, `?x=2`… cache the same response again and again, each one up to `maxBodyBytes`, for its freshness plus at most
  `staleTtl` (see [stale responses](#stale-responses)). Nothing bounds the number of URLs. A client can fill Redis with responses that
  nobody requests again (flooding), which then take the place of the useful ones (pollution). Give Redis a `maxmemory` with the
  `allkeys-lfu` policy, so that responses requested once are evicted first, and have the backend refuse the URLs it does not know. With
  the default `statusCodes`, no error is cached. A redirection is, with `s-maxage`, `max-age` or `Expires`, and so is a `301` or a `308`
  without them when `defaultTtl` is set.

### Redis

- **Redis is trusted.** Whoever writes to it chooses what clients are served, and whoever reads it gets the cached responses. Use it for
  the cache only. With `redis://`, the connection is not encrypted, password included: keep it on a private network, or use `rediss://`.

[rfc5861]: https://www.rfc-editor.org/rfc/rfc5861
[rfc9110-14]: https://www.rfc-editor.org/rfc/rfc9110#section-14
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
[rfc9111-4.1]: https://www.rfc-editor.org/rfc/rfc9111#section-4.1
[rfc9111-4.3]: https://www.rfc-editor.org/rfc/rfc9111#section-4.3
[rfc9111-4.4]: https://www.rfc-editor.org/rfc/rfc9111#section-4.4
[rfc9111-5.2.1.2]: https://www.rfc-editor.org/rfc/rfc9111#section-5.2.1.2
[rfc9211-2.6]: https://www.rfc-editor.org/rfc/rfc9211#section-2.6
