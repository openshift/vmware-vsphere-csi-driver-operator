module github.com/openshift/vmware-vsphere-csi-driver-operator

go 1.16

require (
	github.com/blang/semver v3.5.1+incompatible
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/imdario/mergo v0.3.11 // indirect
	github.com/openshift/api v0.0.0-20210924154557-a4f696157341
	github.com/openshift/build-machinery-go v0.0.0-20210806203541-4ea9b6da3a37
	github.com/openshift/client-go v0.0.0-20210916133943-9acee1a0fb83
	github.com/openshift/library-go v0.0.0-20211202133247-7bf4a770d7d3
	github.com/prometheus/client_golang v1.11.0
	github.com/spf13/cobra v1.1.3
	github.com/spf13/pflag v1.0.5
	github.com/vmware/govmomi v0.23.1
	google.golang.org/appengine v1.6.6 // indirect
	gopkg.in/gcfg.v1 v1.2.3
	gopkg.in/warnings.v0 v0.1.2 // indirect
	k8s.io/api v0.22.3
	k8s.io/apiextensions-apiserver v0.22.3 // indirect
	k8s.io/apimachinery v0.22.3
	k8s.io/client-go v0.22.3
	k8s.io/component-base v0.22.3
	k8s.io/klog/v2 v2.10.0
	k8s.io/legacy-cloud-providers v0.20.4
)
