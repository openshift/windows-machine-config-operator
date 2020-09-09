module github.com/openshift/windows-machine-config-operator

go 1.14

replace github.com/docker/docker => github.com/moby/moby v0.7.3-0.20190826074503-38ab9da00309 // Required by Helm

replace (
	github.com/Azure/go-autorest => github.com/Azure/go-autorest v13.3.2+incompatible // Required by OLM
	github.com/coreos/prometheus-operator => github.com/coreos/prometheus-operator v0.38.1-0.20200424145508-7e176fda06cc
	github.com/go-logr/logr => github.com/go-logr/logr v0.2.1-0.20200730175230-ee2de8da5be6 // Compatibility issues - https://github.com/go-logr/logr/issues/19, bumping to latest operator-sdk might remove this
	github.com/go-logr/zapr => github.com/go-logr/zapr v0.2.0 // Same the logr compatibility issue
	github.com/mattn/go-sqlite3 => github.com/mattn/go-sqlite3 v1.10.0
	github.com/openshift/api => github.com/openshift/api v0.0.0-20200803131051-87466835fcc0 // OpenShift 4.6
	github.com/openshift/client-go => github.com/openshift/client-go v0.0.0-20200729195840-c2b1adc6bed6 // OpenShift 4.6
	k8s.io/api => k8s.io/api v0.18.2
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.18.2
	k8s.io/apimachinery => k8s.io/apimachinery v0.18.2
	k8s.io/apiserver => k8s.io/apiserver v0.18.2
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.18.2
	k8s.io/client-go => k8s.io/client-go v0.18.2
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.18.2
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.18.2
	k8s.io/code-generator => k8s.io/code-generator v0.18.2
	k8s.io/component-base => k8s.io/component-base v0.18.2
	k8s.io/cri-api => k8s.io/cri-api v0.18.2
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.18.2
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.18.2
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.18.2
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.18.2
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.18.2
	k8s.io/kubectl => k8s.io/kubectl v0.18.2
	k8s.io/kubelet => k8s.io/kubelet v0.18.2
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.18.2
	k8s.io/metrics => k8s.io/metrics v0.18.2
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.18.2
	sigs.k8s.io/cluster-api-provider-aws => github.com/openshift/cluster-api-provider-aws v0.2.1-0.20200520125206-5e266b553d8e // This is coming from machine-api repo
	sigs.k8s.io/cluster-api-provider-azure => github.com/openshift/cluster-api-provider-azure v0.1.0-alpha.3.0.20200902180535-72169c58a81f
)

require (
	github.com/aws/aws-sdk-go v1.25.48
	github.com/openshift/api v0.0.0-20200728200559-811027b63048
	github.com/openshift/client-go v0.0.0-20200422192633-6f6c07fc2a70
	github.com/openshift/machine-api-operator v0.2.1-0.20200722104429-f4f9b84df9b7
	github.com/operator-framework/operator-sdk v0.18.1
	github.com/pkg/errors v0.9.1
	github.com/pkg/sftp v1.11.0
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.5.1
	golang.org/x/crypto v0.0.0-20200622213623-75b288015ac9
	golang.org/x/sys v0.0.0-20200728102440-3e129f6d46b1 // indirect
	k8s.io/api v0.19.0-rc.2
	k8s.io/apimachinery v0.19.0-rc.2
	k8s.io/client-go v12.0.0+incompatible
	k8s.io/utils v0.0.0-20200729134348-d5654de09c73 // indirect
	sigs.k8s.io/cluster-api-provider-aws v0.0.0-00010101000000-000000000000
	sigs.k8s.io/cluster-api-provider-azure v0.0.0-00010101000000-000000000000
	sigs.k8s.io/controller-runtime v0.6.0
)
