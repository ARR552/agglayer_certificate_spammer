GO_BASE := $(shell pwd)
GO_BIN := $(GO_BASE)
GO_ENV_VARS := GO_BIN=$(GO_BIN)
GO_BINARY := agglayer-certificate-spammer
GO_CMD := $(GO_BASE)/cmd

LINT := $$(go env GOPATH)/bin/golangci-lint run --timeout=5m -E whitespace -E gosec -E gci -E misspell -E mnd -E gofmt -E goimports --exclude-use-default=false --max-same-issues 0
BUILD := $(GO_ENV_VARS) go build -o $(GO_BIN)/$(GO_BINARY) $(GO_CMD)

.PHONY: build
build: ## Build the binary locally into ./
	$(BUILD)

.PHONY: lint
lint: ## runs linter
	$(LINT)

.PHONY: install-linter
install-linter: ## Installs the linter
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin v1.63.4

