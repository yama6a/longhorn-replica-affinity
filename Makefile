GO_LINT_CONFIG     ?= .build/golangci.yaml
CANONICAL_LINT_URL := https://raw.githubusercontent.com/yama6a/gha/v2/.golangci.yaml
VERSION            ?= dev
IMAGE              ?= ghcr.io/yama6a/longhorn-replica-affinity

.PHONY: help lint-config generate fmt fmt-check lint vet test cover vuln tidy tidy-check \
	generate-check mod build chart image ci

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

lint-config: ## Fetch the canonical lint config, merging .golangci.local.yaml over it.
	mkdir -p .build
	curl -fsSL $(CANONICAL_LINT_URL) -o .build/canonical-golangci.yaml
	if [ -f .golangci.local.yaml ]; then \
		yq eval-all '. as $$item ireduce ({}; . *+ $$item)' \
			.build/canonical-golangci.yaml .golangci.local.yaml > $(GO_LINT_CONFIG); \
	else \
		cp .build/canonical-golangci.yaml $(GO_LINT_CONFIG); \
	fi

generate: ## Run go generate.
	go generate ./...

fmt: lint-config ## Apply the formatters.
	golangci-lint fmt -c $(GO_LINT_CONFIG)

fmt-check: lint-config ## Check formatting.
	golangci-lint fmt --diff -c $(GO_LINT_CONFIG)

lint: lint-config ## Run golangci-lint.
	golangci-lint run ./... -c $(GO_LINT_CONFIG)

vet: ## Run go vet.
	go vet ./...

test: ## Run tests with the race detector.
	go test ./... -race -count=1

cover: ## Run tests and open the coverage report.
	go test ./... -coverprofile=cover.out -covermode=atomic
	go tool cover -html=cover.out

vuln: ## Scan dependencies for known vulnerabilities.
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy: ## Tidy go modules.
	go mod tidy

tidy-check: ## Fail if go.mod or go.sum are not tidy.
	go mod tidy
	git diff --exit-code -- go.mod go.sum

generate-check: generate ## Fail if generated code is stale.
	git diff --exit-code

mod: ## Update every dependency and tidy.
	go get -u -t ./...
	go mod tidy

build: ## Build the binary into bin/.
	go build -ldflags="-X main.version=$(VERSION)" -o bin/longhorn-replica-affinity ./cmd/longhorn-replica-affinity

chart: ## Lint, unit-test and render the Helm chart.
	helm lint charts/longhorn-replica-affinity
	helm unittest charts/longhorn-replica-affinity
	helm template lra charts/longhorn-replica-affinity -n lra \
		--api-versions monitoring.coreos.com/v1 >/dev/null

image: ## Build the multi-arch image locally (does not push).
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) -f .build/Dockerfile -t $(IMAGE):$(VERSION) .

ci: tidy-check generate-check fmt-check lint vet test vuln chart ## Run all CI checks.
