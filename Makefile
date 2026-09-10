# imapsync build and dev tasks.
# The binary is pure Go (no CGO) - static, no shared-library dependencies.

BIN     := imapsync
PKG     := ./cmd/imapsync
GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: build
build: ## build the stripped static binary (no CGO)
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

.PHONY: test
test: ## run all tests with the race detector
	go test -race ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## check formatting (must print nothing)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## sync go.mod / go.sum
	go mod tidy

.PHONY: check
check: fmt vet test ## fmt + vet + test

.PHONY: clean
clean: ## remove the built binary
	rm -f $(BIN)

.PHONY: help
help: ## list targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'
