# build stage for building binaries
FROM golang:1.13.8 as build
LABEL stage=build

# Build WMCB
RUN mkdir /build/
WORKDIR /build/
RUN git clone https://github.com/openshift/windows-machine-config-bootstrapper.git
WORKDIR windows-machine-config-bootstrapper
RUN make build

# Build hybrid-overlay
WORKDIR /build/
RUN git clone https://github.com/openshift/ovn-kubernetes/
WORKDIR ovn-kubernetes/go-controller/
RUN make windows

# download stage for downloading packages
FROM registry.access.redhat.com/ubi8/ubi-minimal as download
LABEL stage=download
WORKDIR /download/
RUN microdnf -y install wget tar gzip
RUN microdnf -y update

# Download, checksum and extract the kubernetes node package
# We are tightly coupling the operator to the OpenShift version, so with every OpenShift release, we will update the
# kubernetes node version.
RUN wget https://dl.k8s.io/v1.19.0-rc.2/kubernetes-node-windows-amd64.tar.gz
RUN echo "61389f8c05c682102e3432a2f05f41b11d531124f61443429627f94ef6e970d44240d44d32aa467b814de0b54a17208b2d2696602ba5dd6d30f64db964900230 kubernetes-node-windows-amd64.tar.gz" > kubernetes-node-windows-amd64.tar.gz.sha512
RUN sha512sum -c kubernetes-node-windows-amd64.tar.gz.sha512
RUN tar -zxf kubernetes-node-windows-amd64.tar.gz

# Download, checksum and extract the CNI plugin package
RUN wget https://github.com/containernetworking/plugins/releases/download/v0.8.6/cni-plugins-windows-amd64-v0.8.6.tgz
RUN wget https://github.com/containernetworking/plugins/releases/download/v0.8.6/cni-plugins-windows-amd64-v0.8.6.tgz.sha512
RUN sha512sum -c cni-plugins-windows-amd64-v0.8.6.tgz.sha512
RUN mkdir /download/cni/
WORKDIR /download/cni/
RUN tar -zxf /download/cni-plugins-windows-amd64-v0.8.6.tgz

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
WORKDIR /payload/
COPY --from=build /build/windows-machine-config-bootstrapper/wmcb.exe .

# Copy hybrid-overlay-node.exe
COPY --from=build /build/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe .

# Copy kubelet.exe and kube-proxy.exe
WORKDIR /payload/kube-node/
COPY --from=download /download/kubernetes/node/bin/kubelet.exe .
COPY --from=download /download/kubernetes/node/bin/kube-proxy.exe .

# Copy CNI plugin binaries and CNI config template cni-conf-template.json
WORKDIR /payload/cni/
COPY --from=download /download/cni/flannel.exe .
COPY --from=download /download/cni/host-local.exe .
COPY --from=download /download/cni/win-bridge.exe .
COPY --from=download /download/cni/win-overlay.exe .
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
