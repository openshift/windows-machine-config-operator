FROM registry.svc.ci.openshift.org/origin/builder:golang-1.15 as build
LABEL stage=build

WORKDIR /build/windows-machine-config-operator/
COPY .git .git

# Build WMCB
WORKDIR /build/windows-machine-config-operator/windows-machine-config-bootstrapper/
COPY windows-machine-config-bootstrapper/ .
RUN make build

# Build hybrid-overlay
WORKDIR /build/windows-machine-config-operator/ovn-kubernetes/
COPY ovn-kubernetes/ .
WORKDIR /build/windows-machine-config-operator/ovn-kubernetes/go-controller/
RUN make windows

# Build promu utility tool, needed to build the windows_exporter.exe metrics binary
WORKDIR /build/windows-machine-config-operator/promu/
COPY promu/ .
RUN go install .

# Build windows_exporter
WORKDIR /build/windows-machine-config-operator/windows_exporter/
COPY windows_exporter/ .
RUN GOOS=windows promu build -v

# Build Kubernetes node binaries
WORKDIR /build/windows-machine-config-operator/kubernetes/
COPY kubernetes/ .
ENV KUBE_BUILD_PLATFORMS windows/amd64
RUN make WHAT=cmd/kubelet
RUN make WHAT=cmd/kube-proxy

# Build CNI plugins
WORKDIR /build/windows-machine-config-operator/containernetworking-plugins/
COPY containernetworking-plugins/ .
ENV CGO_ENABLED=0
RUN ./build_windows.sh

WORKDIR /build/windows-machine-config-operator/
# Copy files and directories needed to build the WMCO binary
# `make build` uses `get_version()` in `hack/common.sh` to determine the version of binary created.
# Any new file added here should be reflected in `hack/common.sh` if it dirties the git working tree.
COPY version version
COPY tools.go tools.go
COPY go.mod go.mod
COPY go.sum go.sum
COPY vendor vendor
COPY .gitignore .gitignore
COPY Makefile Makefile
COPY build build
COPY cmd cmd
COPY hack hack
COPY pkg pkg
RUN make build

# Build the operator image with following payload structure
# /payload/
#├── cni
#│   ├── flannel.exe
#│   ├── host-local.exe
#│   ├── win-bridge.exe
#│   ├── win-overlay.exe
#│   └── cni-conf-template.json
#├── hybrid-overlay-node.exe
#├── kube-node
#│   ├── kubelet.exe
#│   └── kube-proxy.exe
#├── powershell
#│   └── wget-ignore-cert.ps1
#│   └── hns.psm1
#├── windows_exporter.exe
#└── wmcb.exe

FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
LABEL stage=operator

# Copy wmcb.exe
WORKDIR /payload/
COPY --from=build /build/windows-machine-config-operator/windows-machine-config-bootstrapper/wmcb.exe .

# Copy hybrid-overlay-node.exe
COPY --from=build /build/windows-machine-config-operator/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe .

# Copy windows_exporter.exe
COPY --from=build /build/windows-machine-config-operator/windows_exporter/windows_exporter.exe .

# Copy kubelet.exe and kube-proxy.exe
WORKDIR /payload/kube-node/
COPY --from=build /build/windows-machine-config-operator/kubernetes/_output/local/bin/windows/amd64/kubelet.exe .
COPY --from=build /build/windows-machine-config-operator/kubernetes/_output/local/bin/windows/amd64/kube-proxy.exe .

# Copy CNI plugin binaries and CNI config template cni-conf-template.json
WORKDIR /payload/cni/
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/flannel.exe .
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/host-local.exe .
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/win-bridge.exe .
COPY --from=build /build/windows-machine-config-operator/containernetworking-plugins/bin/win-overlay.exe .
COPY pkg/internal/cni-conf-template.json .

# Copy required powershell scripts
WORKDIR /payload/powershell/
COPY pkg/internal/wget-ignore-cert.ps1 .
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

USER ${USER_UID}
