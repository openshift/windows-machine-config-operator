# build stage for building binaries
FROM golang:1.13.8 as build
LABEL stage=build

# Create cache directory as user running the tests doesn't have the permissions to create it at runtime. This is required
# for the go builds below to succeed
RUN mkdir /go/.cache
ENV GOCACHE="/go/.cache"

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
RUN wget https://dl.k8s.io/v1.17.3/kubernetes-node-windows-amd64.tar.gz
RUN echo "7323b7adf83fccc65eea9f23370794aa9901e9a9b3b1ac90403197448408eee5be84f541aa2448ceaa12fe6278814575a26132cf6e0ddd2f8aa5fa47bd127c71 kubernetes-node-windows-amd64.tar.gz" > kubernetes-node-windows-amd64.tar.gz.sha512
RUN sha512sum -c kubernetes-node-windows-amd64.tar.gz.sha512
RUN tar -zxf kubernetes-node-windows-amd64.tar.gz

# Download, checksum and extract the CNI plugin package
RUN wget https://github.com/containernetworking/plugins/releases/download/v0.8.2/cni-plugins-windows-amd64-v0.8.2.tgz
RUN echo "705a760673fd9e2164ac38f0df7d739ca6c3ec4f4204b0c439227ec6da7cb153859013c917e7f8f1a9456365dd9193f627a7e9e4e1981725cab89bb5ab881ec0 cni-plugins-windows-amd64-v0.8.2.tgz" > cni-plugins-windows-amd64-v0.8.2.tgz.sha512
RUN sha512sum -c cni-plugins-windows-amd64-v0.8.2.tgz.sha512
RUN mkdir /download/cni-plugins/
WORKDIR /download/cni-plugins/
RUN tar -zxf /download/cni-plugins-windows-amd64-v0.8.2.tgz

# Build the operator image with following payload structure
# /payload/
#├── cni-plugins
#│   ├── flannel.exe
#│   ├── host-local.exe
#│   ├── win-bridge.exe
#│   └── win-overlay.exe
#├── hybrid-overlay-node.exe
#├── kube-node
#│   ├── kubelet.exe
#│   └── kube-proxy.exe
#├── powershell
#│   └── wget-ignore-cert.ps1
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

# Copy CNI plugin binaries
WORKDIR /payload/cni-plugins/
COPY --from=download /download/cni-plugins/flannel.exe .
COPY --from=download /download/cni-plugins/host-local.exe .
COPY --from=download /download/cni-plugins/win-bridge.exe .
COPY --from=download /download/cni-plugins/win-overlay.exe .

# Copy wget-ignore-cert powershell script
RUN mkdir /payload/powershell/
WORKDIR /payload/powershell/
COPY pkg/internal/wget-ignore-cert.ps1 .

WORKDIR /

ENV OPERATOR=/usr/local/bin/windows-machine-config-operator \
    USER_UID=1001 \
    USER_NAME=windows-machine-config-operator

# install operator binary
COPY build/_output/bin/windows-machine-config-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

# Copy the wget-ignore-powershell script needed to execute wget commands remotely
COPY pkg/internal/wget-ignore-cert.ps1 /payload/powershell/

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
