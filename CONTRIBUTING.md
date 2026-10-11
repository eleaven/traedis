# Contributing

Requires Docker (with Compose), Go ≥ 1.22 and the [Yaegi](https://github.com/traefik/yaegi) version embedded in Traefik v3.7:
`go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1`.

## Project structure

```text
pkg/          # the plugin (package `traedis`): one concern per file, with its `_test.go` next to it
example/      # Traefik configuration of the local stack: one dynamic file per sample backend
e2e/          # end-to-end tests: Hurl files requesting Traefik
benchmark/    # benchmarks under Yaegi, k6 load test, results viewer
compose.yml   # local stack: Traefik, Redis, sample backends (whoami, placeholder, imgproxy), Redis Insight
.traefik.yml  # plugin manifest of the Traefik Plugin Catalog
VERSION       # the version to release
```

In `pkg/`, `middleware.go` is the entry point (`New()` and the handler), `policy.go` decides what may be stored and served, and `redis.go`
is the Redis client (RESP and connection pool).

## Commands

```bash
make start       # start the local stack; Traefik restarts whenever the plugin code changes
make stop        # stop the stack (`make reset` also removes the volumes)
make test        # all the tests the CI requires: `make unit`, then `make e2e`
make unit        # gofmt, go vet, go test -race, then the same tests under Yaegi, with a Redis for the integration tests
make e2e         # end-to-end tests, through Traefik
make bench       # benchmarks → benchmark/results.json (`make bench-view` shows them)
make help        # list all targets
```

`make start` prints the URLs to try: Traefik listens on <http://localhost:8080>, each sample backend has a route without the cache and one
with it, and Redis Insight shows what is stored.

## Rules

- **Yaegi runs the plugin, not the Go compiler.** Only the standard library of Go 1.22: no third-party module (not even a Redis client), no
  `unsafe`, no cgo, no generics. Code that compiles can still fail under the interpreter: `yaegi test` has to pass, not only `go test`.
- **[RFC 9111][rfc9111] to the letter, as a shared cache.** Don't "optimise" its rules. A departure from it is an option that is off by
  default, like `sortQuery`.
- **Fail open.** A Redis error or timeout forwards the request, and `New()` succeeds when Redis is unreachable.
- **Never break a response.** Nothing is buffered past `maxBodyBytes`, and streaming (`Flush`), WebSockets (`Hijack`) and trailers keep
  working.
- **Clients are not trusted, neither is what Redis returns.** Clients must not be able to multiply entries without bound, nor to evict
  stored ones other than by [invalidation](README.md#invalidation), and nothing is allocated from a length that was not checked.
- **Stored formats are stable.** Changing the Redis key invalidates the whole cache; changing the encoding of an entry or of a `Vary` marker
  bumps its version prefix.
- **Every bug fix comes with a regression test**, and what is visible through Traefik with an end-to-end one.
- **Keep it simple.** No option, dependency or abstraction without a need for it.

## Releases

The version is the content of `VERSION`: on `main`, the [CI][ci] checks that it is the next patch, minor or major of the last release.

[ci]: https://github.com/eleaven/traedis/actions/workflows/ci.yml
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
