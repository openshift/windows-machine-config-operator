all: lint build unit

OUTPUT_DIR="build/_output"

.PHONY: build
build:
	build/build.sh ${OUTPUT_DIR}

.PHONY: lint
lint:
	hack/lint-gofmt.sh

.PHONY: unit
unit:
	hack/unit.sh

.PHONY: run-ci-e2e-test
run-ci-e2e-test:
	hack/run-ci-e2e-test.sh

.PHONY: clean
clean:
	rm -rf ${OUTPUT_DIR}
