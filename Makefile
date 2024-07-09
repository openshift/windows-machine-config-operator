# WMCO_VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the WMCO_VERSION as arg of the bundle target (e.g make bundle WMCO_VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export WMCO_VERSION=0.0.2)
WMCO_VERSION ?= 10.15.3

# *_GIT_VERSION are the k8s versions. Any update to the build line could potentially require an update to the sed
# command in generate_k8s_version_commit() in hack/update_submodules.sh
KUBELET_GIT_VERSION=v1.28.11+add48d0
KUBE-PROXY_GIT_VERSION=v1.28.3+402e202
CONTAINERD_GIT_VERSION=v1.7.16
# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "preview,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=preview,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="preview,fast,stable")
CHANNELS = "preview,stable"
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
DEFAULT_CHANNEL = "stable"
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
IMAGE_TAG_BASE ?= <registry>/<operator name>

BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(WMCO_VERSION)

# Image URL to use all building/pushing image targets
IMG ?= REPLACE_IMAGE

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: lint build unit

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=windows-machine-config-operator crd webhook paths="{./cmd/..., ./controllers/..., ./pkg/...}" output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations. Must be run when adding or changing a CRD.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="{./cmd/..., ./controllers/..., ./pkg/...}"

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

##@ Build

OUTPUT_DIR="build/_output"
# Set the go mod vendor flags, if they're not already set
GOFLAGS? = $(shell go env GOFLAGS)
ifeq "$(findstring -mod=vendor,$(GOFLAGS))" "-mod=vendor"
GO_MOD_FLAGS ?=
else
GO_MOD_FLAGS ?= -mod=vendor
endif

.PHONY: build
build: fmt vet
	build/build.sh ${OUTPUT_DIR} ${WMCO_VERSION} ${GO_MOD_FLAGS}

.PHONY: build-daemon
build-daemon:
	env GOOS=windows GOARCH=amd64 go build -o ${OUTPUT_DIR}/bin/windows-instance-config-daemon.exe ./cmd/daemon

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run cmd/operator/main.go

ifndef IGNORE-NOT-FOUND
  IGNORE-NOT-FOUND = false
endif
##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | oc apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | oc delete --ignore-not-found=$(IGNORE-NOT-FOUND) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | oc apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | oc delete --ignore-not-found=$(IGNORE-NOT-FOUND) -f -

.PHONY: controller-gen
CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.4.1)
##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Tool Versions
KUSTOMIZE_VERSION ?= v4.5.5
CONTROLLER_TOOLS_VERSION ?= v0.10.0

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(LOCALBIN)/kustomize || { curl -s $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN); }

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

OS = $(shell go env GOOS)
ARCH = $(shell go env GOARCH)

.PHONY: opm
OPM = ./bin/opm
opm:
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.23.0/$(OS)-$(ARCH)-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif
BUNDLE_IMGS ?= $(BUNDLE_IMG)
# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(WMCO_VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
    BUNDLE_GEN_FLAGS += --use-image-digests
endif
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(WMCO_VERSION) ifneq ($(origin CATALOG_BASE_IMG), undefined) FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG) endif
.PHONY: catalog-build
catalog-build: opm
	$(OPM) index add --container-tool podman --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

.PHONY: bundle ## Generate bundle manifests and metadata, then validate generated files. Requires operator-sdk on $PATH.
bundle: manifests kustomize
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | operator-sdk generate bundle $(BUNDLE_GEN_FLAGS)
	operator-sdk bundle validate ./bundle
	sed -i 's/windows-machine-config-operator\.v.\.0\.0/windows-machine-config-operator.v$(WMCO_VERSION)/g' ./bundle/windows-machine-config-operator.package.yaml

.PHONY: bundle-build ## Build the bundle image.
bundle-build:
	podman build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: lint
lint:
	hack/lint-gofmt.sh
	hack/verify-vendor.sh

.PHONY: imports
imports: ## Organize imports in go files using goio
	go run ./vendor/github.com/go-imports-organizer/goio

.PHONY: unit
unit:
	hack/unit.sh ${GO_MOD_FLAGS}

.PHONY: community-bundle
community-bundle:
	hack/community/generate.sh ${WMCO_VERSION} ${ARTIFACT_DIR}

.PHONY: wicd-unit
wicd-unit:
	hack/wicd-unit.sh

.PHONY: run-ci-e2e-test
run-ci-e2e-test:
	hack/run-ci-e2e-test.sh -t basic

.PHONY: run-ci-e2e-byoh-test
run-ci-e2e-byoh-test:
	hack/run-ci-e2e-test.sh -t basic -m 0

.PHONY: run-ci-e2e-upgrade-test
run-ci-e2e-upgrade-test: ;

.PHONY: upgrade-test-setup
upgrade-test-setup:
	hack/run-ci-e2e-test.sh -t upgrade-setup -s

.PHONY: upgrade-test
upgrade-test:
	hack/run-ci-e2e-test.sh -t upgrade-test

.PHONY: clean
clean:
	rm -rf ${OUTPUT_DIR}

.PHONY: base-img
base-img:
	podman build . -t wmco-base -f build/Dockerfile.base

.PHONY: wmco-img
wmco-img:
	podman build . -t $(IMG) -f build/Dockerfile.wmco
	podman push $(IMG)

.PHONY: kubelet
kubelet:
	KUBE_GIT_VERSION=$(KUBELET_GIT_VERSION) KUBE_BUILD_PLATFORMS=windows/amd64 make -C kubelet WHAT=cmd/kubelet

.PHONY: kube-proxy
kube-proxy:
	KUBE_GIT_VERSION=$(KUBE-PROXY_GIT_VERSION) KUBE_BUILD_PLATFORMS=windows/amd64 make -C kube-proxy WHAT=cmd/kube-proxy

.PHONY : containerd
containerd:
	GOOS=windows VERSION=$(CONTAINERD_GIT_VERSION) make -C containerd bin/containerd.exe
