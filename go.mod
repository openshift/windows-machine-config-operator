module github.com/openshift/windows-machine-config-operator

go 1.16

replace (
	// These are coming from the machine-api repo
	sigs.k8s.io/cluster-api-provider-aws => github.com/openshift/cluster-api-provider-aws v0.2.1-0.20210622023641-c69a3acaee27
	sigs.k8s.io/cluster-api-provider-azure => github.com/openshift/cluster-api-provider-azure v0.1.0-alpha.3.0.20210816141152-a7c40345b994
)

require (
	github.com/apparentlymart/go-cidr v1.1.0
	github.com/aws/aws-sdk-go v1.38.23
	github.com/go-logr/logr v1.2.0
	github.com/openshift/api v0.0.0-20210831091943-07e756545ac1
	github.com/openshift/client-go v0.0.0-20210831095141-e19a065e79f7
	github.com/openshift/library-go v0.0.0-20210811133500-5e31383de2a7
	github.com/openshift/machine-api-operator v0.2.1-0.20210820103535-d50698c302f5
	github.com/operator-framework/api v0.10.5
	github.com/operator-framework/operator-lib v0.4.0
	github.com/pkg/errors v0.9.1
	github.com/pkg/sftp v1.11.0
	github.com/prometheus-operator/prometheus-operator/pkg/client v0.45.0
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.7.0
	golang.org/x/crypto v0.0.0-20210817164053-32db794688a5
	golang.org/x/mod v0.4.2
	golang.org/x/sys v0.0.0-20211029165221-6e7872819dc8
	k8s.io/api v0.23.0
	k8s.io/apimachinery v0.23.0
	k8s.io/client-go v0.23.0
	k8s.io/kubectl v0.23.0
	sigs.k8s.io/cluster-api-provider-aws v0.0.0-00010101000000-000000000000
	sigs.k8s.io/cluster-api-provider-azure v0.0.0-00010101000000-000000000000
	sigs.k8s.io/controller-runtime v0.11.0
)
