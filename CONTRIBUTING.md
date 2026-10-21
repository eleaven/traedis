# Contributing

Requires Docker (with Compose), Go ≥ 1.22 and the [Yaegi](https://github.com/traefik/yaegi) version embedded in Traefik v3.7:
`go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1`

## Project structure

```text
pkg/               # the plugin (package `traedis`): one concern per file, with its `_test.go` next to it; entrypoint is in `middleware.go`
example/           # Traefik configuration of the local stack
e2e/               # end-to-end tests: Hurl files requesting Traefik
benchmark/         # benchmarks under Yaegi, k6 load test
compose.yml        # local stack: Traefik, Redis, sample backends (whoami, placeholder, imgproxy), Redis Insight
.traefik.yml       # plugin manifest of the Traefik Plugin Catalog
```

## Commands

```bash
make start       # start the local stack; Traefik restarts whenever the plugin code changes
make stop        # stop the stack (`make reset` also removes the volumes)
make test        # all the tests the CI requires: `make unit` (Go, then Yaegi), then `make e2e` (through Traefik)
make fuzz        # each fuzzer of pkg/fuzz_test.go for 30 s (FUZZTIME=5m for longer)
make help        # all the other targets, such as `make bench`
```

## Rules

- **Yaegi runs the plugin, not the Go compiler.** Only the standard library of Go 1.22: no third-party module (not even a Redis client), no
  `unsafe`, no cgo, no generics. Code that compiles can still fail under the interpreter: `yaegi test` has to pass, not only `go test`.
- **[RFC 9111][rfc9111] to the letter, as a shared cache.** Don't "optimise" its rules. A departure from it is an option that is off by
  default, like `sortQuery`.
- **Fail open.** A Redis error or timeout forwards the request, and `New()` succeeds when Redis is unreachable.
- **Never break a response.** Nothing is buffered past `maxBodyBytes`, and streaming (`Flush`), WebSockets (`Hijack`) and trailers keep
  working.
- **Clients are not trusted, neither is what Redis returns.** Clients must not be able to multiply entries without bound, nor to evict
  stored ones other than by [invalidation](SPEC.md#invalidation-by-unsafe-methods), and nothing is allocated from a length that was not checked.
- **Stored formats are stable.** Changing the Redis key invalidates the whole cache; changing the encoding of an entry or of a `Vary` marker
  bumps its version prefix.
- **[`SPEC.md`](SPEC.md) is the specification**, with the configuration and HTTP headers of the README. A change of behaviour updates it,
  and the summary of the README when it no longer holds. An option is declared in `pkg/config.go`, in the README table and in the
  `testData` of `.traefik.yml`.
- **What parses untrusted data has a fuzzer** in `pkg/fuzz_test.go`. An input that `make fuzz` finds is committed with its fix, in
  `pkg/testdata/fuzz`: it then runs with the tests. One that the CI finds is in the `fuzz-testdata` artifact of its `fuzz` job.
- **Every bug fix comes with a regression test**, and what is visible through Traefik with an end-to-end one. The files of `e2e/tests/` run
  in parallel: each one only requests URLs that its own `{{newUuid}}` makes unique.
- **Keep the code simple.** Concise and easy to follow, with comments that give the reason rather than repeat the code: the RFC section
  behind a rule, the Yaegi bug behind a workaround.

## Pull requests and releases

`make test` has to pass: the [CI][ci] runs the same tests on every pull request, without `make`, which is for developers. It also runs
the benchmarks, for information only.

The version is the content of `VERSION` (`vMAJOR.MINOR.PATCH`). To release, set it to the next patch, minor or major of the last release,
and describe it in a `## vX.Y.Z` section at the top of [`CHANGELOG.md`](CHANGELOG.md): what it breaks, adds and fixes for those who use the
plugin. The CI checks both on every pull request: a new version must follow the last release and have its section. On `main`, once the
tests pass, it creates the tag and the GitHub release of a new version, with that section as its text; its `release` job is skipped
otherwise.

[ci]: https://github.com/eleaven/traedis/actions/workflows/ci.yml
[rfc9111]: https://www.rfc-editor.org/rfc/rfc9111
