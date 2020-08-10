all: lint build unit

OUTPUT_DIR="build/_output"
# Set the go mod vendor flags, if they're not already set
GOFLAGS? = $(shell go env GOFLAGS)
ifeq "$(findstring -mod=vendor,$(GOFLAGS))" "-mod=vendor"
GO_MOD_FLAGS ?=
else
GO_MOD_FLAGS ?= -mod=vendor
endif

.PHONY: build
build:
	build/build.sh ${OUTPUT_DIR} ${GO_MOD_FLAGS}

.PHONY: lint
lint:
	hack/lint-gofmt.sh
	hack/verify-vendor.sh

.PHONY: unit
unit:
	hack/unit.sh ${GO_MOD_FLAGS}

# Operator-sdk is smart enough to detect vendor directory
.PHONY: run-ci-e2e-test
run-ci-e2e-test:
	hack/run-ci-e2e-test.sh

.PHONY: clean
clean:
	rm -rf ${OUTPUT_DIR}
