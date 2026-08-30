# PolarBEAM build system.
# `make build` / `make test` are fully offline (vendored deps only).
# proto/web/vendor targets need dev tooling and are never part of a release build.

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS    = -s -w \
             -X github.com/devalexllc/polarbeam/internal/version.Version=$(VERSION) \
             -X github.com/devalexllc/polarbeam/internal/version.Commit=$(COMMIT)
GOBUILD    = CGO_ENABLED=0 $(GO) build -mod=vendor -trimpath -ldflags '$(LDFLAGS)'

COMPOSE_BASE = deploy/compose/docker-compose.yml
COMPOSE_DEV  = deploy/compose-dev/docker-compose.dev.yml
# Dev environments ALWAYS compose base + overlay together. Composing the base
# alone silently removes overlay services (fake agents, their enrollment state).
COMPOSE      = docker compose -f $(COMPOSE_BASE) -f $(COMPOSE_DEV)

# Published image registry/namespace (ghcr requires lowercase).
REGISTRY ?= ghcr.io/devalexllc

.PHONY: all build server agent test lint vet fmt-check proto web web-fix vendor notices up down reset logs ps seed clean images bundle

all: build

build: server agent

server:
	$(GOBUILD) -o bin/polarbeam-server ./cmd/polarbeam-server

agent:
	$(GOBUILD) -o bin/polarbeam-agent ./cmd/polarbeam-agent

test:
	CGO_ENABLED=0 $(GO) test -mod=vendor ./...

vet:
	$(GO) vet -mod=vendor ./...

# Fail if any first-party Go file is not gofmt-clean (CI runs this in
# offline-build; the toolchain's own gofmt, no external tools). The file
# list comes from git — tracked + untracked-unignored, minus vendor/ — so
# new files are covered and node_modules/ is never walked. A gofmt error
# (unparsable file) fails the gate too, not just formatting drift.
fmt-check:
	@fail=0; \
	files=$$(git ls-files -co --exclude-standard -- '*.go' | grep -v '^vendor/' | xargs gofmt -l) || fail=1; \
	if [ -n "$$files" ]; then echo "gofmt needed on:"; echo "$$files"; fail=1; fi; \
	exit $$fail

lint: vet fmt-check
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not installed; ran go vet only"; \
	fi

# Production images for the local architecture (CI does multi-arch via
# buildx). The agent MUST name --target release: the Dockerfile's default
# target is the dev image.
images:
	docker build -f deploy/docker/server.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/polarbeam-server:$(VERSION) .
	docker build -f deploy/docker/agent.Dockerfile --target release \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/polarbeam-agent:$(VERSION) .
	docker build -f deploy/docker/proxy.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/polarbeam-proxy:$(VERSION) .

# Air-gap install bundle (images tar + compose + docs).
# Uses local images when present; run `make images` first for a
# fully-local build.
bundle:
	deploy/release/build-bundle.sh $(VERSION) $(BUNDLE_ARCH)

# Default to the Docker host's architecture: `make images` builds native
# images, so a hardcoded amd64 would mislabel bundles built on arm64.
BUNDLE_ARCH ?= $(shell docker version --format '{{.Server.Arch}}' 2>/dev/null || echo amd64)

# ---- dev-time regeneration (network/tooling allowed; outputs are committed) ----

# buf and both protoc plugins are pinned in go.mod's tool block and run from
# vendor/, so regen needs no host tooling and produces identical output
# everywhere — offline-build regenerates and diffs to catch proto drift.
# Delete generated files first: buf only writes outputs for current inputs,
# so without the sweep a deleted/renamed proto would leave its stale .pb.go
# behind and regen-and-diff would miss it.
proto:
	find internal/pb -name '*.pb.go' -delete
	$(GO) run -mod=vendor github.com/bufbuild/buf/cmd/buf generate

# Lint and format-check before building: the SPA gate is the only place the
# committed web/dist can be regenerated, so it is also where style problems
# must surface. `make web-fix` auto-fixes what it can.
# The license step needs node_modules, so it runs here rather than in
# `notices` (which must stay offline). Chained into `notices` because
# web/THIRD-PARTY-LICENSES is one of its inputs — regenerating the bundle
# without regenerating attribution is exactly the drift CI now rejects.
web:
	# The merge parity check runs FIRST and needs no node_modules: it reads
	# the same fixture internal/server/thresholds does and fails if the two
	# resolvers disagree, which would make the dashboard and the outage
	# detector grade the same measurement differently.
	node web/tools/check-threshold-merge.ts
	cd web && pnpm install --frozen-lockfile && pnpm run test && pnpm run lint && pnpm run fmt:check && pnpm run build \
		&& node tools/gen-spa-licenses.mjs
	$(MAKE) notices

web-fix:
	cd web && pnpm run lint:fix && pnpm run fmt

vendor:
	$(GO) mod tidy
	$(GO) mod vendor
	$(MAKE) notices

# Third-party attribution, regenerated from vendor/. Offline and
# deterministic, unlike the other targets in this section, so CI runs it and
# lets the "working tree must stay clean" step catch drift — a new dependency
# whose attribution was never added fails the PR instead of shipping
# unattributed. Chained off `vendor` so the two can never disagree.
notices:
	./tools/gen-third-party-notices.sh

# ---- dev stack ----

# Dev default password; production sets POLARBEAM_DB_PASSWORD explicitly.
up:
	POLARBEAM_DB_PASSWORD=$${POLARBEAM_DB_PASSWORD:-polarbeam-dev} $(COMPOSE) up -d --build

down:
	POLARBEAM_DB_PASSWORD=$${POLARBEAM_DB_PASSWORD:-polarbeam-dev} $(COMPOSE) down

# Full dev reset: tear down INCLUDING volumes so the next `make up` gets a
# fresh DB/CA/tokens. This is the "recreate dev DBs" step the docs call
# `down -v` — which plain `make down -v` cannot do (make eats -v as its
# own --version flag).
reset:
	POLARBEAM_DB_PASSWORD=$${POLARBEAM_DB_PASSWORD:-polarbeam-dev} $(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f --tail=100

ps:
	$(COMPOSE) ps

# Load 90 days of synthetic probe history for the aggregate/percentile
# pipeline (M5 gate). Needs the dev stack up with agents enrolled.
seed:
	POLARBEAM_DB_PASSWORD=$${POLARBEAM_DB_PASSWORD:-polarbeam-dev} $(COMPOSE) exec -T server polarbeam-server seed --config /etc/polarbeam/server.yaml --days 90

clean:
	rm -rf bin/
