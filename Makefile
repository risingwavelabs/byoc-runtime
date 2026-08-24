# https://github.com/golangci/golangci-lint/releases/latest
GOLANGCI_LINT_VERSION="2.13.1"
# https://github.com/uber-go/mock/releases
MOCK_GEN_VERSION=0.6.0
CURR_DIR=$(shell pwd)

RED    = \033[0;31m
GREEN  = \033[0;32m
YELLOW = \033[0;33m
BLUE   = \033[0;36m
RESET  = \033[0m

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make ${BLUE}<target>${RESET}\n\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  ${BLUE}%-30s${RESET} %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s${RESET}\n", substr($$0, 5) }' $(MAKEFILE_LIST)


##@ linting and formatting

check-golangci-lint:
	@./bin/golangci-lint --version | grep -qF "$(GOLANGCI_LINT_VERSION)" || { \
		echo "golangci-lint not found or version mismatch. Installing..."; \
		$(MAKE) install-golangci-lint; \
	}

install-golangci-lint: ## install golang lint
	curl -sSfL https://golangci-lint.run/install.sh | sh -s v${GOLANGCI_LINT_VERSION}

lint: check-golangci-lint ## Run golang lint
	./bin/golangci-lint run --config .golangci.yaml

lint-fix: check-golangci-lint ## Fix golang lint errors
	./bin/golangci-lint run --config .golangci.yaml --fix

ut:
	COLOR=ALWAYS go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -html coverage.out -o coverage.html

check-diff:
	@echo =======uncommitted changes========
	@echo $$(git diff --name-only);
	@echo $$(git clean -d --dry-run | grep -o '\S*$$');
	@echo ============= end ================
	@if [ -n "$$(git diff --name-only)" ] || [ -n "$$(git clean -d --dry-run | grep -o '\S*$$')" ]; then \
		echo "Error: There are uncommitted changes"; \
		exit 1; \
	fi

mod-tidy: ## Run go mod tidy
	go mod tidy

mod-tidy-check: mod-tidy check-diff

##@ Mock generation

cleanup-mock: ## Remove mock binary
	rm -rf ./bin/mockgen

check-mock:
	@./bin/mockgen --version | grep -qF "$(MOCK_GEN_VERSION)" || { \
		echo "mockgen not found or version mismatch. Installing..."; \
		$(MAKE) install-mock; \
	}

install-mock: ## Install mock tool
	GOBIN=${CURR_DIR}/bin go install go.uber.org/mock/mockgen@v$(MOCK_GEN_VERSION)

clean-gen-mock: ## Remove generated mock files
	@rm -f pkg/terraform/mock_gen.go

gen-mock: check-mock clean-gen-mock ## Generate mocks
	@./bin/mockgen -self_package=github.com/risingwavelabs/byoc-runtime/pkg/terraform -package=terraform -destination=pkg/terraform/mock_gen.go github.com/risingwavelabs/byoc-runtime/pkg/terraform Executor,ExecutorFactory,Installer,HTTPClient

##@ Code generation

codegen: gen-mock ## Run all code generators

codegen-check: codegen check-diff ## Check that generated code is up-to-date
