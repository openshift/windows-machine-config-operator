FROM scratch

# This block are standard Red Hat container labels
LABEL name="openshift4/windows-machine-config-operator-bundle" \
    License="ASL 2.0" \
    io.k8s.display-name="Windows Machine Config Operator bundle" \
    io.k8s.description="Windows Machine Config Operator's OLM bundle image" \
    summary="Windows Machine Config Operator's OLM bundle image" \
    maintainer="Team Windows Containers <team-winc@redhat.com>"

# These are three labels needed to control how the pipeline should handle this container image
# This first label tells the pipeline that this is a bundle image and should be
# delivered via an index image
LABEL com.redhat.delivery.operator.bundle=true

# This second label tells the pipeline which versions of OpenShift the operator supports (4.6+).
# This is used to control which index images should include this operator.
LABEL com.redhat.openshift.versions="v4.6"

# This third label tells the pipeline that this operator should *also* be supported on OCP 4.4 and
# earlier.  It is used to control whether or not the pipeline should attempt to automatically
# backport this content into the old appregistry format and upload it to the quay.io application
# registry endpoints.
LABEL com.redhat.delivery.backport=false

# The rest of these labels are copies of the same content in annotations.yaml and are needed by OLM
# Note the package name and channels which are very important!
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1 \
    operators.operatorframework.io.bundle.manifests.v1=manifests/ \
    operators.operatorframework.io.bundle.metadata.v1=metadata/ \
    operators.operatorframework.io.bundle.package.v1=windows-machine-config-operator \
    operators.operatorframework.io.bundle.channels.v1=alpha \
    operators.operatorframework.io.bundle.channel.default.v1=alpha

# This label gets replaced by render_templates, so it is safer to keep it on its own
LABEL version="v0.0.0"

# This label maps to the brew build target
LABEL com.redhat.component="windows-machine-config-operator-bundle-container"

# COPY manifests and metadata directories
COPY deploy/olm-catalog/windows-machine-config-operator/manifests /manifests/
COPY deploy/olm-catalog/windows-machine-config-operator/metadata /metadata/
