module github.com/openshift/vmware-vsphere-csi-driver-operator

go 1.16

require (
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/go-logr/logr v0.2.1 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/imdario/mergo v0.3.11 // indirect
	github.com/onsi/gomega v1.10.1 // indirect
	github.com/openshift/api v0.0.0-20210315130343-ff0ef568c9ca
	github.com/openshift/build-machinery-go v0.0.0-20200917070002-f171684f77ab
	github.com/openshift/client-go v0.0.0-20201214125552-e615e336eb49
	github.com/openshift/library-go v0.0.0-20210113192829-cfbb3f4c80c2
	github.com/prometheus/client_golang v1.8.0
	github.com/spf13/cobra v1.1.1
	github.com/spf13/pflag v1.0.5
	github.com/vmware/govmomi v0.23.1
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.16.0 // indirect
	golang.org/x/tools v0.0.0-20200825202427-b303f430e36d // indirect
	google.golang.org/appengine v1.6.6 // indirect
	google.golang.org/genproto v0.0.0-20201112120144-2985b7af83de // indirect
	gopkg.in/gcfg.v1 v1.2.3
	honnef.co/go/tools v0.0.1-2020.1.4 // indirect
	k8s.io/api v0.20.4
	k8s.io/apimachinery v0.20.4
	k8s.io/client-go v0.20.4
	k8s.io/component-base v0.20.4
	k8s.io/klog/v2 v2.4.0
	k8s.io/legacy-cloud-providers v0.20.4
)
