FROM brew.registry.redhat.io/rh-osbs/openshift-golang-builder:rhel_9_1.22 as image-replacer
COPY bundle/manifests /manifests
RUN sed -i "s|REPLACE_IMAGE|registry.redhat.io/openshift4-wincw/windows-machine-config-rhel9-operator@sha256:05223a792a351ac5d6e610544507c8ce59356c392c646ef79d4cfc24d1f04e52|g" /manifests/windows-machine-config-operator.clusterserviceversion.yaml

FROM scratch

# This block contains standard Red Hat container labels
LABEL name="openshift4-wincw/windows-machine-config-operator-bundle" \
    License="ASL 2.0" \
    io.k8s.display-name="Windows Machine Config Operator bundle" \
    io.k8s.description="Windows Machine Config Operator's OLM bundle image" \
	description="Windows Machine Config Operator's OLM bundle image" \
    summary="Windows Machine Config Operator's OLM bundle image" \
    maintainer="Team Windows Containers <team-winc@redhat.com>" \
    io.openshift.tags=""

# These are three labels needed to control how the pipeline should handle this container image
# This first label tells the pipeline that this is a bundle image and should be
# delivered via an index image
LABEL com.redhat.delivery.operator.bundle=true

# This second label tells the pipeline which versions of OpenShift the operator supports.
# This is used to control which index images should include this operator.
LABEL com.redhat.openshift.versions="=v4.19"

# This third label tells the pipeline that this operator should *also* be supported on OCP 4.4 and
# earlier.  It is used to control whether or not the pipeline should attempt to automatically
# backport this content into the old appregistry format and upload it to the quay.io application
# registry endpoints.
LABEL com.redhat.delivery.backport=false

LABEL version="v10.19.0"

# This label maps to the brew build target
LABEL com.redhat.component="windows-machine-config-operator-bundle-container"

# Core bundle labels.
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=windows-machine-config-operator
LABEL operators.operatorframework.io.bundle.channels.v1=preview,stable
LABEL operators.operatorframework.io.bundle.channel.default.v1=stable
LABEL operators.operatorframework.io.metrics.builder=operator-sdk-v1.32.0
LABEL operators.operatorframework.io.metrics.mediatype.v1=metrics+v1
LABEL operators.operatorframework.io.metrics.project_layout=go.kubebuilder.io/v3

LABEL distribution-scope=public
LABEL release="10.19.0"
LABEL url="https://docs.openshift.com/container-platform/4.17/windows_containers/index.html"
LABEL vendor="Red Hat, Inc."

# Copy files to locations specified by labels.
COPY --from=image-replacer /manifests /manifests/
COPY bundle/metadata /metadata/
