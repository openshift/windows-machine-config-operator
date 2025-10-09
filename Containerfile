FROM brew.registry.redhat.io/rh-osbs/openshift-golang-builder:rhel_9_1.24 as build

LABEL stage=build

# Silence go compliance shim output
ENV GO_COMPLIANCE_INFO=0
ENV GO_COMPLIANCE_DEBUG=0

# Set go toolchain to local, this prevents it from
# downloading the latest version
ENV GOTOOLCHAIN=local

ENV GOEXPERIMENT=strictfipsruntime

WORKDIR /build/windows-machine-config-operator/
COPY .git .git

# Build hybrid-overlay
WORKDIR /build/windows-machine-config-operator/ovn-kubernetes/
COPY ovn-kubernetes/ .
WORKDIR /build/windows-machine-config-operator/ovn-kubernetes/go-controller/
RUN make windows

# Build promu utility tool, needed to build the windows_exporter.exe metrics binary
WORKDIR /build/windows-machine-config-operator/promu/
COPY promu/ .
# Explicitly set the $GOBIN path for promu installation
RUN GOBIN=/build/windows-machine-config-operator/windows_exporter/ go install .

# Build windows_exporter
WORKDIR /build/windows-machine-config-operator/windows_exporter/
COPY windows_exporter/ .
RUN GOOS=windows ./promu build -v

# Build containerd
WORKDIR /build/windows-machine-config-operator/
COPY containerd/ containerd/
COPY Makefile Makefile
RUN make containerd

# Build containerd shim
WORKDIR /build/windows-machine-config-operator/hcsshim/
COPY hcsshim/ .
RUN GOOS=windows go build ./cmd/containerd-shim-runhcs-v1

# Build kube-log-runner
WORKDIR /build/windows-machine-config-operator/kubelet/
COPY kubelet/ .
ENV KUBE_BUILD_PLATFORMS windows/amd64
RUN make WHAT=vendor/k8s.io/component-base/logs/kube-log-runner

# Build kubelet and kube-proxy
WORKDIR /build/windows-machine-config-operator/
RUN make kubelet
RUN make kube-proxy

# Build azure-cloud-node-manager
WORKDIR /build/windows-machine-config-operator/cloud-provider-azure/
COPY cloud-provider-azure/ .
RUN GOOS=windows go build -o azure-cloud-node-manager.exe ./cmd/cloud-node-manager

# Build ecr-credential-provider
WORKDIR /build/windows-machine-config-operator/cloud-provider-aws/
COPY cloud-provider-aws/ .
RUN env -u VERSION GOOS=windows make ecr-credential-provider

# Build CNI plugins
WORKDIR /build/windows-machine-config-operator/containernetworking-plugins/
COPY containernetworking-plugins/ .
RUN CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc ./build_windows.sh

# Build csi-proxy
WORKDIR /build/windows-machine-config-operator/csi-proxy
COPY csi-proxy/ .
RUN GOOS=windows make build

WORKDIR /build/windows-machine-config-operator/
# Copy files and directories needed to build the WMCO binary
# Any new file added here should be reflected in `build/build.sh` if it dirties the git working tree.
COPY version version
COPY tools.go tools.go
COPY go.mod go.mod
COPY go.sum go.sum
COPY vendor vendor
COPY .gitignore .gitignore
COPY build build
COPY cmd cmd
COPY controllers controllers
COPY hack hack
COPY pkg pkg
RUN make build
RUN make build-daemon

# Build the operator image with following payload structure
# /payload/
#├── azure-cloud-node-manager.exe
#├── cni/
#│   ├── flannel.exe
#│   ├── host-local.exe
#│   ├── win-bridge.exe
#│   └── win-overlay.exe
#├── containerd/
#│   ├── containerd.exe
#│   └── containerd-shim-runhcs-v1.exe
#│   └── containerd_conf.toml
#├── csi-proxy/
#│   ├── csi-proxy.exe
#├── ecr-credential-provider.exe
#├── generated/
#├── hybrid-overlay-node.exe
#├── kube-node/
#│   ├── kubelet.exe
#│   ├── kube-log-runner.exe
#│   └── kube-proxy.exe
#├── powershell/
#│   ├── gcp-get-hostname.ps1
#│   ├── windows-defender-exclusion.ps1
#│   └── hns.psm1
#├── windows-exporter/
#│   ├── windows_exporter.exe
#│   └── windows-exporter-webconfig.yaml
#└── windows-instance-config-daemon.exe

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL stage=operator

# This block contains standard Red Hat container labels
LABEL name="openshift4-wincw/windows-machine-config-rhel9-operator" \
    cpe="cpe:/a:redhat:windows_machine_config:10.21::el9" \
    License="ASL 2.0" \
    io.k8s.display-name="Windows Machine Config Operator" \
    io.k8s.description="Windows Machine Config Operator" \
    description="Windows Machine Config Operator" \
    summary="Windows Machine Config Operator" \
    maintainer="Team Windows Containers <team-winc@redhat.com>" \
    com.redhat.component="windows-machine-config-operator-container" \
    io.openshift.tags=""

WORKDIR /payload/
# Copy WICD
COPY --from=build /build/windows-machine-config-operator/build/_output/bin/windows-instance-config-daemon.exe .

# Copy hybrid-overlay-node.exe
COPY --from=build /build/windows-machine-config-operator/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe .

# Copy windows_exporter.exe and TLS windows-exporter-webconfig.yaml
WORKDIR /payload/windows-exporter
COPY --from=build /build/windows-machine-config-operator/windows_exporter/windows_exporter.exe .
COPY pkg/internal/windows-exporter-webconfig.yaml .

# Copy azure-cloud-node-manager.exe
WORKDIR /payload/
COPY --from=build /build/windows-machine-config-operator/cloud-provider-azure/azure-cloud-node-manager.exe .

# Copy ecr-credential-provider
COPY --from=build /build/windows-machine-config-operator/cloud-provider-aws/ecr-credential-provider ecr-credential-provider.exe

# Copy containerd.exe, containerd-shim-runhcs-v1.exe and containerd config containerd_conf.toml
WORKDIR /payload/containerd/
COPY --from=build /build/windows-machine-config-operator/containerd/bin/containerd.exe .
COPY --from=build /build/windows-machine-config-operator/hcsshim/containerd-shim-runhcs-v1.exe .
COPY pkg/internal/containerd_conf.toml .

# Copy kubelet.exe, kube-log-runner.exe and kube-proxy.exe
WORKDIR /payload/kube-node/
COPY --from=build /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kubelet.exe .
COPY --from=build /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kube-log-runner.exe .
COPY --from=build /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kube-proxy.exe .

# Copy CNI plugin binaries
WORKDIR /payload/cni/
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/host-local.exe .
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/win-bridge.exe .
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/win-overlay.exe .

# Build csi-proxy.exe
WORKDIR /payload/csi-proxy/
COPY --from=build /build/windows-machine-config-operator/csi-proxy/bin/csi-proxy.exe .

# Create directory for generated files with open permissions, this allows WMCO to write to this directory
RUN mkdir /payload/generated
RUN chmod 0777 /payload/generated

# Copy required powershell scripts
WORKDIR /payload/powershell/
COPY pkg/internal/gcp-get-hostname.ps1 .
COPY pkg/internal/windows-defender-exclusion.ps1 .
COPY pkg/internal/hns.psm1 .

WORKDIR /

ENV OPERATOR=/usr/local/bin/windows-machine-config-operator \
    USER_UID=1001 \
    USER_NAME=windows-machine-config-operator

# install operator binary
COPY --from=build /build/windows-machine-config-operator/build/_output/bin/windows-machine-config-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

# Used to tag the released image. Should be a semver.
LABEL version="v10.21.0"

USER ${USER_UID}
