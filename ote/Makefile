.PHONY: unit
unit:
	./hack/unit.sh

.PHONY: ote-unit
ote-unit: ## Run focused unit tests for OTE TLS helper logic.
	./hack/tls-unit.sh

.PHONY: gen-csi-manifests
gen-csi-manifests: ## Regenerate test/e2e/csi_manifests_gen.go from hack/manifests/csi.
	./hack/gen-csi-manifests.sh

.PHONY: verify-csi-manifests
verify-csi-manifests: ## Verify the generated Windows CSI manifests match hack/manifests/csi.
	@tmp=$$(mktemp) || exit 1; trap 'rm -f "$$tmp"' EXIT; ./hack/gen-csi-manifests.sh "$$tmp" && diff -u test/e2e/csi_manifests_gen.go "$$tmp"

.PHONY: verify-ote-discovery
verify-ote-discovery: ## Verify the OTE extension discovers all tests without Ginkgo decorator errors.
	./hack/verify-ote-discovery.sh

.PHONY: lint
lint: verify-csi-manifests verify-ote-discovery ## Run OTE lint checks.
