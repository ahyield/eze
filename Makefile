# Development tasks for eze. Run `make` or `make help` to list them.

BIN      ?= eze
PORT     ?= 8080
DATA     ?= ./data
IMAGE    ?= eze
LINT_VERSION ?= v2.15.0

# Local dev server settings, shared by `run` and `docker-run`.
DEV_ENV = EZE_DOMAIN=localhost:$(PORT) EZE_SCHEME=http EZE_AUTH=dev

.DEFAULT_GOAL := help

##@ Build and run

.PHONY: build
build: ## Build the eze binary into ./eze
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/eze

.PHONY: install
install: ## Install eze into your Go bin directory
	go install ./cmd/eze

.PHONY: run
run: build ## Run a local dev server at http://localhost:8080, or PORT= (everyone is dev@localhost)
	$(DEV_ENV) EZE_ADDR=127.0.0.1:$(PORT) EZE_DATA_DIR=$(DATA) ./$(BIN) serve

.PHONY: login
login: build ## Point the CLI at the local dev server
	./$(BIN) login http://localhost:$(PORT)

##@ Test and check

.PHONY: test
test: ## Run the tests (Node 22+ also runs the browser code checks)
	go test ./...

.PHONY: race
race: ## Run the tests with the race detector, as CI does
	go test -race ./...

.PHONY: cover
cover: ## Run the tests and open a coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: fmt
fmt: ## Format the Go code
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file needs formatting
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (installs the CI version if missing)
	@command -v golangci-lint >/dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)
	golangci-lint run

.PHONY: smoke
smoke: build ## Deploy, upload, restart and delete against a real server on local disk
	scripts/storage-smoke.sh ./$(BIN) ''

.PHONY: check
check: fmt-check vet lint race ## Everything CI's test and lint jobs run, minus Docker

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

##@ Docker

.PHONY: docker-build
docker-build: ## Build the Docker image
	docker build -t $(IMAGE) .

# Dev auth only listens on loopback, so a socat container on eze's network forwards
# localhost:$(PORT) to 127.0.0.1 inside it.
.PHONY: docker-run
docker-run: docker-build docker-stop ## Run the image in dev mode at http://localhost:8080, or PORT=
	docker run -d --name eze -p $(PORT):8081 -v eze-data:/data \
		-e EZE_DOMAIN=localhost:$(PORT) -e EZE_SCHEME=http -e EZE_AUTH=dev \
		-e EZE_ADDR=127.0.0.1:8080 $(IMAGE)
	docker run -d --name eze-proxy --network container:eze \
		alpine/socat TCP-LISTEN:8081,fork,reuseaddr TCP:127.0.0.1:8080
	@echo "eze is running at http://localhost:$(PORT) (make docker-logs, make docker-stop)"

.PHONY: docker-logs
docker-logs: ## Follow the Docker container's logs
	docker logs -f eze

.PHONY: docker-stop
docker-stop: ## Stop and remove the Docker containers (keeps the eze-data volume)
	-@docker rm -f eze-proxy eze >/dev/null 2>&1

.PHONY: docker-smoke
docker-smoke: docker-build ## Start the image and check /healthz, as CI does
	-@docker rm -f eze-smoke >/dev/null 2>&1
	docker run -d --name eze-smoke -p 18081:8080 -e EZE_DOMAIN=localhost:18081 -e EZE_AUTH=header $(IMAGE)
	@for i in $$(seq 1 20); do curl -fsS http://localhost:18081/healthz && echo && docker rm -f eze-smoke >/dev/null && exit 0; sleep 1; done; \
		docker logs eze-smoke; docker rm -f eze-smoke >/dev/null; exit 1

##@ Website (eze.page)

.PHONY: site
site: ## Preview the landing page at http://localhost:8000
	python3 -m http.server -d site 8000

.PHONY: og
og: ## Re-render site/og.png from scripts/og.html (needs Chrome and ImageMagick)
	scripts/og.sh

##@ Housekeeping

.PHONY: clean
clean: ## Remove the binary and coverage files
	rm -f $(BIN) coverage.out

.PHONY: clean-data
clean-data: ## Delete local dev data (./data and the eze-data Docker volume)
	rm -rf $(DATA)
	-docker volume rm eze-data

.PHONY: help
help: ## List the targets
	@awk 'BEGIN {FS = ":.*##"} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-z-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
