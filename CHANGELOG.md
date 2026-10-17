# Changelog

What each release changes for those who use the plugin, the latest first. The section of a version is the text of its
[GitHub release](https://github.com/eleaven/traedis/releases): the CI reads it from this file.

## v0.3.0

Security fixes, and a cache that stores more by default. Clients can no longer poison the cache with the content of a `GET`, replace a
stored response with what the backend answers to their own request, remove one through its background refresh, nor make Traefik hold as much
memory as they want. Redirections are now stored by default, `statusCodes` takes ranges, and neither the `Cache-Control` of requests nor the
deprecated `Pragma` header field is read any more: see the breaking changes before upgrading.

### ⚠️ Breaking changes

- **The `Cache-Control` of requests is ignored by default**: `no-cache`, `max-age` and `min-fresh` no longer make the cache forward a
  request, `max-stale` no longer gets a stale response, `only-if-cached` no longer gets a `504`, and `no-store` no longer keeps a response
  from being stored. RFC 9111 makes these directives advisory (§5.2.1); followed, they let any client replace a stored response whenever it
  wants (see the security fixes). The new `ignoreClientCacheControl` option set to `false` follows them as before.
- **`Pragma: no-cache` no longer bypasses the cache**: a request with it and without `Cache-Control` is now answered from the cache like any
  other. A client that must reach the backend sends `Cache-Control: no-cache`, which the cache follows with
  `ignoreClientCacheControl: false`. RFC 9111 deprecates `Pragma` (§5.4).
- **Redirections are stored by default**: `statusCodes` is now `["200-399"]` (it was `[200]`), so that the cache keeps what the backend
  allows but its errors. A `301` or a `308` is stored for `defaultTtl` even without `Cache-Control`; a `302`, `303` or `307` only with
  explicit freshness. A backend that sends the same `Cache-Control: public` on all its responses, its redirection to its login page
  included, has to send `private` with it, or the configuration has to set `statusCodes: ["200-299"]`, or `["200"]` as before.
- **A `maxBodyBytes` over 64 MiB needs a `maxBufferedBytes` as large**: the new option bounds the bodies held in memory, and must leave room
  for the largest one. Without it, the middleware is refused when Traefik creates it.

### Features

- **`exposeCacheStatus`**: `false` hides the `Cache-Status` header from clients, which then no longer see what the cache did: the status of
  the backend behind a stale response, Redis down, memory full… It is `true` by default: `Age`, which RFC 9111 requires on a response
  served from the cache, tells a hit all the same. `exposeKey` requires it.
- **Ranges in `statusCodes`**: `["200-399", "404"]` allows any `2xx` or `3xx` and a `404` to be stored, as the `errors` middleware of
  Traefik writes its ranges. A range keeps out the codes that are never stored (`206`, `304`), but one with none that can be (`100-199`) is
  refused. The codes written as numbers (`[200, 404]`) still work.

### Security

- **Any client could replace a stored response with what the backend answers to its own request.** `Cache-Control: no-cache`, `max-age=0`
  or `min-fresh` made the cache forward the request, and store its response for everyone: with a header field that the backend reads but
  that its `Vary` does not name, a client could poison the cache whenever it wanted, without waiting for the stored response to expire. The
  `Cache-Control` of requests is now ignored, unless the new `ignoreClientCacheControl` option is `false`. Clients can no longer read stale
  responses with `max-stale` either, nor tell whether a URL is stored without storing it with `only-if-cached`.
- **Behind `basicAuth` or `forwardAuth`, the page of a user could be served to the others.** The cache knows a request is authenticated by
  its `Authorization`: `defaultTtl` is not applied to its response, which is only stored when the backend allows sharing it. `basicAuth`
  with `removeHeader` replaces it with the header field of `headerField`, and `forwardAuth` adds those of `authResponseHeaders`: such a
  request looked anonymous, and its response without `Cache-Control` was stored for `defaultTtl`. The new `authorizationHeaders` option
  lists these header fields (`[X-WebAuth-User]`), or one with an API key: their requests are treated as those with `Authorization`.
- **A `GET` with content could poison the cache.** Its content is not part of the cache key, but some backends read parameters in it: what
  they answered was stored and served to the other clients of the URL. A `GET` or `HEAD` with content (`Content-Length` or
  `Transfer-Encoding`) is now forwarded untouched, with `Cache-Status: traedis; fwd=bypass`: it is not answered from the cache, and its
  response is not stored.
- **A client could remove a stored response** with a safe method. The background refresh of `stale-while-revalidate` is the request that got
  the stale response, with its header fields: an invalid `Authorization` or cookie, or any header field that changes what the backend
  answers, got a response that may not be stored (an error, `private`, `Set-Cookie`, a body too large…), which removed the stored response.
  It now keeps it, but for a `404` or a `410`: the resource is gone. Neither does a `304` that may not be stored remove it any more, nor a
  `HEAD` whose response does not have the validators of a fresh stored response.
- **A request for many ranges got an answer many times larger than itself.** Each range of a stored response is sent as a part of its own,
  with its header fields: with `bytes=0-0,2-2,…`, a request of a few kilobytes got an answer tens of times larger, from a response that the
  backend made once, and even from a backend that does not serve ranges. A request for more than 16 ranges now gets the whole response, as
  RFC 9110 allows (§14.2).
- **Clients could make Traefik use as much memory as they wanted.** A response is copied in full to be stored, and a stored one read in full
  from Redis to be sent, and each one stays in memory until its client got it all: many slow requests for large responses added up without
  bound. The new `maxBufferedBytes` option (`64 MiB`) bounds the bodies a middleware holds at once (those up to 32 KiB are not counted):
  past it, a response is sent without being stored, and a stored one is not sent from the cache
  (`Cache-Status: traedis; fwd=bypass; detail=memory`). It must be at least `maxBodyBytes`: a configuration that raises `maxBodyBytes` past
  64 MiB has to raise it too. The README now advises an `inFlightReq` middleware before the cache.
- **Behind a proxy that terminates TLS, `http` and `https` requests shared their stored responses**: the scheme of the cache key was the one
  of the connection to Traefik, `http` for both. Now that redirections are stored by default, the redirection of an `http` request to
  `https` would have answered the `https` requests, in a loop. The scheme of the key is now the one of `X-Forwarded-Proto`, which Traefik
  sets to the one of the connection unless a proxy it trusts sent it: list that proxy in the `forwardedHeaders.trustedIPs` of the entry
  point. Behind it, the `https` requests start with an empty cache.

### Documentation

- **[Security](https://github.com/eleaven/traedis/blob/v0.3.0/README.md#security): clients can tell what others requested.** A response
  served from the cache says so, in its `Cache-Status` and its `Age`: a client learns whether someone else requested a URL, and when.
- **[Security](https://github.com/eleaven/traedis/blob/v0.3.0/README.md#security): clients can remove stored responses.** As RFC 9111
  requires, a `2xx` or a `3xx` answer to an unsafe method, even an unknown one, removes the stored responses of its URL: a backend that
  answers any method like a `GET`, or redirects an anonymous `POST` to its login page, lets anyone empty the cache. The README tells how to
  refuse the other methods.
- **[Security](https://github.com/eleaven/traedis/blob/v0.3.0/README.md#security): the key is the URL the cache sees, not the router.**
  Routers that use the same cache middleware share its stored responses, even when they reach different backends for different clients; so
  do URLs that a middleware before the cache rewrites to the same one (`stripPrefix`…).

## v0.2.1 – 2026-10-13

No change to the plugin itself: this release is for its page in the [Traefik Plugin
Catalog](https://plugins.traefik.io/plugins/6acde5da1c6f05d528c59972/traedis).

### Documentation

- **Catalog name and summary**: the plugin is now listed as `Traedis – Redis-backed HTTP Cache`, with the summary
  `RFC 9111 shared cache: Vary, ETag, stale-while-revalidate, fail-open`. The catalog only searches names: it is found by `redis`, `http`
  and `cache`, which `Traedis` alone was not.
- **The README displays in the catalog**: its logo, its CI badge and its links to the files of the repository were broken there, and its two
  notes showed the alert syntax of GitHub as text.

## v0.2.0 – 2026-10-13

Redis over TLS, range requests, and a cache that copes better with a slow or missing Redis.

### ⚠️ Breaking changes

- **The Redis keys have changed**: `traedis:<middleware>:<hash>`, with the name Traefik gives the middleware (`cache@file`). What v0.1.0
  stored is no longer read and expires by itself: the cache starts empty. Two middlewares no longer share their stored responses, even in
  the same Redis database.
- **`redis.timeout` has another meaning and another default**: it now bounds a whole Redis operation, the transfer of a body included, and
  defaults to `1s` (it was `50ms`). Getting a connection is bounded by the new `redis.dialTimeout` (`50ms`), which must not exceed it: a
  configuration that sets `redis.timeout` under `50ms` has to lower `redis.dialTimeout` too.
- **Configurations that could not work are refused** when the middleware is created, instead of being silently ineffective: an empty
  `statusCodes`, or one with a `1xx`, `206` or `304`; a `defaultStaleWhileRevalidate` or `defaultStaleIfError` over `staleTtl`; a
  `maxBodyBytes` over 512 MiB.

### Features

- **TLS to Redis**: a `rediss://` DSN encrypts the connection and verifies the certificate of Redis, with the authorities of the system or
  those of `redis.tls.ca` (a PEM file, or its content). `redis.tls.insecureSkipVerify` accepts any certificate.
- **[Range requests](https://github.com/eleaven/traedis/blob/v0.2.0/README.md#range-requests)**: a `GET` with `Range` is answered from the
  stored response with a `206 Partial Content` (several parts for several ranges), or a `416`. `If-Range` is honoured.
- **[Redis is no longer asked when it does not answer](https://github.com/eleaven/traedis/blob/v0.2.0/README.md#redis-failures)**: after
  `redis.maxFailures` failures in a row (`5`), requests are forwarded at once instead of each one waiting for a timeout. Redis is tried
  again every `redis.retryAfter` (`1s`), and the cache is back as soon as it answers.
- **New options**: `redis.poolSize` (`10`), `maxRevalidations` (`10`) and `revalidationTimeout` (`30s`) replace what were fixed values.
- **Logs name their middleware**: every line starts with `traedis: <middleware>:`, and one line tells when Redis is given up on, another
  when it is back.

### Fixes

- **Storing a response no longer delays it.** The response to a miss was written to Redis before the request ended: a response without
  `Content-Length` was only complete for its client after that, and the next request on the same connection waited for it. Responses are now
  stored once they are sent. The next request for the same URL waits for that, for no longer than `redis.dialTimeout`, so that a miss is
  still followed by a hit.
- **`Age` could be negative**, and `ttl` in `Cache-Status` longer than the freshness lifetime, for a response stored by a Traefik instance
  whose clock is ahead.
- **A connection closed while it was idle** (Redis restarted, a firewall dropped it) cost a bypass for each connection of the pool. The
  command is now run again on a new connection.

### Tests and CI

- Fuzzers for what parses untrusted data: the replies of Redis, stored entries and markers, `Cache-Control` and `Accept-Encoding`
  (`make fuzz`, and a short run in the CI).
- End-to-end tests against a Redis that only talks TLS.
- Releases are created with the GitHub CLI of the runner, without a third-party action in the job that may write to the repository.

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
