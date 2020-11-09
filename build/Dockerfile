# We cannot use a golang image as building the kubelet requires rsync and that is not present plus there is no easy way
# to install it.
FROM registry.access.redhat.com/ubi8/ubi-minimal as build
LABEL stage=build
RUN microdnf -y install rsync make git tar findutils diffutils
RUN mkdir /build/

# Install go 1.15
RUN mkdir /build/go
WORKDIR /build/go
RUN curl https://golang.org/dl/go1.15.4.linux-amd64.tar.gz -L -o go.tar.gz
# Check sha256
RUN echo "eb61005f0b932c93b424a3a4eaa67d72196c79129d9a3ea8578047683e2c80d5  go.tar.gz" |sha256sum -c
RUN tar -C /usr/local -xzf go.tar.gz
ENV PATH=${PATH}:/usr/local/go/bin

# Build WMCB
WORKDIR /build/windows-machine-config-bootstrapper/
COPY windows-machine-config-bootstrapper/ .
RUN make build

# Build hybrid-overlay
WORKDIR /build/ovn-kubernetes/
COPY ovn-kubernetes/ .
WORKDIR /build/ovn-kubernetes/go-controller/
RUN make windows

# Build Kubernetes node binaries
WORKDIR /build/kubernetes/
COPY kubernetes/ .
ENV KUBE_BUILD_PLATFORMS windows/amd64
RUN make WHAT=cmd/kubelet
RUN make WHAT=cmd/kube-proxy

# Build CNI plugins
WORKDIR /build/containernetworking-plugins/
COPY containernetworking-plugins/ .
ENV CGO_ENABLED=0
RUN ./build_windows.sh

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
#└── wmcb.exe

FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
LABEL stage=operator

# Copy wmcb.exe
RUN mkdir /payload/
WORKDIR /payload/
COPY --from=build /build/windows-machine-config-bootstrapper/wmcb.exe .

# Copy hybrid-overlay-node.exe
COPY --from=build /build/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe .

# Copy kubelet.exe and kube-proxy.exe
RUN mkdir /payload/kube-node/
WORKDIR /payload/kube-node/
COPY --from=build /build/kubernetes/_output/local/bin/windows/amd64/kubelet.exe .
COPY --from=build /build/kubernetes/_output/local/bin/windows/amd64/kube-proxy.exe .

# Copy CNI plugin binaries and CNI config template cni-conf-template.json
RUN mkdir /payload/cni/
WORKDIR /payload/cni/
COPY --from=build /build/containernetworking-plugins/bin/flannel.exe .
COPY --from=build /build/containernetworking-plugins/bin/host-local.exe .
COPY --from=build /build/containernetworking-plugins/bin/win-bridge.exe .
COPY --from=build /build/containernetworking-plugins/bin/win-overlay.exe .
COPY pkg/internal/cni-conf-template.json .

# Copy required powershell scripts
RUN mkdir /payload/powershell/
WORKDIR /payload/powershell/
COPY pkg/internal/wget-ignore-cert.ps1 .
COPY pkg/internal/hns.psm1 .

WORKDIR /

ENV OPERATOR=/usr/local/bin/windows-machine-config-operator \
    USER_UID=1001 \
    USER_NAME=windows-machine-config-operator

# install operator binary
COPY build/_output/bin/windows-machine-config-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
