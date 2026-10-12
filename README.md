> [!WARNING]
> **This project is a work in progress.** Use it at your own risk.

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
- **Fail open**: if Redis is slow or down, requests go straight to the backend, and Traefik still starts.
- **Observable**: every response tells what the cache did in a [`Cache-Status`](#cache-status) header.
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

| Key                           | Default                | Description                                                                                                                                    |
|-------------------------------|------------------------|------------------------------------------------------------------------------------------------------------------------------------------------|
| `redis.dsn`                   | `redis://cache:6379/0` | `redis://[user[:password]@]host[:port][/db]`. TLS (`rediss://`) is not supported yet.                                                          |
| `redis.timeout`               | `50ms`                 | Maximum time for a Redis operation. Past it, the cache is bypassed.                                                                            |
| `statusCodes`                 | `[200]`                | Status codes allowed to be stored. It only narrows what [RFC 9111][rfc9111] allows; it never makes a response cacheable.                                  |
| `defaultTtl`                  | `5m`                   | Freshness when the backend sends no `max-age`, `s-maxage` or `Expires`. `0` disables it.                                                       |
| `staleTtl`                    | `1h`                   | How long an entry is kept in Redis after it expires: the upper bound of every [stale response](#stale-responses). `0` disables them.           |
| `defaultStaleWhileRevalidate` | `0s`                   | `stale-while-revalidate` window for responses that don't send the directive. `0` disables it.                                                  |
| `defaultStaleIfError`         | `0s`                   | `stale-if-error` window for responses that don't send the directive. `0` disables it.                                                          |
| `maxBodyBytes`                | `5242880` (5 MiB)      | Larger responses are streamed to the client and not cached.                                                                                    |
| `maxVariants`                 | `16`                   | Stored [variants](#variants-vary) per URL. `0`: responses with `Vary` are not cached.                                                          |
| `sortQuery`                   | `false`                | Sort the query parameters by name in the cache key: `?a=1&b=2` and `?b=2&a=1` share the same entry. Only for backends that ignore their order. |
| `exposeKey`                   | `false`                | Add the Redis key to the `Cache-Status` header (`key="…"`).                                                                                    |

## How it works

### What gets cached

Responses are stored by URL (scheme, host, path and query), and only when all of the following hold:

- the request is a `GET` (a `HEAD` is answered from the stored `GET`);
- its status is listed in `statusCodes`;
- it is fresh, through `s-maxage`, `max-age`, `Expires` or `defaultTtl`;
- neither side says `no-store`, and the response is not `private` or `no-cache`;
- a request with `Authorization` got a response with `public`, `s-maxage` or `must-revalidate`;
- a response with `Set-Cookie` says `private="Set-Cookie"` or `no-cache="Set-Cookie"`: it is stored without the cookie, which only the
  client that caused the miss gets. Without one of them, it is not stored, even `public`;
- its `Vary`, if any, is not `*` and names neither `Cookie` nor `Authorization` (one variant per user);
- it is complete (a response that the backend cuts short is not stored) and fits in `maxBodyBytes`.

`OPTIONS`, `TRACE`, WebSocket upgrades and `Accept: text/event-stream` requests are forwarded as is. So are unsafe methods (`POST`, `PUT`,
`DELETE`…), which [invalidate](#invalidation) what is stored.

### `Cache-Status`

Every response tells what the cache did, in a `Cache-Status: traedis; …` header ([RFC 9211][rfc9211]):

| `Cache-Status: traedis; …`                                 | Meaning                                                                                                                           |
|------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------|
| `hit; ttl=N`                                               | Served from the cache, fresh for `N` more seconds.                                                                                |
| `hit; ttl=-N; detail=stale-while-revalidate`               | Served from the cache, stale by `N` seconds, and refreshed in the background.                                                     |
| `hit; ttl=-N; detail=max-stale`                            | Served from the cache, stale by `N` seconds, as the request allows.                                                               |
| `fwd=uri-miss`                                             | Forwarded: nothing is stored for this URL.                                                                                        |
| `fwd=vary-miss`                                            | Forwarded: this URL has [variants](#variants-vary), but none for this request.                                                    |
| `fwd=stale`                                                | Forwarded: the stored response is stale.                                                                                          |
| `fwd=request`                                              | Forwarded: the request says `no-cache`, or its `max-age` or `min-fresh` refuses a stored response that is still fresh.            |
| `fwd=stale; fwd-status=304; ttl=N`                         | [Validated](#conditional-requests-etag-last-modified) by the backend: the stored response is served, fresh for `N` seconds again. |
| `fwd=stale; fwd-status=503; ttl=-N; detail=stale-if-error` | The backend failed: the stored response is served in place of its error.                                                          |
| `fwd=bypass`                                               | Forwarded untouched: WebSocket upgrade or server-sent events.                                                                     |
| `fwd=bypass; detail=redis`                                 | Forwarded: Redis failed or timed out.                                                                                             |
| `fwd=method`                                               | Forwarded: `OPTIONS`, `TRACE` or an unsafe method.                                                                                |
| `fwd=method; detail=invalidated`                           | Unsafe method: the stored responses of the URL were [removed](#invalidation) (`detail=redis`: Redis could not be reached).        |

`fwd-status=N` is the status the backend answered with, whatever the client gets.

### Variants (`Vary`)

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

### Conditional requests (`ETag`, `Last-Modified`)

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

- **`stale-while-revalidate=N`** on the response ([RFC 5861](https://www.rfc-editor.org/rfc/rfc5861)): for `N` seconds, it is served at once
  and refreshed in the background, with at most one refresh per URL and 10 at a time in each Traefik instance.
- **`stale-if-error=N`** on the response: for `N` seconds, it replaces a `500`, `502`, `503` or `504`, including those of Traefik when the
  backend is unreachable.
- **`max-stale`** on the request ([RFC 9111 §5.2.1.2](https://www.rfc-editor.org/rfc/rfc9111#section-5.2.1.2)): `max-stale=N` accepts a
  response that is stale by less than `N` seconds, `max-stale` alone by any time. It is served as it is, without asking the backend nor
  refreshing it.

As [RFC 9111][rfc9111] requires, `must-revalidate`, `proxy-revalidate`, `s-maxage` and `no-cache` forbid all three:
`max-age=60, stale-while-revalidate=300` is served stale, `s-maxage=60, stale-while-revalidate=300` is not.

The request has its say too. With `max-age` or `min-fresh` (a browser reload), it always goes to the backend, and only gets a stale response
through `stale-if-error`. With `no-cache`, it never gets a stored response. With `only-if-cached`, it is never forwarded: when nothing
stored can answer it, it gets a `504` without `Cache-Status` (with `max-stale`, it is what an offline client sends).

For backends that don't send these directives, `defaultStaleWhileRevalidate` and `defaultStaleIfError` set the windows instead:
`defaultStaleIfError: 10m` serves what is stored rather than a `5xx`. A directive of the response always wins (`stale-if-error=0` still
turns it off), and the rules above still apply. With `defaultStaleWhileRevalidate`, clients can get a response older than what the backend
asked for: on a URL that is rarely requested, by up to the whole window.

### Invalidation

When the backend answers a request with an unsafe method (any method that is not `GET`, `HEAD`, `OPTIONS` or `TRACE`) with a `2xx` or a
`3xx`, the stored responses of its URL are removed, with all their variants
([RFC 9111 §4.4][rfc9111-4.4]).

- They are removed before the client gets the answer: its next `GET` is not served what was stored before. The answer itself is never held
  back.
- An error (`4xx`, `5xx`) changes nothing: the stored responses are kept, also for `stale-if-error`.
- Only the URL of the request is invalidated, query included: `POST /cart/add` does not invalidate `/cart`, and the `Location` and
  `Content-Location` of the response are not followed.
- There is no guarantee: if Redis cannot be reached at that moment, the stored responses stay until they expire, and a `GET` that was
  already on its way to the backend can store its response after the invalidation.

## Security

A shared cache gives the response made for one client to all the others. The defaults are careful (see
[What gets cached](#what-gets-cached)), but the cache only knows what the backend and the configuration tell it:

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
- **Redis is trusted.** Whoever writes to it chooses what clients are served, whoever reads it gets the stored responses, and the connection
  is not encrypted, password included: keep it on a private network, for the cache only.

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
| [Text answered after 100 ms][whoami] | 99.0 req/s · 102 ms | 98.0 req/s · 102 ms | 3,603 req/s · 2.1 ms | **×36** |
| [Rendered PNG][placeholder] | 81.6 req/s · 115 ms | 85.6 req/s · 111 ms | 3,328 req/s · 2.2 ms | **×41** |
| [Image converted to AVIF][imaging] | 68.3 req/s · 133 ms | 67.5 req/s · 135 ms | 3,426 req/s · 2.2 ms | **×50** |

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
[bench-run]: https://github.com/eleaven/traedis/actions/runs/38282212745/attempts/1#summary-114921946392
[whoami]: example/dynamic/whoami.yml
[placeholder]: example/dynamic/placeholder.yml
[imaging]: example/dynamic/imaging.yml
