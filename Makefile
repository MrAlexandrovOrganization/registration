DOCKER_COMPOSE = docker compose
BIN = $(CURDIR)/.bin
include versions.mk

.PHONY: install install-proto install-hooks run format check test test-race test-integration build proto-gen config-check migrate import-dry-run validate-data compose-build up down logs restart init-topics
install: install-proto
	go mod download
	GOBIN="$(abspath $(BIN))" go install golang.org/x/tools/cmd/goimports@v$(GOIMPORTS_VERSION)
	python3 -m venv .tools
	.tools/bin/pip install --disable-pip-version-check -r requirements-tools.txt
install-proto:
	GOBIN="$(abspath $(BIN))" go install github.com/bufbuild/buf/cmd/buf@v$(BUF_VERSION)
	GOBIN="$(abspath $(BIN))" go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN="$(abspath $(BIN))" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$(PROTOC_GEN_GO_GRPC_VERSION)
install-hooks:
	python3 scripts/install_hooks.py
proto-gen:
	PATH="$(abspath $(BIN)):$$PATH" "$(BIN)/buf" generate
run: proto-gen
	go run ./cmd/registration serve
format: proto-gen
	$(BIN)/goimports -w cmd internal
	go fix ./...
	.tools/bin/ruff check --fix scripts
	.tools/bin/ruff format scripts
check: proto-gen
	.tools/bin/ruff check scripts
	.tools/bin/ruff format --check scripts
	python3 scripts/check_format.py
	go vet ./...
	$(MAKE) test
	$(MAKE) test-integration
	$(MAKE) test-compose
	$(MAKE) config-check
	$(MAKE) secrets-check
test: proto-gen
	go test ./... -count=1 -timeout=120s
test-race: proto-gen
	go test -race ./... -count=1 -timeout=180s
test-integration: proto-gen
	python3 scripts/test_integration.py
.PHONY: benchmark
benchmark: proto-gen
	python3 scripts/benchmark.py
.PHONY: test-compose
test-compose:
	python3 scripts/test_compose.py
build: proto-gen
	go build -trimpath -o .bin/registration ./cmd/registration
config-check:
	env -u POSTGRES_PASSWORD -u BACKEND_TOKEN -u BOT_ID -u ROOT_ID $(DOCKER_COMPOSE) --env-file config/test.env config --quiet
migrate: proto-gen
	go run ./cmd/registration migrate
import-dry-run: proto-gen
	go run ./cmd/registration import-sqlite --source "$(SOURCE)" --dry-run
validate-data: proto-gen
	go run ./cmd/registration validate-data
compose-build:
	$(DOCKER_COMPOSE) build backend
up:
	$(MAKE) compose-build
	$(DOCKER_COMPOSE) stop backend
	$(MAKE) database-up
	$(MAKE) migrate-compose
	$(DOCKER_COMPOSE) up -d --no-build --pull never --wait --wait-timeout 180 backend
down:
	$(DOCKER_COMPOSE) down
logs:
	$(DOCKER_COMPOSE) logs -f --tail=100
restart:
	$(DOCKER_COMPOSE) restart
init-topics: proto-gen
	go run ./cmd/registration init-topics
secrets-check:
	python3 scripts/check_secrets.py
clear:
	$(DOCKER_COMPOSE) down -v

.PHONY: versions
versions:
	@$(foreach v,$(VERSION_VARS),printf '%s=%s\n' '$(v)' '$($(v))';)

.PHONY: database-up
database-up:
	$(DOCKER_COMPOSE) up -d --wait --wait-timeout 180 postgres

.PHONY: migrate-compose
migrate-compose:
	$(DOCKER_COMPOSE) run --rm --no-deps --pull never migrate
