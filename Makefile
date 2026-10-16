# @see: https://stackoverflow.com/a/70550568
MAKEFLAGS += --no-print-directory
.PHONY: help start stop reset unwatch test unit fuzz e2e bench assets

# Services the tests and benchmarks need: the whole stack but Redis Insight
TEST_STACK := ingress cache cache-tls whoami placeholder imaging

# Stops the containers when the recipe ends, even when it fails or on Ctrl-C (which /bin/sh only traps when told to)
down_on_exit = trap 'docker compose down' EXIT; trap 'exit 1' INT TERM

help: ## Show this help
	@# @see: https://www.avonture.be/blog/makefile-help/
	@awk 'BEGIN { FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n" } \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

start: unwatch ## Start containers with Docker Compose and Docker Compose Watch
	docker compose up -d --force-recreate --wait --wait-timeout 120
	nohup docker compose watch </dev/null >/dev/null 2>&1 & echo $$! > /tmp/compose-watch.traedis.pid
	@echo "Open http://localhost:8080/whoami to see whoami page without caching"
	@echo "Open http://localhost:8080/whoami-cache to see whoami page with caching"
	@echo "Open http://localhost:8080/placeholder/800x600 to see sample image without caching"
	@echo "Open http://localhost:8080/placeholder-cache/800x600 to see sample image with caching"
	@echo "Open http://localhost:8080/imaging/v3YFo0Q8-M1ABfuc0gpmFX1PqQaePkuXXgHQLlobl98/rs:fill:300:200/plain/http://placeholder:3000/800x600@avif to see sample image through imgproxy without caching"
	@echo "Open http://localhost:8080/imaging-cache/v3YFo0Q8-M1ABfuc0gpmFX1PqQaePkuXXgHQLlobl98/rs:fill:300:200/plain/http://placeholder:3000/800x600@avif to see sample image through imgproxy with caching"
	@echo "Open http://localhost:5540/ to see Redis Insight"

stop: unwatch ## Stop and delete all containers
	docker compose down

reset: unwatch ## Stop, delete all containers and remove volumes
	docker compose down --remove-orphans --volumes

unwatch: ## Stop Docker Compose Watch
	@if [ -f /tmp/compose-watch.traedis.pid ]; then kill $$(cat /tmp/compose-watch.traedis.pid) 2>/dev/null || true; rm -f /tmp/compose-watch.traedis.pid; fi

test: unit e2e ## Run all tests: what the CI requires (unit, then end-to-end)

unit: export TRAEDIS_REDIS_DSN = redis://localhost:6379/15
unit: stop ## Run gofmt, go vet, then unit and Redis integration tests (compiled Go, then Yaegi)
	@command -v yaegi >/dev/null \
		|| { echo "yaegi not found: go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1" >&2; exit 1; }
	@# Fails when files need formatting, and lists them
	! gofmt -l . | grep .
	go vet ./...
	$(down_on_exit); docker compose up -d --wait --wait-timeout 120 cache \
		&& go test -race ./... && (cd pkg && yaegi test -v .)

fuzz: FUZZTIME ?= 30s
fuzz: ## Run each fuzzer of pkg/fuzz_test.go for FUZZTIME (30s): what it finds goes to pkg/testdata/fuzz
	@for target in $$(go test -list '^Fuzz' ./pkg | grep '^Fuzz'); do \
		echo "# $$target"; \
		go test -run '^$$' -fuzz "^$$target\$$" -fuzztime $(FUZZTIME) ./pkg || exit 1; \
	done

e2e: stop ## Run end-to-end tests (Hurl through Traefik)
	$(down_on_exit); docker compose up -d --wait --wait-timeout 120 $(TEST_STACK) && ./e2e/run.sh

bench: stop ## Run benchmarks (under Yaegi, then k6 through Traefik) into benchmark/results.json
	$(down_on_exit); docker compose up -d --wait --wait-timeout 120 $(TEST_STACK) && ./benchmark/run.sh

assets: ## Optimize icon.svg with SVGO and generate icon.png
	@echo "Optimize SVG…"
	docker run -u "$$(id -u):$$(id -g)" --rm -v "./assets/icon.svg:/assets/icon.svg" oven/bun:alpine bunx --bun svgo \
	--pretty --multipass "/assets/icon.svg" --output "/assets/icon.svg"
	@echo "Generate PNG…"
	docker run -u "$$(id -u):$$(id -g)" --rm -v "./assets:/assets" linuxserver/inkscape inkscape "/assets/icon.svg" \
	--export-type=png --export-png-antialias=3 --export-filename="/assets/icon.png" --export-area-drawing --export-height=256
	@echo "Center PNG…"
	docker run -u "$$(id -u):$$(id -g)" --rm -v "./assets:/assets" dpokidov/imagemagick /assets/icon.png \
	-gravity center -background none -extent 256x256 /assets/icon.png
