GO ?= go
PODMAN ?= podman
COMPOSE ?= podman compose

# Everything a build writes goes here, so `make clean` removes one directory.
BUILD_DIR ?= build

# Both binaries, without cgo, like the images: keera-agents, the control
# plane, and keera-agents-runtime, which runs in agents' and workspaces'
# containers.
.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=0 $(GO) build -trimpath -o $(BUILD_DIR)/ ./cmd/keera-agents ./cmd/keera-agents-runtime

.PHONY: check
check: test vet fmt-check
	@if command -v node >/dev/null 2>&1; then $(MAKE) --no-print-directory check-js; \
	else echo "node is not on PATH, so the panel's JavaScript was not checked. Install Node.js 22 or later."; fi

.PHONY: test
test:
	@$(GO) test -race -cover ./...

# wait-for-postgres runs $(1), a pg_isready, once a second until it
# succeeds, and fails after a minute.
wait-for-postgres = printf 'waiting for postgres'; for i in $$(seq 1 60); do \
	if $(1) >/dev/null 2>&1; then echo " ready"; break; fi; \
	if [ $$i = 60 ]; then echo " timed out"; exit 1; fi; printf '.'; sleep 1; done

# The store tests skip without Postgres. This starts a throwaway one, unless
# KEERA_AGENTS_TEST_DATABASE_URL points at your own.
PG_IMAGE ?= docker.io/library/postgres:18
PG_CONTAINER ?= keera-agents-test-pg
PG_PORT ?= 55433
PG_DSN ?= postgres://keera:keera@127.0.0.1:$(PG_PORT)/keera_agents_test?sslmode=disable
# Tests that need Postgres live in *_integration_test.go files, so a new
# package with them is picked up here.
INTEGRATION_PKGS = $(sort $(patsubst %/,./%,$(dir $(shell find internal -name '*_integration_test.go'))))
INTEGRATION_TESTS = $(GO) test -race -count=1 -p 1 $(INTEGRATION_PKGS)

.PHONY: test-integration
test-integration:
ifneq ($(KEERA_AGENTS_TEST_DATABASE_URL),)
	@$(INTEGRATION_TESTS)
else
	@$(PODMAN) rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	@$(PODMAN) run -d --name $(PG_CONTAINER) -e POSTGRES_USER=keera -e POSTGRES_PASSWORD=keera \
		-e POSTGRES_DB=keera_agents_test -p 127.0.0.1:$(PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@trap '$(PODMAN) rm -f $(PG_CONTAINER) >/dev/null 2>&1' EXIT; \
		$(call wait-for-postgres,$(PODMAN) exec $(PG_CONTAINER) pg_isready -U keera -d keera_agents_test); \
		KEERA_AGENTS_TEST_DATABASE_URL='$(PG_DSN)' $(INTEGRATION_TESTS)
endif

.PHONY: check-all
check-all: check test-integration

# The panel has no build step, so this checks the syntax of every module and
# runs the unit tests of its pure helpers.
.PHONY: check-js
check-js:
	@for f in $$(find internal/webui/assets -name '*.js'); do node --check "$$f" || exit 1; done
	@node --test 'internal/webui/test/*.test.mjs'

.PHONY: vet
vet:
	@$(GO) vet ./...

GOLANGCI_LINT ?= golangci-lint

# gofumpt through golangci-lint when it is there, as `make fmt` formats;
# plain gofmt otherwise.
.PHONY: fmt-check
fmt-check:
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		$(GOLANGCI_LINT) fmt --diff || { echo "not formatted: run make fmt"; exit 1; }; \
	else out=$$(gofmt -l cmd internal); \
		test -z "$$out" || { echo "not gofmt-clean:"; echo "$$out"; exit 1; }; fi

.PHONY: fmt
fmt:
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then $(GOLANGCI_LINT) fmt; \
		else gofmt -w cmd internal; fi

.PHONY: lint
lint:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint is not on PATH. Install it with:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"; \
		exit 1; }
	@$(GOLANGCI_LINT) run ./...

# Passed in, since .git is not in the build context.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo devel)
IMAGE ?= localhost/keera-agents:latest
AGENT_IMAGE ?= localhost/keera-agent:latest
WORKSPACE_IMAGE ?= localhost/keera-workspace:latest

# The images come from the one Containerfile, which says how they differ.
.PHONY: image
image:
	@$(PODMAN) build --target control-plane -t $(IMAGE) --build-arg VERSION='$(VERSION)' .

.PHONY: agent-image
agent-image:
	@$(PODMAN) build --target agent -t $(AGENT_IMAGE) --build-arg VERSION='$(VERSION)' .

.PHONY: workspace-image
workspace-image:
	@$(PODMAN) build --target workspace -t $(WORKSPACE_IMAGE) --build-arg VERSION='$(VERSION)' .

# What is inside each image, for auditors and vulnerability scanners: its Go
# binary and that binary's modules, Alpine's packages in the agent image, and Debian's
# and npm's in the workspace image. The images
# are rebuilt first (cached), so the SBOMs match the source.
SYFT ?= syft
# Pinned, so two builds of one commit describe it the same way. Dependabot does
# not read this file.
SYFT_IMAGE ?= docker.io/anchore/syft:v1.57.2
SBOM_DIR ?= $(BUILD_DIR)/sbom
# CycloneDX is what scanners take. For another syft format, such as spdx-json,
# change the file names below too.
SBOM_FORMAT ?= cyclonedx-json

# syft runs from its own image unless it is on PATH. It reads an OCI archive,
# so it needs no container socket and works under podman and docker. In the
# container it reads /scan and writes /out. --source-name and --source-version
# keep it from naming the subject after a path under /tmp.
.PHONY: sbom
sbom: image agent-image workspace-image
	@mkdir -p $(SBOM_DIR)
	@out=$$(cd $(SBOM_DIR) && pwd); \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT INT TERM; \
	if command -v $(SYFT) >/dev/null 2>&1; then \
		run_syft() { $(SYFT) "$$@"; }; scan=$$tmp; dest=$$out; \
	else \
		run_syft() { $(PODMAN) run --rm -v "$$tmp:/scan:z" -v "$$out:/out:z" $(SYFT_IMAGE) "$$@"; }; \
		scan=/scan; dest=/out; \
	fi; \
	for pair in '$(IMAGE)|keera-agents' '$(AGENT_IMAGE)|keera-agent' '$(WORKSPACE_IMAGE)|keera-workspace'; do \
		image=$${pair%|*}; name=$${pair#*|}; \
		id=$$($(PODMAN) image inspect --format '{{.Id}}' "$$image") || exit 1; \
		$(PODMAN) save --format oci-archive -o "$$tmp/$$name.tar" "$$image" >/dev/null 2>&1 || exit 1; \
		run_syft scan "oci-archive:$$scan/$$name.tar" \
			--source-name "$$image" --source-version "sha256:$$id" \
			-o "$(SBOM_FORMAT)=$$dest/$$name.cdx.json" || exit 1; \
		echo "wrote $(SBOM_DIR)/$$name.cdx.json for $$image ($$(echo "$$id" | cut -c1-12))"; \
	done

# ---------------------------------------------------------------------------
# The development loop: Postgres in a container, keera-agents on the host
# under air, so the podman driver can start agents next to it. Keera Gateway
# runs from its own repository with `make dev`; see README.md.
# ---------------------------------------------------------------------------

AIR ?= air
DEV_ENV ?= compose/.env
DEV_COMPOSE ?= -f compose/compose.yaml

.PHONY: dev
dev: dev-env dev-db
	@command -v $(AIR) >/dev/null 2>&1 || { \
		echo "air is not on PATH. Install it with:"; \
		echo "  go install github.com/air-verse/air@latest"; \
		exit 1; }
	@$(PODMAN) image exists $(AGENT_IMAGE) || { \
		echo "building the agent image first ($(AGENT_IMAGE))"; $(MAKE) --no-print-directory agent-image; }
	@$(PODMAN) image exists $(WORKSPACE_IMAGE) || { \
		echo "building the workspace image first ($(WORKSPACE_IMAGE))"; $(MAKE) --no-print-directory workspace-image; }
	@echo
	@echo "  panel    http://localhost:8070"
	@echo "  gateway  $$(sed -n 's/^KEERA_AGENTS_GATEWAY_URL=//p' $(DEV_ENV))"
	@echo
	@echo "The gateway needs, in its compose/.env:"
	@echo "  KEERA_PUBLIC_URL=http://localhost:8080"
	@echo "  KEERA_AGENTS_URL=http://localhost:8070"
	@echo "  KEERA_AGENTS_SERVICE_KEY=$$(sed -n 's/^KEERA_AGENTS_SERVICE_KEY=//p' $(DEV_ENV))"
	@echo
	@# The gateway and public URLs come from $(DEV_ENV). Agents run in podman
	@# containers and reach the host by its own name.
	@set -a; . ./$(DEV_ENV); set +a; \
		KEERA_AGENTS_DATABASE_URL="$${KEERA_AGENTS_DATABASE_URL:-postgres://keera:$${KEERA_AGENTS_DB_PASSWORD:-keera}@127.0.0.1:5433/keera_agents?sslmode=disable}" \
		KEERA_AGENTS_DRIVER="$${KEERA_AGENTS_DRIVER:-podman}" \
		KEERA_AGENTS_IMAGE="$${KEERA_AGENTS_IMAGE:-$(AGENT_IMAGE)}" \
		KEERA_AGENTS_WORKSPACE_IMAGE="$${KEERA_AGENTS_WORKSPACE_IMAGE:-$(WORKSPACE_IMAGE)}" \
		KEERA_AGENTS_RUNTIME_URL="$${KEERA_AGENTS_RUNTIME_URL:-http://host.containers.internal:8070}" \
		KEERA_AGENTS_RUNTIME_GATEWAY_URL="$${KEERA_AGENTS_RUNTIME_GATEWAY_URL:-http://host.containers.internal:8080}" \
		KEERA_AGENTS_LOG_FORMAT=text KEERA_AGENTS_LOG_LEVEL=debug \
		$(AIR)

# Fresh development secrets, once.
.PHONY: dev-env
dev-env:
	@if [ ! -f $(DEV_ENV) ]; then \
		echo "generating $(DEV_ENV) with fresh development credentials"; \
		service=$$(od -An -vtx1 -N32 /dev/urandom | tr -d ' \n'); \
		secret=$$(od -An -vtx1 -N32 /dev/urandom | tr -d ' \n'); \
		sed -e "s|^KEERA_AGENTS_SERVICE_KEY=.*|KEERA_AGENTS_SERVICE_KEY=$$service|" \
		    -e "s|^KEERA_AGENTS_SECRET_KEY=.*|KEERA_AGENTS_SECRET_KEY=$$secret|" \
		    compose/.env.example > $(DEV_ENV).tmp && mv $(DEV_ENV).tmp $(DEV_ENV); \
		chmod 0600 $(DEV_ENV); \
	fi

.PHONY: dev-db
dev-db:
	@$(COMPOSE) $(DEV_COMPOSE) up -d keera-agents-db
	@$(call wait-for-postgres,$(COMPOSE) $(DEV_COMPOSE) exec -T keera-agents-db pg_isready -U keera)

# Takes the database container down and keeps its volume.
.PHONY: dev-down
dev-down:
	@$(COMPOSE) $(DEV_COMPOSE) down

.PHONY: clean
clean:
	@rm -rf $(BUILD_DIR)
