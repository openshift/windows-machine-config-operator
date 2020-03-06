module github.com/openshift/windows-machine-config-operator

go 1.13

replace github.com/docker/docker => github.com/moby/moby v0.7.3-0.20190826074503-38ab9da00309 // Required by Helm

replace (
	github.com/coreos/prometheus-operator => github.com/coreos/prometheus-operator v0.34.0
	github.com/openshift/api => github.com/openshift/api v0.0.0-20200205145930-e9d93e317dd1 // OpenShift 4.3
	github.com/openshift/client-go => github.com/openshift/client-go v0.0.0-20191125132246-f6563a70e19a // OpenShift 4.3
	github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer => github.com/ravisantoshgudimetla/windows-machine-config-operator/tools/windows-node-installer v0.0.0-20200227194518-855d8c68bfd9
	k8s.io/api => k8s.io/api v0.16.7
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.16.7
	k8s.io/apimachinery => k8s.io/apimachinery v0.16.7
	k8s.io/apiserver => k8s.io/apiserver v0.16.7
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.16.7
	k8s.io/client-go => k8s.io/client-go v0.16.7
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.16.7
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.16.7
	k8s.io/code-generator => k8s.io/code-generator v0.16.7
	k8s.io/component-base => k8s.io/component-base v0.16.7
	k8s.io/cri-api => k8s.io/cri-api v0.16.7
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.16.7
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.16.7
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.16.7
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.16.7
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.16.7
	k8s.io/kubectl => k8s.io/kubectl v0.16.7
	k8s.io/kubelet => k8s.io/kubelet v0.16.7
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.16.7
	k8s.io/metrics => k8s.io/metrics v0.16.7
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.16.7
)

require (
	github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer v0.0.0-20200225013401-cc0efc3f4ecf
	github.com/operator-framework/operator-sdk v0.15.2
	github.com/pkg/errors v0.8.1
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.4.0
	k8s.io/api v0.17.2
	k8s.io/apimachinery v0.17.3
	k8s.io/client-go v12.0.0+incompatible
	sigs.k8s.io/controller-runtime v0.5.0
)
