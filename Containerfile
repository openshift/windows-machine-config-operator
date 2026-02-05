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

# Build ecr-credential-provider and rename it to have the appropriate extension
WORKDIR /build/windows-machine-config-operator/cloud-provider-aws/
COPY cloud-provider-aws/ .
RUN env -u VERSION GOOS=windows make ecr-credential-provider && mv ecr-credential-provider ecr-credential-provider.exe

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
#├── windows-instance-config-daemon.exe
#└── sha256sum

WORKDIR /payload/
# Copy WICD
RUN pushd /build/windows-machine-config-operator/build/_output/bin/ && tar -cf - windows-instance-config-daemon.exe | gzip -9 > /payload/windows-instance-config-daemon.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/build/_output/bin/windows-instance-config-daemon.exe >> sha256sum && \
# Copy hybrid-overlay-node.exe
pushd /build/windows-machine-config-operator/ovn-kubernetes/go-controller/_output/go/bin/windows/ && tar -cf - hybrid-overlay-node.exe | gzip -9 > /payload/hybrid-overlay-node.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe >> sha256sum && \
# Copy windows_exporter.exe and TLS windows-exporter-webconfig.yaml
mkdir /payload/windows-exporter && \
pushd /build/windows-machine-config-operator/windows_exporter/ && tar -cf - windows_exporter.exe | gzip -9 > /payload/windows-exporter/windows_exporter.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/windows_exporter/windows_exporter.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/pkg/internal/ && tar -cf - windows-exporter-webconfig.yaml | gzip -9 > /payload/windows-exporter/windows-exporter-webconfig.yaml.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/pkg/internal/windows-exporter-webconfig.yaml >> sha256sum && \
# Copy azure-cloud-node-manager.exe
pushd /build/windows-machine-config-operator/cloud-provider-azure/ && tar -cf - azure-cloud-node-manager.exe | gzip -9 > /payload/azure-cloud-node-manager.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/cloud-provider-azure/azure-cloud-node-manager.exe >> sha256sum && \
# Copy ecr-credential-provider
pushd /build/windows-machine-config-operator/cloud-provider-aws/ && tar -cf - ecr-credential-provider.exe | gzip -9 > /payload/ecr-credential-provider.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/cloud-provider-aws/ecr-credential-provider.exe >> sha256sum && \
# Copy containerd.exe, containerd-shim-runhcs-v1.exe and containerd config containerd_conf.toml
mkdir /payload/containerd && \
pushd /build/windows-machine-config-operator/containerd/bin/ && tar -cf - containerd.exe | gzip -9 > /payload/containerd/containerd.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/containerd/bin/containerd.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/hcsshim/ && tar -cf - containerd-shim-runhcs-v1.exe | gzip -9 > /payload/containerd/containerd-shim-runhcs-v1.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/hcsshim/containerd-shim-runhcs-v1.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/pkg/internal/ && tar -cf - containerd_conf.toml | gzip -9 > /payload/containerd/containerd_conf.toml.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/pkg/internal/containerd_conf.toml >> sha256sum && \
# Copy kubelet.exe, kube-log-runner.exe and kube-proxy.exe
mkdir /payload/kube-node && \
pushd /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/ && tar -cf - kubelet.exe | gzip -9 > /payload/kube-node/kubelet.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kubelet.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/ && tar -cf - kube-log-runner.exe | gzip -9 > /payload/kube-node/kube-log-runner.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kube-log-runner.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/ && tar -cf - kube-proxy.exe | gzip -9 > /payload/kube-node/kube-proxy.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/kubelet/_output/local/bin/windows/amd64/kube-proxy.exe >> sha256sum && \
# Copy CNI plugin binaries
mkdir /payload/cni && \
pushd /build/windows-machine-config-operator/containernetworking-plugins/bin/ && tar -cf - host-local.exe | gzip -9 > /payload/cni/host-local.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/containernetworking-plugins/bin/host-local.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/containernetworking-plugins/bin/ && tar -cf - win-bridge.exe | gzip -9 > /payload/cni/win-bridge.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/containernetworking-plugins/bin/win-bridge.exe >> sha256sum && \
pushd /build/windows-machine-config-operator/containernetworking-plugins/bin/ && tar -cf - win-overlay.exe | gzip -9 > /payload/cni/win-overlay.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/containernetworking-plugins/bin/win-overlay.exe >> sha256sum && \
# Copy csi-proxy.exe
mkdir /payload/csi-proxy && \
pushd /build/windows-machine-config-operator/csi-proxy/bin/ && tar -cf - csi-proxy.exe | gzip -9 > /payload/csi-proxy/csi-proxy.exe.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/csi-proxy/bin/csi-proxy.exe >> sha256sum && \
# Copy required powershell scripts
mkdir /payload/powershell && \
pushd /build/windows-machine-config-operator/pkg/internal/ && tar -cf - gcp-get-hostname.ps1 | gzip -9 > /payload/powershell/gcp-get-hostname.ps1.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/pkg/internal/gcp-get-hostname.ps1 >> sha256sum && \
pushd /build/windows-machine-config-operator/pkg/internal/ && tar -cf - windows-defender-exclusion.ps1 | gzip -9 > /payload/powershell/windows-defender-exclusion.ps1.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/pkg/internal/windows-defender-exclusion.ps1 >> sha256sum && \
pushd /build/windows-machine-config-operator/pkg/internal/ && tar -cf - hns.psm1 | gzip -9 > /payload/powershell/hns.psm1.tar.gz && popd && \
sha256sum /build/windows-machine-config-operator/pkg/internal/hns.psm1 >> sha256sum && \
chmod -R 644 /payload && chmod -R +X /payload &&\
# Create directory for generated files with open permissions, this allows WMCO to write to this directory
mkdir -m 0777 /payload/generated

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest@sha256:759f5f42d9d6ce2a705e290b7fc549e2d2cd39312c4fa345f93c02e4abb8da95
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
    distribution-scope="public" \
    url="https://catalog.redhat.com/en/software/container-stacks/detail/66b6400d1db8d82852703bc1" \
    vendor="Red Hat, Inc." \
    com.redhat.component="windows-machine-config-operator-container" \
    io.openshift.tags=""

COPY --from=build /payload /payload

# create licenses directory
# See https://docs.redhat.com/en/documentation/red_hat_software_certification/2025/html-single/red_hat_openshift_software_certification_policy_guide/index#con-image-content-requirements_openshift-sw-cert-policy-container-images
COPY LICENSE /licenses/

ENV OPERATOR=/usr/local/bin/windows-machine-config-operator \
    USER_UID=1001 \
    USER_NAME=windows-machine-config-operator

# install operator binary
COPY --from=build /build/windows-machine-config-operator/build/_output/bin/windows-machine-config-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

# Used to tag the released image. Should be a semver.
LABEL version="v10.21.1"
LABEL release="v10.21.1"

USER ${USER_UID}
