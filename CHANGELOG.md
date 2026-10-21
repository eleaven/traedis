# Changelog

What each release changes for those who use the plugin, the latest first. The section of a version is the text of its
[GitHub release](https://github.com/eleaven/traedis/releases): the CI reads it from this file.

## v0.4.0

Safer defaults and less memory in Redis: see the breaking changes before upgrading.

### ⚠️ Breaking changes

- **`defaultTtl` is now `0`**: a response without `max-age`, `s-maxage` nor `Expires` is no longer stored. Set `defaultTtl: 5m` to keep
  the previous behaviour, only for backends whose responses are the same for every client.
- **`redis.dsn` is required**: it no longer defaults to `redis://cache:6379/0`.
- **`ignoreClientCacheControl` is renamed `followClientCacheControl`**, with the opposite value: replace `ignoreClientCacheControl: false`
  with `followClientCacheControl: true`. The old key is silently ignored.

### Features

- **[Expired responses are only kept while they may be used](https://github.com/eleaven/traedis/blob/v0.4.0/SPEC.md#stale-responses)**:
  for `staleTtl` with an `ETag` or a `Last-Modified`, otherwise for their stale windows, if any.

### Documentation

- **A shorter README**, with a security checklist and tables for the HTTP headers. The caching rules and the details of security move to
  [`SPEC.md`](https://github.com/eleaven/traedis/blob/v0.4.0/SPEC.md).

## v0.3.1 – 2026-10-19

Security fixes for redirections and the case of the host.

### Security

- **A client could have every client redirected in a loop**, by having the backend redirect to the URL it asked for (Rails' `force_ssl` with
  `X-Forwarded-Scheme: http`), which was stored for everyone. A redirection to its own URL is now never stored.
- **A client could have the page of another case of the host served to everyone** (`Host: EXAMPLE.com` answered with the default site or a
  redirection, stored for `example.com`). The cache key now keeps the case of the host.

## v0.3.0 – 2026-10-17

Security fixes, and redirections stored by default: see the breaking changes before upgrading.

### ⚠️ Breaking changes

- **The `Cache-Control` of requests is ignored by default**: its directives are advisory, and let any client replace a stored response.
  `ignoreClientCacheControl: false` follows them as before.
- **`Pragma: no-cache` no longer bypasses the cache**: clients send `Cache-Control: no-cache` instead, with `ignoreClientCacheControl:
  false`.
- **Redirections are stored by default**: `statusCodes` is now `["200-399"]` (it was `[200]`). A backend that sends `Cache-Control: public`
  with its redirection to a login page has to send `private`, or the configuration has to set `statusCodes: ["200-299"]`.
- **A `maxBodyBytes` over 64 MiB needs a `maxBufferedBytes` as large.**

### Features

- **`exposeCacheStatus`**: `false` hides the `Cache-Status` header from clients.
- **Ranges in `statusCodes`**: `["200-399", "404"]`, as Traefik writes them.

### Security

- **Any client could replace a stored response** with what the backend answered to its own request (`Cache-Control: no-cache`).
- **Behind `basicAuth` or `forwardAuth`, the page of a user could be served to the others**: list the header fields that replace
  `Authorization` in the new `authorizationHeaders` option (`[X-WebAuth-User]`).
- **A client could have its own page served to the others**, with its session cookie on a second `Cookie` line, which the cache did not
  read.
- **A `304` could change the `Content-Type` of a stored response** for everyone: the stored body now keeps its own.
- **A `GET` with content could poison the cache**: it now bypasses it.
- **A client could remove a stored response** through its background refresh: only a `404` or a `410` removes it now.
- **A request for many ranges got an answer many times larger**: past 16 ranges, the whole response is sent.
- **Clients could make Traefik use as much memory as they wanted**: the new `maxBufferedBytes` option (`64 MiB`) bounds the bodies held in
  memory at once.
- **Behind a proxy that terminates TLS, `http` and `https` requests shared their stored responses**: the scheme of the key is now the one of
  `X-Forwarded-Proto`. List that proxy in the `forwardedHeaders.trustedIPs` of the entry point.

## v0.2.1 – 2026-10-13

No change to the plugin: its name, summary and README now display well in the [Traefik Plugin
Catalog](https://plugins.traefik.io/plugins/6acde5da1c6f05d528c59972/traedis).

## v0.2.0 – 2026-10-13

Redis over TLS, range requests, and a cache that copes better with a slow or missing Redis: see the breaking changes before upgrading.

### ⚠️ Breaking changes

- **The Redis keys have changed** (`traedis:<middleware>:<hash>`): the cache starts empty, and two middlewares no longer share their stored
  responses.
- **`redis.timeout` now bounds a whole Redis operation**, and defaults to `1s`. Getting a connection is bounded by the new
  `redis.dialTimeout` (`50ms`), which must not exceed it.
- **Configurations that could not work are refused** when the middleware is created, instead of being silently ineffective.

### Features

- **TLS to Redis**: `rediss://`, with `redis.tls.ca` and `redis.tls.insecureSkipVerify`.
- **[Range requests](https://github.com/eleaven/traedis/blob/v0.2.0/README.md#range-requests)**: `206 Partial Content`, `416` and
  `If-Range`.
- **[Redis is no longer asked when it does not answer](https://github.com/eleaven/traedis/blob/v0.2.0/README.md#redis-failures)**: after
  `redis.maxFailures` failures in a row, and tried again every `redis.retryAfter`.
- **New options**: `redis.poolSize`, `maxRevalidations` and `revalidationTimeout`.
- **Logs name their middleware.**

### Fixes

- **Storing a response no longer delays it.**
- **`Age` could be negative** behind a Traefik instance whose clock is ahead.
- **A connection closed while it was idle** no longer costs a bypass.

## v0.1.0 – 2026-10-12

First release of Traedis:

- **HTTP caching per [RFC 9111](https://www.rfc-editor.org/rfc/rfc9111)**: `Cache-Control` (request and response directives), `Expires`,
  `Age`, and safe defaults for `Authorization`, `Set-Cookie` and `private`.
- **Variants**: one stored response per `Vary` value, with normalized `Accept-Encoding` handling (`zstd`, `br`, `gzip`, `deflate`).
- **Conditional requests**: `ETag` / `Last-Modified` validation against the backend, and `304 Not Modified` answered to clients.
- **Stale responses** ([RFC 5861](https://www.rfc-editor.org/rfc/rfc5861)): `stale-while-revalidate` with background refresh,
  `stale-if-error`, and `max-stale`.
- **Invalidation**: unsafe methods (`POST`, `PUT`, `DELETE`…) remove the stored responses of their URL.
- **Observability**: a `Cache-Status` header ([RFC 9211](https://www.rfc-editor.org/rfc/rfc9211)) on every response.
- **Fail open**: Redis errors and timeouts bypass the cache instead of failing the request.
- **Pass-through**: WebSockets, server-sent events and bodies over `maxBodyBytes` are streamed untouched.
- **Dependency-free**: Go standard library only.
