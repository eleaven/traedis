> ⚠️ **This project is in its early stages.** Use it at your own risk.

<!-- The Traefik Plugin Catalog shows this file on another site, where it rewrites every https://github.com/ link to
     raw.githubusercontent.com. So the links are absolute: to a file through /blob/, which that turns into the file itself, and to
     anything else (a directory, a workflow) through www.github.com, which it leaves alone. It does not know the alerts of GitHub
     either (`> [!NOTE]`), which it shows as they are written -->
<!--suppress HtmlDeprecatedAttribute -->
<p align="center">
  <img src="https://raw.githubusercontent.com/eleaven/traedis/main/assets/icon.svg" alt="Traedis Logo" height="290">
</p>

# Traedis

[![Release](https://img.shields.io/github/v/release/eleaven/traedis)][releases]
[![CI](https://img.shields.io/github/actions/workflow/status/eleaven/traedis/ci.yml?branch=main&label=CI)][ci]
[![Traefik Plugin Catalog](https://img.shields.io/badge/Traefik_Plugin_Catalog-v3-24A1C1?logo=traefikproxy&logoColor=white)][catalog]
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)][gomod]

**Redis**-backed HTTP cache for **Traefik**, shipped as a **middleware plugin**.

- **Standards-based**: follows [RFC 9111][rfc9111] (`Cache-Control`, `Vary`, `ETag`, `304 Not Modified`…), [RFC 5861][rfc5861]
  (`stale-while-revalidate`, `stale-if-error`) and [RFC 9211][rfc9211] (`Cache-Status`).
- **Dependency-free**: only uses the Go standard library, with no third-party module and no `unsafe`.
- **Safe by default**: never caches what must not be shared (`Authorization`, `Set-Cookie`, `private`) unless the backend explicitly allows
  it. See [Security](#security) for the pitfalls.
- **Fail open**: if Redis is slow or down, requests are forwarded to the backend, and Traefik still starts.
- **Observable**: every response tells what the cache did in a [`Cache-Status`](#added-by-traedis) header.
- **Stream-friendly**: large bodies, WebSockets and server-sent events pass through untouched.
- **Persistent and shared**: responses are cached in Redis, not in Traefik's memory. The cache survives restarts and redeploys, and all
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

Put authentication and rate limiting before the cache (see [Security](#security)). [`compose.yml`][compose] and [`example/`][example]
provide a complete local setup, with Traefik, Redis and sample backends.

## Configuration

All keys but `redis.dsn` are optional. Durations use the [Go syntax](https://pkg.go.dev/time#ParseDuration) (`50ms`, `5m`, `1h`).

| Key                            | Default             | Description                                                                                                                                               |
|--------------------------------|---------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `redis.dsn`                    | required            | `redis://[user[:password]@]host[:port][/db]`, or `rediss://` for TLS, which verifies Redis's certificate.                                                 |
| `redis.tls.ca`                 | system authorities  | With `rediss://`, the authorities trusted to sign Redis's certificate, as a PEM file or its content.                                                      |
| `redis.tls.insecureSkipVerify` | `false`             | With `rediss://`, accept any certificate. The connection is encrypted, but Redis may be anyone.                                                           |
| `redis.poolSize`               | `10`                | Maximum connections to Redis, per middleware and per Traefik instance. A request that finds them all busy waits for one.                                  |
| `redis.dialTimeout`            | `50ms`              | Maximum time to get a Redis connection, free in the pool or new. Past it, the request is forwarded without the cache.                                     |
| `redis.timeout`                | `1s`                | Maximum time for a whole Redis operation, including getting the connection and transferring a body. Past it, the request is forwarded without the cache.  |
| `redis.maxFailures`            | `5`                 | Consecutive failures after which Redis is [no longer asked][spec-redis]. `0`: every request tries it.                                                     |
| `redis.retryAfter`             | `1s`                | How often Redis is tried again once it is no longer asked.                                                                                                |
| `statusCodes`                  | `["200-399"]`       | Codes (`404`) and ranges (`400-499`) that may be cached. It only narrows what [RFC 9111][rfc9111] allows; it never makes a response cacheable.            |
| `defaultTtl`                   | `0`                 | Freshness when the backend sends no `max-age`, `s-maxage` or `Expires` (`5m`). `0`: not cached. Read [Security](#security) first.                         |
| `staleTtl`                     | `1h`                | How long, at most, an expired response stays in Redis to be [validated][spec-conditional] or [served stale][spec-stale].                                  |
| `defaultStaleWhileRevalidate`  | `0`                 | `stale-while-revalidate` window for responses without the directive, at most `staleTtl`. `0` disables it.                                                 |
| `defaultStaleIfError`          | `0`                 | `stale-if-error` window for responses without the directive, at most `staleTtl`. `0` disables it.                                                         |
| `maxBodyBytes`                 | `5242880` (5 MiB)   | Larger responses are streamed to the client and not cached. At most 512 MiB, the largest value Redis accepts.                                             |
| `maxBufferedBytes`             | `67108864` (64 MiB) | Total size of the [bodies held in memory][spec-abuse] per Traefik instance. Past it, requests are forwarded without the cache. At least `maxBodyBytes`.   |
| `maxVariants`                  | `16`                | Maximum cached [variants][spec-variants] per URL. `0`: responses with `Vary` are not cached.                                                              |
| `maxRevalidations`             | `10`                | Maximum [background refreshes][spec-stale] running at once, per middleware and per Traefik instance.                                                      |
| `revalidationTimeout`          | `30s`               | Maximum duration of a background refresh.                                                                                                                 |
| `collapseTimeout`              | `0`                 | How long [requests that wait][spec-collapse] for the response to another request for their URL wait, at most. `0`: they don't.                            |
| `sortQuery`                    | `false`             | Sort the query parameters by name in the cache key, so that `?a=1&b=2` and `?b=2&a=1` share a cached response. Only for backends that ignore their order. |
| `authorizationHeaders`         | `[]`                | Request headers that identify the client, as `Authorization` does (`X-WebAuth-User`). Requests with one are treated like those with `Authorization`.      |
| `followClientCacheControl`     | `false`             | Follow the request's `Cache-Control`, whose directives are advisory. See [Sent by the client](#sent-by-the-client).                                       |
| `exposeCacheStatus`            | `true`              | Add the [`Cache-Status`](#added-by-traedis) header to responses, to tell what the cache did.                                                              |
| `exposeKey`                    | `false`             | Add the [Redis key][spec-cacheable] to `Cache-Status` (`key="…"`). Requires `exposeCacheStatus`.                                                          |

## HTTP headers

Traedis follows the standard HTTP headers sent by the backend and the client, and adds a few of its own.

### Sent by the backend

The backend's response says whether it may be cached, for how long, and whether it may be served stale.

| `Cache-Control` directive                       | Effect                                                                                |
|-------------------------------------------------|---------------------------------------------------------------------------------------|
| `max-age=N`                                     | Fresh for `N` seconds.                                                                |
| `s-maxage=N`                                    | Same, and wins over `max-age`. Never served [stale][spec-stale].                      |
| `no-store`, `private`, `no-cache`               | Not cached.                                                                           |
| `private="Set-Cookie"`, `no-cache="Set-Cookie"` | Cached without the headers they name. Only the client that caused the miss gets them. |
| `public`                                        | Cached even when the request has `Authorization`.                                     |
| `must-revalidate`, `proxy-revalidate`           | Never served stale.                                                                   |
| `stale-while-revalidate=N`                      | Once stale, still served for `N` seconds, and refreshed in the background.            |
| `stale-if-error=N`                              | Once stale, still served for `N` seconds instead of a backend error.                  |

| Header                  | Effect                                                                                                                                                  |
|-------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Expires`               | Fresh until this date, when there is no `max-age` or `s-maxage`. Without any of the three, the response is not cached, unless `defaultTtl` is set.      |
| `Age`, `Date`           | How old the response already is. It is cached for the freshness it has left.                                                                            |
| `Vary`                  | One cached [variant][spec-variants] per value of the request headers it names. Not cached with `*`, `Cookie` or `Authorization` (one variant per user). |
| `ETag`, `Last-Modified` | Let the cache [validate][spec-conditional] a stale response instead of fetching it again, and answer `304` to clients that already have it.             |
| `Set-Cookie`            | Not cached, even with `public`, unless `private="Set-Cookie"` or `no-cache="Set-Cookie"` keeps the cookie out.                                          |

### Sent by the client

The client's request says whether a cached response may answer it.

Its `Cache-Control` is ignored by default. Its directives are advisory
([RFC 9111 §5.2.1](https://www.rfc-editor.org/rfc/rfc9111#section-5.2.1)), and following them lets any client replace the cached response
with what the backend answers to its own request, get a stale one, and tell whether a URL is cached without caching it. With
`followClientCacheControl: true`:

| `Cache-Control` directive | Effect                                                                                                                                                 |
|---------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| `no-cache`                | Always forwarded, whatever is cached. The response is cached.                                                                                          |
| `no-store`                | The response is not cached. A cached one can still answer.                                                                                             |
| `max-age=N`               | Refuses a cached response older than `N` seconds.                                                                                                      |
| `min-fresh=N`             | Refuses a cached response that will not be fresh for `N` more seconds.                                                                                 |
| `max-stale=N`             | Accepts a response that is stale by less than `N` seconds. `max-stale` alone accepts any staleness.                                                    |
| `only-if-cached`          | Never forwarded. When nothing cached can answer, the client gets a `504` without `Cache-Status`. With `max-stale`, it is what an offline client sends. |

| Header                                                     | Effect                                                                                                                                                              |
|------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `If-None-Match`, `If-Modified-Since`                       | Answered with a `304` when the client [already has][spec-conditional] the cached response.                                                                          |
| `Range`, `If-Range`                                        | Answered with the requested bytes of the cached response ([range requests][spec-range]).                                                                            |
| `Authorization`, and the headers of `authorizationHeaders` | The response is not cached, unless the backend allows sharing it with `public`, `s-maxage` or `must-revalidate`.                                                    |
| `Cookie`                                                   | Not part of the cache key: a cookie does not make a response private.                                                                                               |
| `Accept-Encoding`, and the headers that `Vary` names       | Select the [variant][spec-variants].                                                                                                                                |
| `Upgrade`, `Accept: text/event-stream`                     | Forwarded untouched (WebSockets, server-sent events).                                                                                                               |
| `Content-Length`, `Transfer-Encoding`                      | A `GET` or `HEAD` with a body is forwarded untouched, and its response is not cached. Some backends read parameters in the body, which the cache key does not have. |

### Added by Traedis

| Header                               | Effect                                                                                              |
|--------------------------------------|-----------------------------------------------------------------------------------------------------|
| `Age`                                | On a response served from the cache, the seconds since the backend made it.                         |
| `If-None-Match`, `If-Modified-Since` | Sent to the backend instead of the client's, to [validate][spec-conditional] a cached response.     |
| `Cache-Status`                       | On every response, what the cache did ([RFC 9211][rfc9211]), unless `exposeCacheStatus` is `false`. |

`Cache-Status` reads `traedis;` followed by one of these values:

| Value                                                      | Meaning                                                                                                                                                  |
|------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `hit; ttl=N`                                               | Served from the cache, fresh for `N` more seconds.                                                                                                       |
| `hit; ttl=-N; detail=stale-while-revalidate`               | Served from the cache, stale by `N` seconds, and refreshed in the background.                                                                            |
| `hit; ttl=-N; detail=max-stale`                            | Served from the cache, stale by `N` seconds, as the request allows (`followClientCacheControl: true`).                                                   |
| `fwd=uri-miss`                                             | Forwarded: nothing is cached for this URL.                                                                                                               |
| `fwd=vary-miss`                                            | Forwarded: this URL has [variants][spec-variants], but none for this request.                                                                            |
| `fwd=stale`                                                | Forwarded: the cached response is stale.                                                                                                                 |
| `fwd=request`                                              | Forwarded: the request has `no-cache`, or its `max-age` or `min-fresh` refuses a cached response that is still fresh (`followClientCacheControl: true`). |
| `fwd=stale; fwd-status=304; ttl=N`                         | [Validated][spec-conditional] by the backend: the cached response is served, fresh again for `N` seconds.                                                |
| `fwd=uri-miss; ttl=N; collapsed`                           | Forwarded with another request, which it [waited for][spec-collapse]: served its cached response, fresh for `N` more seconds.                            |
| `fwd=stale; fwd-status=503; ttl=-N; detail=stale-if-error` | The backend failed: the cached response is served instead of its error.                                                                                  |
| `fwd=bypass`                                               | Forwarded untouched: WebSocket upgrade, server-sent events or request with a body.                                                                       |
| `fwd=bypass; detail=redis`                                 | Forwarded: Redis [failed, timed out or is no longer asked][spec-redis].                                                                                  |
| `fwd=bypass; detail=memory`                                | Forwarded: the [bodies held in memory][spec-abuse] reach `maxBufferedBytes`, so the cached response is not sent.                                         |
| `fwd=method`                                               | Forwarded: `OPTIONS`, `TRACE` or an unsafe method.                                                                                                       |
| `fwd=method; detail=invalidated`                           | Unsafe method: the URL's cached responses were [removed][spec-invalidation] (`detail=redis` when Redis could not be reached).                            |

Two parameters can come with them:

| Parameter      | Meaning                                                                           |
|----------------|-----------------------------------------------------------------------------------|
| `fwd-status=N` | The status the backend answered with, whatever the client gets.                   |
| `key="…"`      | The [Redis key][spec-cacheable], with `exposeKey: true`, to help debug the cache. |

## Caching rules

The [specification][spec] details each rule. In short:

- **[Cacheable responses][spec-cacheable]**: a `GET` is cached per middleware and per URL when its status is in `statusCodes`, it is fresh
  and complete, it fits in `maxBodyBytes`, and no header forbids it.
- **[Variants][spec-variants]**: one cached response per value of the request headers that `Vary` names, up to `maxVariants` per URL.
  Responses are cached by `Content-Encoding`, not by `Accept-Encoding`.
- **[Conditional requests][spec-conditional]**: the cache answers `304` to clients that already have the response, and validates stale
  responses with the backend's `ETag` and `Last-Modified`.
- **[Range requests][spec-range]**: only whole responses are cached, and `Range` requests are answered from them.
- **[Stale responses][spec-stale]**: served for `stale-while-revalidate`, `stale-if-error` or the client's `max-stale`, for at most
  `staleTtl`.
- **[Collapsed requests][spec-collapse]**: with `collapseTimeout`, the requests for a URL that is being fetched wait for its response,
  which the backend then gets once.
- **[Invalidation by unsafe methods][spec-invalidation]**: a `2xx` or `3xx` answer to a `POST`, `PUT`, `DELETE`… removes the URL's cached
  responses.
- **[Redis failures][spec-redis]**: requests are forwarded when Redis is slow or down, and Redis is no longer asked after
  `redis.maxFailures` consecutive failures.

## Security

A shared cache serves the response made for one client to all the others. The defaults are careful, but the cache only knows what your
backend and your configuration tell it. Check these points. The [specification][spec-security] explains each of them.

- **Leave `defaultTtl` at `0`**, unless every response that your backend sends without `Cache-Control` is the same for all clients.
- **Have your backend send `private` or `no-store` with whatever depends on the client**, redirections included, or leave `3xx` out of
  `statusCodes`. Cookies are not part of the cache key, and `private="Set-Cookie"` only keeps the cookie itself out.
- **List in `authorizationHeaders` the headers that identify the client** when it is not `Authorization`: an API key, or the header that
  `basicAuth` or `forwardAuth` set (`X-WebAuth-User`).
- **Name in `Vary` every request header that the response depends on** (`User-Agent`, `Accept-Language`, `Forwarded`…). Otherwise, one
  client chooses what the others get.
- **Give each router its own cache middleware** when routers share URLs but not backends, or when they rewrite the URL. Rewrite it after
  the cache, not before.
- **Behind a proxy that terminates TLS, list it in the entry point's `forwardedHeaders.trustedIPs`**, so that `http` and `https`
  requests don't share cached responses.
- **Have your backend refuse the methods it does not handle with a `405`**, or filter them in the router rule. Otherwise, any client can
  empty the cache of any URL.
- **Give Redis a `maxmemory` with the `allkeys-lfu` policy**, and keep it for the cache only, on a private network or behind `rediss://`.
- **Set `exposeCacheStatus: false`** if clients must not see what the cache did. `Age` still tells them that a response was served from
  the cache, and so that someone else requested it.
- **Put authentication, `rateLimit` and `inFlightReq` before the cache**:

```yaml
http:
  routers:
    my-app:
      middlewares:
        - auth        # a hit is served without running what comes after the cache
        - ratelimit   # random URLs are misses that reach the backend
        - inflightreq # each request may hold a body in memory
        - cache
```

## Performance

A hit is served in about 1.5 ms, whatever the backend costs, so the slower the backend, the larger the gain. A miss takes no longer than
the backend alone, since its response is cached after the client receives it.

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
[catalog]: https://plugins.traefik.io/plugins/6acde5da1c6f05d528c59972/traedis
[gomod]: https://github.com/eleaven/traedis/blob/main/go.mod
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
[rfc5861]: https://www.rfc-editor.org/rfc/rfc5861
[rfc9211]: https://www.rfc-editor.org/rfc/rfc9211
[spec]: https://github.com/eleaven/traedis/blob/main/SPEC.md
[spec-cacheable]: https://github.com/eleaven/traedis/blob/main/SPEC.md#cacheable-responses
[spec-variants]: https://github.com/eleaven/traedis/blob/main/SPEC.md#variants
[spec-conditional]: https://github.com/eleaven/traedis/blob/main/SPEC.md#conditional-requests
[spec-range]: https://github.com/eleaven/traedis/blob/main/SPEC.md#range-requests
[spec-stale]: https://github.com/eleaven/traedis/blob/main/SPEC.md#stale-responses
[spec-collapse]: https://github.com/eleaven/traedis/blob/main/SPEC.md#collapsed-requests
[spec-invalidation]: https://github.com/eleaven/traedis/blob/main/SPEC.md#invalidation-by-unsafe-methods
[spec-redis]: https://github.com/eleaven/traedis/blob/main/SPEC.md#redis-failures
[spec-security]: https://github.com/eleaven/traedis/blob/main/SPEC.md#security
[spec-abuse]: https://github.com/eleaven/traedis/blob/main/SPEC.md#abuse-by-clients
[bench-run]: https://www.github.com/eleaven/traedis/actions/runs/38480761928/attempts/1#summary-115582483916
[whoami]: https://github.com/eleaven/traedis/blob/main/example/dynamic/whoami.yml
[placeholder]: https://github.com/eleaven/traedis/blob/main/example/dynamic/placeholder.yml
[imaging]: https://github.com/eleaven/traedis/blob/main/example/dynamic/imaging.yml
[compose]: https://github.com/eleaven/traedis/blob/main/compose.yml
[example]: https://www.github.com/eleaven/traedis/tree/main/example
[contributing]: https://github.com/eleaven/traedis/blob/main/CONTRIBUTING.md
[license]: https://github.com/eleaven/traedis/blob/main/LICENSE
