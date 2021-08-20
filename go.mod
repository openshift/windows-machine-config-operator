module github.com/openshift/windows-machine-config-operator

go 1.16

replace (
	sigs.k8s.io/cluster-api-provider-aws => github.com/openshift/cluster-api-provider-aws v0.2.1-0.20200520125206-5e266b553d8e // This is coming from machine-api repo
	sigs.k8s.io/cluster-api-provider-azure => github.com/openshift/cluster-api-provider-azure v0.1.0-alpha.3.0.20200902180535-72169c58a81f
)

require (
	github.com/aws/aws-sdk-go v1.27.0
	github.com/go-logr/logr v0.4.0
	github.com/openshift/api v0.0.0-20210521075222-e273a339932a
	github.com/openshift/client-go v0.0.0-20210521082421-73d9475a9142
	github.com/openshift/library-go v0.0.0-20210521084623-7392ea9b02ca
	github.com/openshift/machine-api-operator v0.2.1-0.20200722104429-f4f9b84df9b7
	github.com/operator-framework/api v0.10.7
	github.com/operator-framework/operator-lib v0.4.0
	github.com/pkg/errors v0.9.1
	github.com/pkg/sftp v1.11.0
	github.com/prometheus-operator/prometheus-operator/pkg/client v0.45.0
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.7.0
	golang.org/x/crypto v0.0.0-20210711020723-a769d52b0f97
	golang.org/x/mod v0.4.2
	k8s.io/api v0.22.1
	k8s.io/apimachinery v0.22.1
	k8s.io/client-go v0.22.1
	k8s.io/kubectl v0.21.1
	sigs.k8s.io/cluster-api-provider-aws v0.0.0-00010101000000-000000000000
	sigs.k8s.io/cluster-api-provider-azure v0.0.0-00010101000000-000000000000
	sigs.k8s.io/controller-runtime v0.10.0
)
