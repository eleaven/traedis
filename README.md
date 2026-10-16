> ⚠️ **This project is in its early stages.** Use it at your own risk.

<!-- The Traefik Plugin Catalog shows this file on another site, where it rewrites every https://github.com/ link to
     raw.githubusercontent.com. So the links are absolute: to a file through /blob/, which that turns into the file itself, and to
     anything else (a directory, a workflow) through www.github.com, which it leaves alone. It does not know the alerts of GitHub
     either (`> [!NOTE]`), which it shows as they are written -->
<!--suppress HtmlDeprecatedAttribute -->
<p align="center">
  <img src="https://raw.githubusercontent.com/eleaven/traedis/main/assets/icon.svg" alt="Traedis Logo" height="360">
</p>

# Traedis

[![Release](https://img.shields.io/github/v/release/eleaven/traedis)][releases]
[![CI](https://img.shields.io/github/actions/workflow/status/eleaven/traedis/ci.yml?branch=main&label=CI)][ci]

**Redis**-backed HTTP cache for **Traefik**, shipped as a **middleware plugin**.

- **Standards-based**: follows [RFC 9111][rfc9111] (`Cache-Control`, `Vary`, `ETag`, `304 Not Modified`…), [RFC 5861][rfc5861]
  (`stale-while-revalidate`, `stale-if-error`) and [RFC 9211][rfc9211] (`Cache-Status`).
- **Dependency-free**: only uses the Go standard library, with no third-party module and no `unsafe`.
- **Safe by default**: never caches what must not be shared (`Authorization`, `Set-Cookie`, `private`) unless the backend explicitly allows
  it. See [Security](#security) for the pitfalls.
- **Fail open**: if Redis is slow or down, requests go straight to the backend, and Traefik still starts. See
  [Redis failures](#redis-failures).
- **Observable**: every response tells what the cache did in a [`Cache-Status`](#added-by-traedis) header.
- **Stream-friendly**: large bodies, WebSockets and server-sent events pass through untouched.
- **Persistent and shared**: responses are stored in Redis, not in Traefik's memory: the cache survives restarts and redeploys, and all
  Traefik replicas share it.

## Installation

Requires Traefik v3 and Redis ≥ 8.0 (for `HSETEX`).

Declare the plugin in the Traefik **static** configuration:

```yaml
experimental:
  plugins:
    traedis:
      moduleName: github.com/eleaven/traedis
      version: vX.Y.Z # replace with the release you want
```

Then declare the middleware in the **dynamic** configuration, and add it to your routers:

```yaml
http:
  middlewares:
    cache:
      plugin:
        traedis:
          redis:
            dsn: "redis://:password@redis:6379/0"

  routers:
    my-app:
      rule: Host(`app.example.com`)
      service: my-app
      middlewares:
        - cache
```

Authentication and rate limiting go before the cache: see [Security](#security). A complete local setup (Traefik, Redis, sample backends) is
available in [`compose.yml`][compose] and [`example/`][example].

## Configuration

All keys are optional. Durations use the [Go syntax](https://pkg.go.dev/time#ParseDuration) (`50ms`, `5m`, `1h`).

| Key                            | Default                | Description                                                                                                                                    |
|--------------------------------|------------------------|------------------------------------------------------------------------------------------------------------------------------------------------|
| `redis.dsn`                    | `redis://cache:6379/0` | `redis://[user[:password]@]host[:port][/db]`, or `rediss://` for TLS: the certificate of Redis is then verified.                               |
| `redis.tls.ca`                 | system authorities     | With `rediss://`, the authorities that may have signed the certificate of Redis: a PEM file, or its content.                                   |
| `redis.tls.insecureSkipVerify` | `false`                | With `rediss://`, accept any certificate: the connection is encrypted, but Redis may be anyone.                                                |
| `redis.poolSize`               | `10`                   | Connections to Redis, at most, for each middleware in each Traefik instance. A request that finds them all busy waits for one.                 |
| `redis.dialTimeout`            | `50ms`                 | Maximum time to get a connection to Redis: a free one of the pool, or a new one. Past it, the cache is bypassed.                               |
| `redis.timeout`                | `1s`                   | Maximum time for a whole Redis operation, getting the connection and transferring a body included. Past it, the cache is bypassed.             |
| `redis.maxFailures`            | `5`                    | Failures in a row after which Redis is [no longer asked](#redis-failures). `0`: every request tries it.                                        |
| `redis.retryAfter`             | `1s`                   | How often Redis is tried again once it is no longer asked.                                                                                     |
| `statusCodes`                  | `[200]`                | Status codes allowed to be stored, at least one. It only narrows what [RFC 9111][rfc9111] allows; it never makes a response cacheable.         |
| `defaultTtl`                   | `5m`                   | Freshness when the backend sends no `max-age`, `s-maxage` or `Expires`. `0` disables it.                                                       |
| `staleTtl`                     | `1h`                   | How long an entry is kept in Redis after it expires: the upper bound of every [stale response](#stale-responses). `0` disables them.           |
| `defaultStaleWhileRevalidate`  | `0s`                   | `stale-while-revalidate` window for responses that don't send the directive, at most `staleTtl`. `0` disables it.                              |
| `defaultStaleIfError`          | `0s`                   | `stale-if-error` window for responses that don't send the directive, at most `staleTtl`. `0` disables it.                                      |
| `maxBodyBytes`                 | `5242880` (5 MiB)      | Larger responses are streamed to the client and not cached. At most 512 MiB, the largest value Redis takes.                                    |
| `maxBufferedBytes`             | `67108864` (64 MiB)    | [Bodies held in memory](#security) at once, in each Traefik instance: past it, the cache is bypassed. At least `maxBodyBytes`.                 |
| `maxVariants`                  | `16`                   | Stored [variants](#variants) per URL. `0`: responses with `Vary` are not cached.                                                               |
| `maxRevalidations`             | `10`                   | [Background refreshes](#stale-responses) running at once, for each middleware in each Traefik instance.                                        |
| `revalidationTimeout`          | `30s`                  | Maximum time of a background refresh.                                                                                                          |
| `sortQuery`                    | `false`                | Sort the query parameters by name in the cache key: `?a=1&b=2` and `?b=2&a=1` share the same entry. Only for backends that ignore their order. |
| `exposeKey`                    | `false`                | Add the [Redis key](#cacheable-responses) to the `Cache-Status` header (`key="…"`).                                                            |

## HTTP headers

Traedis is driven by the standard HTTP header fields sent by the backend, the client or added by Traedis itself.

### Sent by the backend

The response of the backend says whether it may be stored, for how long, and whether it may be served stale.

- `Cache-Control`, a list of directives separated by commas (`max-age=60, stale-if-error=300`):
  - `max-age=N`: fresh for `N` seconds.
  - `s-maxage=N`: the same, and it wins over `max-age`. Never served [stale](#stale-responses).
  - `no-store`, `private`, `no-cache`: do not store the response.
  - `private="Set-Cookie"`, `no-cache="Set-Cookie"`: stored without the header fields they name, which only the client that caused the
    miss gets.
  - `public`: stored even when the request has `Authorization`.
  - `must-revalidate`, `proxy-revalidate`: never served stale.
  - `stale-while-revalidate=N`: once stale, still served for `N` seconds, and refreshed in the background.
  - `stale-if-error=N`: once stale, still served for `N` seconds in place of an error of the backend.
- `Expires`: until when it is fresh, without `max-age` nor `s-maxage`. Without any of the three, `defaultTtl` applies, unless the exchange
  has `Authorization`, `Cookie` or `Set-Cookie`.
- `Age`, `Date`: how old the response already is. It is stored for the freshness it has left.
- `Vary`: one stored [variant](#variants) per value of the request header fields it names. Not stored with `*`, `Cookie` or
  `Authorization` (one variant per user).
- `ETag`, `Last-Modified`: let the cache [validate](#conditional-requests) a stale response instead of getting it
  again, and answer `304` to the clients that already have it.
- `Set-Cookie`: not stored, even `public`, unless `private="Set-Cookie"` or `no-cache="Set-Cookie"` keeps the cookie out.

### Sent by the client

The request of the client says whether a stored response may answer it.

- `Cache-Control`:
  - `no-cache`: always forwarded, whatever is stored. The response is stored.
  - `no-store`: the response is not stored. A stored one can still answer.
  - `max-age=N`: refuses a stored response older than `N` seconds.
  - `min-fresh=N`: refuses a stored response that is not fresh for `N` more seconds.
  - `max-stale=N`: accepts a response that is stale by less than `N` seconds; `max-stale` alone, by any time.
  - `only-if-cached`: never forwarded. When nothing stored can answer, it gets a `504` without `Cache-Status`. With `max-stale`, it is what
    an offline client sends.
- `If-None-Match`, `If-Modified-Since`: answered with a `304` when the stored response is the one the client
  [already has](#conditional-requests).
- `Range`, `If-Range`: answered with the bytes asked for of the stored response ([range requests](#range-requests)).
- `Authorization`: the response is not stored, unless the backend allows sharing it with `public`, `s-maxage` or `must-revalidate`.
- `Cookie`: not part of the cache key, see [Security](#security).
- `Accept-Encoding`, and the header fields that `Vary` names: choose the [variant](#variants).
- `Upgrade`, `Accept: text/event-stream`: forwarded untouched (WebSockets, server-sent events).
- `Content-Length`, `Transfer-Encoding`: a `GET` or `HEAD` with content is forwarded untouched, and its response is not stored. Some
  backends read parameters in it, which the cache key does not have.

### Added by Traedis

Traedis adds these to the responses it sends to the client, and to the requests it sends to the backend.

- `Age`: on a response served from the cache, the seconds since the backend made it.
- `If-None-Match`, `If-Modified-Since`: sent to the backend in place of those of the client, to
  [validate](#conditional-requests) a stored response.
- `Cache-Status: traedis; …`: on every response, what the cache did ([RFC 9211][rfc9211]):
  - `hit; ttl=N`: served from the cache, fresh for `N` more seconds.
  - `hit; ttl=-N; detail=stale-while-revalidate`: served from the cache, stale by `N` seconds, and refreshed in the background.
  - `hit; ttl=-N; detail=max-stale`: served from the cache, stale by `N` seconds, as the request allows.
  - `fwd=uri-miss`: forwarded, nothing is stored for this URL.
  - `fwd=vary-miss`: forwarded, this URL has [variants](#variants), but none for this request.
  - `fwd=stale`: forwarded, the stored response is stale.
  - `fwd=request`: forwarded, the request says `no-cache`, or its `max-age` or `min-fresh` refuses a stored response that is still fresh.
  - `fwd=stale; fwd-status=304; ttl=N`: [validated](#conditional-requests) by the backend, the stored response is
    served, fresh for `N` seconds again.
  - `fwd=stale; fwd-status=503; ttl=-N; detail=stale-if-error`: the backend failed, the stored response is served in place of its error.
  - `fwd=bypass`: forwarded untouched, WebSocket upgrade, server-sent events or request with content.
  - `fwd=bypass; detail=redis`: forwarded, Redis [failed, timed out or is no longer asked](#redis-failures).
  - `fwd=bypass; detail=memory`: forwarded, the bodies held in memory reach `maxBufferedBytes`: the stored response is not sent from the
    cache (see [Security](#security)).
  - `fwd=method`: forwarded, `OPTIONS`, `TRACE` or an unsafe method.
  - `fwd=method; detail=invalidated`: unsafe method, the stored responses of the URL were [removed](#invalidation-by-unsafe-methods)
    (`detail=redis`: Redis could not be reached).
  - `fwd-status=N`: the status the backend answered with, whatever the client gets.
  - `key="…"`: the [Redis key](#cacheable-responses), enabled by the `exposeKey` option, to help debug the cache.

## Caching rules

### Cacheable responses

Responses are stored by middleware and by URL (scheme, host, path and query), and only when all of the following hold:

- the request is a `GET` (a `HEAD` is answered from the stored `GET`);
- its status is listed in `statusCodes`;
- it is fresh, through `s-maxage`, `max-age`, `Expires` or `defaultTtl`;
- no [header field](#http-headers) of the request or of the response forbids it;
- it is complete (a response that the backend cuts short is not stored) and fits in `maxBodyBytes`, and in `maxBufferedBytes` along with
  the other bodies held in memory (see [Security](#security)).

A response is stored once it is sent: its client never waits for Redis. The next request for the same URL does, for no longer than
`redis.dialTimeout`, so that a miss is followed by a hit. No more responses are being stored at a time than `redis.poolSize`, and one per
URL: past that, a response is not stored, and the next one for its URL is.

`OPTIONS`, `TRACE`, WebSocket upgrades, `Accept: text/event-stream` requests and requests with content are forwarded as is. So are
unsafe methods (`POST`, `PUT`, `DELETE`…), which [invalidate](#invalidation-by-unsafe-methods) what is stored.

The Redis key is `traedis:<middleware>:<hash of the URL>`, with the name Traefik gives the middleware (`cache@file`). The routers that use
the same middleware share its stored responses, in all the Traefik instances; two middlewares never do, even in the same Redis database
and for the same URL: renaming a middleware starts it with an empty cache.

### Variants

With `Vary`, the cache stores one variant per value of the request headers it names
([RFC 9111 §4.1][rfc9111-4.1]): with `Vary: Accept-Language`, `fr` and `en` requests each get their own stored response.

- The backend decides: there is no option to add or ignore a header, and the `Vary` sent to clients is never rewritten.
- Values are compared as they are, apart from surrounding spaces and repeated header lines: `fr-FR` and `fr-fr` are two variants, and a
  request without the header has its own.
- At most `maxVariants` variants are stored per URL: further ones are forwarded and not stored. Stored ones are never evicted to make room,
  not even when `maxVariants` is lowered. Clients choose these values: the worst case for one URL is `maxVariants` × `maxBodyBytes` in
  Redis.

`Accept-Encoding` is the exception, because clients spell it in many ways (`gzip, br`, `br;q=1.0, gzip;q=0.8`…). The request reaches the
backend untouched, and its response is stored by `Content-Encoding` (`zstd`, `br`, `gzip`, `deflate` or none), then served to every request
that accepts that coding.

- A request never gets a coding it does not accept (`q=0` and `*` are honoured; without `Accept-Encoding`, only an uncompressed response),
  and clients cannot create variants by changing their `Accept-Encoding`.
- Among the stored codings a request accepts, the first of `zstd`, `br`, `gzip`, `deflate` is served, whatever its other weights: a browser
  gets the stored `gzip` variant until it is stale, even from a backend that would have sent it `br`.
- An uncompressed response is only served to requests accepting no more codings than the request it was stored for: a `curl` without
  `Accept-Encoding` cannot make browsers get it.
- A response with `Vary: Accept-Encoding` and another `Content-Encoding`, or several, is not cached.

> **Note:** Traefik's `compress` middleware only adds `Vary: Accept-Encoding` to the responses it compresses: placed after the cache, its
> uncompressed response has no `Vary` and is served to everyone. Put `compress` before the cache, or set `Vary: Accept-Encoding` on every
> response with a `headers` middleware, as [`example/dynamic/whoami.yml`][whoami] does.

### Conditional requests

The cache plays both parts of `If-None-Match`, `If-Modified-Since` and `304 Not Modified` ([RFC 9111 §4.3][rfc9111-4.3]).

**Towards its clients**, it answers `304` from a stored response whose `ETag` is one of `If-None-Match` (weak comparison), or that was not
modified since `If-Modified-Since` (its `Last-Modified`, otherwise its `Date`). With nothing stored, the request reaches the backend as it
is and its `304` goes to the client, without being stored: a URL is cached once a client asks for it without having it already. `If-Match`
and `If-Unmodified-Since` are always for the backend.

**Towards the backend**, it validates a stored response that may no longer be served (stale, or too old for the `max-age` of the request):
its `ETag` and `Last-Modified` are sent in place of the conditions of the client. The background refresh of `stale-while-revalidate` does
the same.

- A `304` that names the validator of the stored response updates its header fields and makes it fresh again: it is served, as a `304` in
  turn to a client that already has it.
- A `304` without validator: the stored response is served but stays stale. With another validator: the backend is asked again for a whole
  response.
- Any other response is sent to the client as it is and replaces the stored one.
- Nothing to validate with means a whole response: a stored response without `ETag` nor `Last-Modified`, or a variant that is not stored
  (the `ETag` of the other variants is not sent). Responses are not stored only to be validated either: `no-cache`, `max-age=0` and already
  expired ones are still not cached.

A `HEAD` request is never made conditional. A response with the `ETag` or the `Last-Modified` of the stored one (and its length) refreshes
it like a `304`; a different one drops a stored response that was still fresh.

### Range requests

Only whole responses are stored. A `GET` for some bytes of one (`Range: bytes=0-1023`) is answered from it, as the backend would
([RFC 9110 §14](https://www.rfc-editor.org/rfc/rfc9110#section-14)):

- with a `206 Partial Content` that has these bytes, in several parts when it asks for several ranges, or a `416` when there are none;
- with the whole response when its `If-Range` is not the `ETag` or the `Last-Modified` of the stored one, or when the backend said
  `Accept-Ranges: none`;
- with a `304` when its `If-None-Match` or `If-Modified-Since` says it already has the response: these [conditions](#conditional-requests)
  come first.

With nothing stored, the request reaches the backend as it is and its `206` goes to the client, without being stored: a URL is cached once
a client asks for all of it.

### Stale responses

A stored response that is no longer fresh is served only when allowed, for at most `staleTtl`:

- **`stale-while-revalidate`** ([RFC 5861][rfc5861]): it is served at once and refreshed in the background, with at most one refresh per
  URL and `maxRevalidations` at a time: past it, the refresh is left to a later request. Not for a request with `max-age` or `min-fresh`
  (a browser reload), which waits for the backend. The refresh is the request that got the stale response, with its header fields: a
  response that may not be stored removes the stored one, but for an error, which keeps it (`5xx`, `429`, or a `4xx` that may only be
  about that request: its `Authorization`, its cookies…). A `404` or a `410` still removes it: the resource is gone.
- **`stale-if-error`**: it replaces a `500`, `502`, `503` or `504`, including those of Traefik when the backend is unreachable. A request
  with `max-age` or `min-fresh` gets it too, one with `no-cache` does not.
- **`max-stale`** on the request ([RFC 9111 §5.2.1.2](https://www.rfc-editor.org/rfc/rfc9111#section-5.2.1.2)): it is served as it is,
  without asking the backend nor refreshing it.

As [RFC 9111][rfc9111] requires, `must-revalidate`, `proxy-revalidate`, `s-maxage` and `no-cache` forbid all three:
`max-age=60, stale-while-revalidate=300` is served stale, `s-maxage=60, stale-while-revalidate=300` is not.

For backends that don't send these directives, `defaultStaleWhileRevalidate` and `defaultStaleIfError` set the windows instead:
`defaultStaleIfError: 10m` serves what is stored rather than a `5xx`. A directive of the response always wins (`stale-if-error=0` still
turns it off), and the rules above still apply. With `defaultStaleWhileRevalidate`, clients can get a response older than what the backend
asked for: on a URL that is rarely requested, by up to the whole window.

### Invalidation by unsafe methods

When the backend answers a request with an unsafe method (any method that is not `GET`, `HEAD`, `OPTIONS` or `TRACE`) with a `2xx` or a
`3xx`, the stored responses of its URL are removed, with all their variants
([RFC 9111 §4.4][rfc9111-4.4]).

- They are removed before the client gets the answer: its next `GET` is not served what was stored before. The answer itself is never held
  back.
- An error (`4xx`, `5xx`) changes nothing: the stored responses are kept, also for `stale-if-error`.
- Only the URL of the request is invalidated, query included, and only for the middleware the request goes through: `POST /cart/add`
  does not invalidate `/cart`, and the `Location` and `Content-Location` of the response are not followed.
- There is no guarantee: if Redis cannot be reached at that moment, the stored responses stay until they expire, and a `GET` that was
  already on its way to the backend can store its response after the invalidation.
- Any client can trigger it, even without credentials when the backend answers it with a `2xx` or a `3xx`: see [Security](#security).

### Redis failures

The cache never fails a request because of Redis: what Redis cannot answer in time is forwarded to the backend, with
`Cache-Status: traedis; fwd=bypass; detail=redis`, and its response is not stored.

- Getting a connection takes at most `redis.dialTimeout`, and the whole operation `redis.timeout`: Redis has more time to send a large
  body than to show that it is there.
- A connection that was closed while it was idle (Redis restarted, a firewall dropped it) is replaced without a bypass.
- After `redis.maxFailures` failures in a row, Redis is no longer asked: requests are forwarded at once, instead of each one waiting for a
  timeout. One of them tries Redis again every `redis.retryAfter`, and the cache is back as soon as it answers.
- The log of Traefik tells it, in lines that start with `traedis: <middleware>:`: the failures, at most once a minute, then when Redis
  is given up on and when it is back.

## Security

A shared cache gives the response made for one client to all the others. The defaults are careful (see
[HTTP headers](#http-headers)), but the cache only knows what the backend and the configuration tell it:

- **A cookie does not make a response private.** Cookies are not part of the cache key: a response with `max-age`, `s-maxage` or `Expires`
  is stored and shared even when its request had a `Cookie` (only `defaultTtl` is never applied to an exchange with `Authorization`,
  `Cookie` or `Set-Cookie`). Whatever depends on the session must say `Cache-Control: private` or `no-store`. In the same way, `public`,
  `s-maxage` and `must-revalidate` allow sharing the response to a request with `Authorization`.
- **`private="Set-Cookie"` only keeps the cookie out.** The other header fields and the body are served to everyone: don't use it when they
  depend on the cookie (CSRF token, user name…).
- **Middlewares listed after the cache don't run on a hit.** `basicAuth`, `forwardAuth`, `ipAllowList` and `rateLimit` go before it. The
  cache does not know who they let in: what it stores is shared by all of them.
- **What the response depends on must be in `Vary`.** When the backend reads a request header that its `Vary` does not name
  (`X-Forwarded-Host`, `User-Agent`, `Accept-Language`…), one client chooses what the others get. The scheme is the one of the connection to
  Traefik, `X-Forwarded-Proto` is not trusted: behind a proxy that terminates TLS, `http` and `https` requests share their stored responses.
- **Clients can always reach the backend.** A random query string is a miss, `Cache-Control: no-cache` skips the cache: put a `rateLimit`
  before it.
- **Bodies are held in memory.** A response is copied in full to be stored, and a stored one is read in full from Redis to be sent: each
  one stays in the memory of Traefik until its client got it all, which a slow client makes last. Past `maxBufferedBytes` for all of them
  (bodies up to 32 KiB are not counted, no more than what Traefik buffers anyway), a response is sent without being stored, and a stored
  one is not sent from the cache (`fwd=bypass; detail=memory`): the backend gets the requests. Put an `inFlightReq` before the cache:
  `rateLimit` bounds how often a client sends requests, not how many of them it keeps waiting at once.
- **Clients can remove stored responses.** As [RFC 9111][rfc9111-4.4] requires, a `2xx` or a `3xx` answer to an unsafe method, even one
  the backend does not know (`FOO`), removes the stored responses of its URL for everyone. A backend that answers any method like a `GET`,
  or that redirects an anonymous `POST` to its login page, lets any client empty the cache of any URL as often as it wants, and choose when
  it is stored again (with what a header field that `Vary` does not name makes the backend answer). Have the backend refuse the methods it
  does not handle with a `405`, or let only those reach it with the rule of the router, as for a site that is only read:
  ``Host(`app.example.com`) && (Method(`GET`) || Method(`HEAD`))``.
- **Cache pollution and cache flooding.** Each URL has its own entry, and the cache cannot know which query parameters the backend ignores:
  `?x=1`, `?x=2`… store the same response again and again, each one up to `maxBodyBytes` and for its freshness plus `staleTtl`. Nothing
  bounds the number of URLs: a client can fill Redis with entries that nobody asks for again (flooding), which then take the place of the
  useful ones (pollution). Give Redis a `maxmemory` with the `allkeys-lfu` policy, so that entries requested once are evicted first, and
  have the backend refuse or redirect the URLs it does not know: with the default `statusCodes`, only a `200` is stored.
- **Redis is trusted.** Whoever writes to it chooses what clients are served, and whoever reads it gets the stored responses: keep it for
  the cache only. With `redis://`, the connection is not encrypted, password included: keep it on a private network, or use `rediss://`.

Hence the order of the middlewares of a router:

```yaml
http:
  routers:
    my-app:
      middlewares:
        - auth        # before the cache: a hit is served without running what comes after it
        - ratelimit   # before the cache: random URLs are misses that reach the backend
        - inflightreq # before the cache: each request may hold a body in memory
        - cache
```

## Performance

A hit is served from Traedis in about 1.5 ms, whatever the backend costs: the slower the backend, the larger the gain. A miss takes no longer
than the backend alone: its response is stored once the client has it.

Load test through Traefik with 10 concurrent clients, as requests per second and median latency:

| Backend | Without cache | Cache miss | Cache hit | Gain |
| --- | --: | --: | --: | --: |
| [Text answered after 100 ms][whoami] | 99.0 req/s – 102 ms | 97.0 req/s – 103 ms | 5,032 req/s – 1.48 ms | **×51** |
| [Rendered PNG][placeholder] | 98.4 req/s – 96.2 ms | 106 req/s – 87.7 ms | 4,598 req/s – 1.60 ms | **×47** |
| [Image converted to AVIF][imaging] | 85.7 req/s – 107 ms | 84.7 req/s – 107 ms | 4,768 req/s – 1.56 ms | **×56** |

These figures come from the [CI run of October 13, 2026][bench-run], on a shared GitHub Actions runner: read them as orders of magnitude.
Every [CI][ci] run publishes its own in its summary, with the p95 and the cost of the middleware alone. `make bench` runs them locally.

## Contributing

See [CONTRIBUTING.md][contributing] for the local stack, the tests and the rules of the project.

## License

[MIT][license]

[ci]: https://www.github.com/eleaven/traedis/actions/workflows/ci.yml
[releases]: https://github.com/eleaven/traedis/releases
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
[rfc9111-4.1]: https://www.rfc-editor.org/rfc/rfc9111#section-4.1
[rfc9111-4.3]: https://www.rfc-editor.org/rfc/rfc9111#section-4.3
[rfc9111-4.4]: https://www.rfc-editor.org/rfc/rfc9111#section-4.4
[rfc5861]: https://www.rfc-editor.org/rfc/rfc5861
[rfc9211]: https://www.rfc-editor.org/rfc/rfc9211
[bench-run]: https://www.github.com/eleaven/traedis/actions/runs/38480761928/attempts/1#summary-115582483916
[whoami]: https://github.com/eleaven/traedis/blob/main/example/dynamic/whoami.yml
[placeholder]: https://github.com/eleaven/traedis/blob/main/example/dynamic/placeholder.yml
[imaging]: https://github.com/eleaven/traedis/blob/main/example/dynamic/imaging.yml
[compose]: https://github.com/eleaven/traedis/blob/main/compose.yml
[example]: https://www.github.com/eleaven/traedis/tree/main/example
[contributing]: https://github.com/eleaven/traedis/blob/main/CONTRIBUTING.md
[license]: https://github.com/eleaven/traedis/blob/main/LICENSE
