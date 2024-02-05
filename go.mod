module github.com/openshift/windows-machine-config-operator

go 1.21

toolchain go1.21.5

require (
	github.com/apparentlymart/go-cidr v1.1.0
	github.com/aws/aws-sdk-go v1.44.298
	github.com/coreos/ignition/v2 v2.16.2
	github.com/go-imports-organizer/goio v1.3.3
	github.com/go-logr/logr v1.2.4
	github.com/openshift/api v0.0.0-20231118005202-0f638a8a4705
	github.com/openshift/client-go v0.0.0-20230120202327-72f107311084
	github.com/openshift/library-go v0.0.0-20231115094609-5e510a6e9a52
	github.com/openshift/machine-config-operator v0.0.1-0.20240202131832-4da1fdde4aa0
	github.com/operator-framework/api v0.16.0
	github.com/operator-framework/operator-lib v0.4.0
	github.com/operator-framework/operator-lifecycle-manager v0.22.0
	github.com/pkg/sftp v1.13.1
	github.com/prometheus-operator/prometheus-operator/pkg/client v0.58.0
	github.com/spf13/cobra v1.7.0
	github.com/spf13/pflag v1.0.6-0.20210604193023-d5e0c0615ace
	github.com/stretchr/testify v1.8.4
	github.com/vincent-petithory/dataurl v1.0.0
	go.uber.org/zap v1.24.0
	golang.org/x/crypto v0.14.0
	golang.org/x/mod v0.12.0
	golang.org/x/sys v0.13.0
	k8s.io/api v0.26.13
	k8s.io/apimachinery v0.26.13
	k8s.io/client-go v0.26.13
	k8s.io/cloud-provider v0.26.13
	k8s.io/klog/v2 v2.100.1
	k8s.io/kubectl v0.26.13
	k8s.io/kubelet v0.26.13
	sigs.k8s.io/controller-runtime v0.14.7
)

require (
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/MakeNowJust/heredoc v1.0.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver v3.5.1+incompatible // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/chai2010/gettext-go v1.0.2 // indirect
	github.com/coreos/go-json v0.0.0-20230131223807-18775e0fb4fb // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/coreos/vcontext v0.0.0-20230201181013-d72178a18687 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/emicklei/go-restful/v3 v3.11.2 // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.7.0 // indirect
	github.com/exponent-io/jsonpath v0.0.0-20151013193312-d6023ce2651d // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-errors/errors v1.0.1 // indirect
	github.com/go-logr/zapr v1.2.3 // indirect
	github.com/go-openapi/jsonpointer v0.20.2 // indirect
	github.com/go-openapi/jsonreference v0.20.4 // indirect
	github.com/go-openapi/swag v0.22.9 // indirect
	k8s.io/kube-openapi v0.0.0-20240126223410-2919ad4fcfec // indirect
	k8s.io/utils v0.0.0-20240102154912-e7106e64919e // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/kustomize/api v0.12.1 // indirect
	sigs.k8s.io/kustomize/kyaml v0.13.9 // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.3.0 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)
