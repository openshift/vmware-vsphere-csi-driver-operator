package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/cnsmigration"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	klog "k8s.io/klog/v2"
)

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	// Some other error occurred, possibly indicating a problem
	return false
}

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	// TODO: Do we need to accept datastore URL or just a name?
	// Using a datastoreURL will guarantee unique name whereas a datastore name can be
	// not-unique across datacenters.
	destinationDatastore := flag.String("destination", "", "name of destination datastore")
	sourceDatastore := flag.String("source", "", "name of source datastore")
	volumeFile := flag.String("volume-file", "", "file from which we can read list of PVs to migrate")

	klog.InitFlags(nil)
	flag.Parse()

	kubeConfigEnv := os.Getenv("KUBECONFIG")
	if kubeConfigEnv != "" && fileExists(kubeConfigEnv) {
		kubeconfig = &kubeConfigEnv
	}

	if destinationDatastore == nil || *destinationDatastore == "" {
		klog.Fatalf("Specify destination datastore")
	}

	if sourceDatastore == nil || *sourceDatastore == "" {
		klog.Fatalf("Specify source datastore")
	}

	fmt.Printf("KubeConfig is: %s\n", *kubeconfig)

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		klog.Fatalf("error building kubeconfig: %v", err)
	}

	migrator := cnsmigration.NewCNSVolumeMigrator(config, *sourceDatastore, *destinationDatastore)

	migrator.StartMigration(context.TODO(), *volumeFile)
	if err != nil {
		klog.Fatalf("error migration one or more volumes: %v", err)
	}
}
