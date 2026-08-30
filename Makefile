.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

VERSION ?= dev
IMAGE   ?= ghcr.io/yama6a/longhorn-replica-affinity

.PHONY: build
build: ## Build the binary into bin/.
	go build -ldflags="-X main.version=$(VERSION)" -o bin/longhorn-replica-affinity ./cmd/longhorn-replica-affinity

.PHONY: test
test: ## Run tests.
	go test ./... -count=1

.PHONY: cover
cover: ## Run tests and open the coverage report.
	go test ./... -coverprofile=cover.out
	go tool cover -html=cover.out

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./... -c .golangci.yaml

.PHONY: tidy
tidy: ## Tidy go modules.
	go mod tidy

.PHONY: image
image: ## Build the multi-arch image locally (does not push).
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) -f .build/Dockerfile -t $(IMAGE):$(VERSION) .

.PHONY: ci
ci: vet lint test ## Run all CI checks.
