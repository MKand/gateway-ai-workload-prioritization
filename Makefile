SHELL := /bin/bash
UNAME_S := $(shell uname -s)

# Ensure Go binaries (like protoc-gen-go) installed to GOPATH/bin or ~/go/bin are in PATH
GOPATH := $(shell go env GOPATH 2>/dev/null || echo $(HOME)/go)
export PATH := $(PATH):$(GOPATH)/bin:$(HOME)/go/bin

.PHONY: default check-env check-os check-tools proto help

# Default target when running just 'make' without arguments
default:
	@echo "================================================================================"
	@echo "ERROR: Please specify a valid target to run."
	@echo "================================================================================"
	@$(MAKE) help --no-print-directory
	@exit 1

## ----------------------------------------------------------------------
## ENVIRONMENT & PREREQUISITE CHECKS
## ----------------------------------------------------------------------

# Master environment check target - add new prerequisite checks as dependencies here
check-env: check-os check-tools

check-os:
	@if [ "$(UNAME_S)" != "Linux" ]; then \
		echo "================================================================================"; \
		echo "ERROR: Operating system '$(UNAME_S)' is not supported by default."; \
		echo "Change the content of this Makefile in accordance to your operating systems"; \
		echo "and comment this check."; \
		echo "================================================================================"; \
		exit 1; \
	fi

check-tools:
	@# 1. Check Go
	@command -v go >/dev/null 2>&1 || { \
		echo "================================================================================"; \
		echo "ERROR: 'go' does not exist."; \
		echo "Please follow the instructions to install it: https://go.dev/doc/install"; \
		echo "Or on Debian/Ubuntu: sudo apt-get update && sudo apt-get install -y golang-go"; \
		echo "================================================================================"; \
		exit 1; \
	}
	@# 2. Check protoc (Protobuf compiler)
	@command -v protoc >/dev/null 2>&1 || { \
		echo "================================================================================"; \
		echo "ERROR: 'protoc' (Protocol Buffer Compiler) does not exist."; \
		echo "Please follow the instructions to install it: https://grpc.io/docs/protoc-installation/"; \
		echo "Or on Debian/Ubuntu: sudo apt-get update && sudo apt-get install -y protobuf-compiler"; \
		echo "Or download prebuilt release binaries from: https://github.com/protocolbuffers/protobuf/releases"; \
		echo "================================================================================"; \
		exit 1; \
	}
	@# 3. Check protoc-gen-go plugin
	@command -v protoc-gen-go >/dev/null 2>&1 || { \
		echo "================================================================================"; \
		echo "ERROR: 'protoc-gen-go' plugin does not exist."; \
		echo "Please install it by running:"; \
		echo "  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"; \
		echo "and ensure \$$(go env GOPATH)/bin is added to your PATH:"; \
		echo "  export PATH=\"\$$PATH:\$$(go env GOPATH)/bin\""; \
		echo "Reference: https://protobuf.dev/reference/go/go-generated/"; \
		echo "================================================================================"; \
		exit 1; \
	}
	@# 4. Check protoc-gen-go-grpc plugin
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { \
		echo "================================================================================"; \
		echo "ERROR: 'protoc-gen-go-grpc' plugin does not exist."; \
		echo "Please install it by running:"; \
		echo "  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"; \
		echo "and ensure \$$(go env GOPATH)/bin is added to your PATH:"; \
		echo "  export PATH=\"\$$PATH:\$$(go env GOPATH)/bin\""; \
		echo "Reference: https://grpc.io/docs/languages/go/quickstart/"; \
		echo "================================================================================"; \
		exit 1; \
	}

## ----------------------------------------------------------------------
## CODE GENERATION
## ----------------------------------------------------------------------

proto: check-env
	@echo "==> Generating Go code from Protobuf schemas..."
	@mkdir -p gen/go/governor/v1
	protoc \
		--proto_path=proto \
		--go_out=. \
		--go_opt=module=github.com/MKand/gateway-ai-workload-prioritization \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/MKand/gateway-ai-workload-prioritization \
		proto/governor/v1/governor.proto
	@echo "==> Protobuf Go code generated successfully in gen/go/governor/v1/"

## ----------------------------------------------------------------------
## HELP
## ----------------------------------------------------------------------

help:
	@echo "Available targets:"
	@echo "  make proto      - Run environment checks and generate Go code from proto definitions"
	@echo "  make check-env  - Run all environment and prerequisite checks (OS, go, protoc, plugins)"
	@echo "  make help       - Display this help message"
