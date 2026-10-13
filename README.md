> [!WARNING]
> **This project is in its early stages.** Use it at your own risk.

<!--suppress HtmlDeprecatedAttribute -->
<p align="center">
  <img src="assets/icon.svg" alt="Traedis Logo" height="360">
</p>

# Traedis

[![CI](https://github.com/eleaven/traedis/actions/workflows/ci.yml/badge.svg)][ci]

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
available in [`compose.yml`](compose.yml) and [`example/`](example).

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
| `maxVariants`                  | `16`                   | Stored [variants](#variants) per URL. `0`: responses with `Vary` are not cached.                                                               |
| `maxRevalidations`             | `10`                   | [Background refreshes](#stale-responses) running at once, for each middleware in each Traefik instance.                                        |
| `revalidationTimeout`          | `30s`                  | Maximum time of a background refresh.                                                                                                          |
| `sortQuery`                    | `false`                | Sort the query parameters by name in the cache key: `?a=1&b=2` and `?b=2&a=1` share the same entry. Only for backends that ignore their order. |
| `exposeKey`                    | `false`                | Add the [Redis key](#cacheable-responses) to the `Cache-Status` header (`key="…"`).                                                            |

## HTTP headers

Traedis is driven by the standard HTTP header fields. This is what each of them does; [Caching rules](#caching-rules) has the
fine print.

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
- `Pragma: no-cache`: like `Cache-Control: no-cache`, for a request without `Cache-Control`.
- `If-None-Match`, `If-Modified-Since`: answered with a `304` when the stored response is the one the client
  [already has](#conditional-requests).
- `Authorization`: the response is only stored with `public`, `s-maxage` or `must-revalidate`.
- `Cookie`: not part of the cache key, see [Security](#security).
- `Accept-Encoding`, and the header fields that `Vary` names: choose the [variant](#variants).
- `Upgrade`, `Accept: text/event-stream`: forwarded untouched (WebSockets, server-sent events).

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
  - `fwd=bypass`: forwarded untouched, WebSocket upgrade or server-sent events.
  - `fwd=bypass; detail=redis`: forwarded, Redis [failed, timed out or is no longer asked](#redis-failures).
  - `fwd=method`: forwarded, `OPTIONS`, `TRACE` or an unsafe method.
  - `fwd=method; detail=invalidated`: unsafe method, the stored responses of the URL were [removed](#invalidation-by-unsafe-methods)
    (`detail=redis`: Redis could not be reached).
  - `fwd-status=N`: the status the backend answered with, whatever the client gets.
  - `key="…"`: the Redis key, with `exposeKey`.

## Caching rules

### Cacheable responses

Responses are stored by middleware and by URL (scheme, host, path and query), and only when all of the following hold:

- the request is a `GET` (a `HEAD` is answered from the stored `GET`);
- its status is listed in `statusCodes`;
- it is fresh, through `s-maxage`, `max-age`, `Expires` or `defaultTtl`;
- no [header field](#http-headers) of the request or of the response forbids it;
- it is complete (a response that the backend cuts short is not stored) and fits in `maxBodyBytes`.

`OPTIONS`, `TRACE`, WebSocket upgrades and `Accept: text/event-stream` requests are forwarded as is. So are unsafe methods (`POST`, `PUT`,
`DELETE`…), which [invalidate](#invalidation-by-unsafe-methods) what is stored.

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

> [!NOTE]
> Traefik's `compress` middleware only adds `Vary: Accept-Encoding` to the responses it compresses: placed after the cache, its uncompressed
> response has no `Vary` and is served to everyone. Put `compress` before the cache, or set `Vary: Accept-Encoding` on every response with a
> `headers` middleware, as [`example/dynamic/whoami.yml`](example/dynamic/whoami.yml) does.

### Conditional requests

The cache plays both parts of `If-None-Match`, `If-Modified-Since` and `304 Not Modified` ([RFC 9111 §4.3][rfc9111-4.3]).

**Towards its clients**, it answers `304` from a stored response whose `ETag` is one of `If-None-Match` (weak comparison), or that was not
modified since `If-Modified-Since` (its `Last-Modified`, otherwise its `Date`). With nothing stored, the request reaches the backend as it
is and its `304` goes to the client, without being stored: a URL is cached once a client asks for it without having it already. `If-Match`,
`If-Unmodified-Since` and `If-Range` are always for the backend.

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

### Stale responses

A stored response that is no longer fresh is served only when allowed, for at most `staleTtl`:

- **`stale-while-revalidate`** ([RFC 5861][rfc5861]): it is served at once and refreshed in the background, with at most one refresh per
  URL and `maxRevalidations` at a time: past it, the refresh is left to a later request. Not for a request with `max-age` or `min-fresh`
  (a browser reload), which waits for the backend.
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

### Redis failures

The cache never fails a request because of Redis: what Redis cannot answer in time is forwarded to the backend, with
`Cache-Status: traedis; fwd=bypass; detail=redis`, and its response is not stored.

- Getting a connection takes at most `redis.dialTimeout`, and the whole operation `redis.timeout`: Redis has more time to send a large
  body than to show that it is there.
- A connection that was closed while it was idle (Redis restarted, a firewall dropped it) is replaced without a bypass.
- After `redis.maxFailures` failures in a row, Redis is no longer asked: requests are forwarded at once, instead of each one waiting for a
  timeout. One of them tries Redis again every `redis.retryAfter`, and the cache is back as soon as it answers.
- Traefik logs the failures, at most once a minute, and when Redis is given up on and back.

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
        - auth      # before the cache: a hit is served without running what comes after it
        - ratelimit # before the cache: random URLs are misses that reach the backend
        - cache
```

## Performance

A hit is served from Redis in about 2 ms, whatever the backend costs: the slower the backend, the larger the gain. A miss takes no longer
than the backend alone.

Load test through Traefik with 10 concurrent clients, as requests per second and median latency:

| Backend | Without cache | Cache miss | Cache hit | Gain |
| --- | --: | --: | --: | --: |
| [Text answered after 100 ms][whoami] | 98.1 req/s · 102 ms | 98.0 req/s · 102 ms | 4,182 req/s · 1.73 ms | **×43** |
| [Rendered PNG][placeholder] | 77.3 req/s · 124 ms | 81.8 req/s · 115 ms | 3,900 req/s · 1.84 ms | **×50** |
| [Image converted to AVIF][imaging] | 64.1 req/s · 143 ms | 57.9 req/s · 161 ms | 4,009 req/s · 1.79 ms | **×62** |

These figures come from the [CI run of October 12, 2026][bench-run], on a shared GitHub Actions runner: read them as orders of magnitude.
Every [CI][ci] run publishes its own in its summary, with the p95 and the cost of the middleware alone. `make bench` runs them locally.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the local stack, the tests and the rules of the project.

## License

[MIT](LICENSE)

[ci]: https://github.com/eleaven/traedis/actions/workflows/ci.yml
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
[rfc9111-4.1]: https://www.rfc-editor.org/rfc/rfc9111#section-4.1
[rfc9111-4.3]: https://www.rfc-editor.org/rfc/rfc9111#section-4.3
[rfc9111-4.4]: https://www.rfc-editor.org/rfc/rfc9111#section-4.4
[rfc5861]: https://www.rfc-editor.org/rfc/rfc5861
[rfc9211]: https://www.rfc-editor.org/rfc/rfc9211
[bench-run]: https://github.com/eleaven/traedis/actions/runs/38289730377/attempts/1#summary-114946639664
[whoami]: example/dynamic/whoami.yml
[placeholder]: example/dynamic/placeholder.yml
[imaging]: example/dynamic/imaging.yml
