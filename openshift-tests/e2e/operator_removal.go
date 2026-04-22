package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	clusteroperatorhelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	clientName        = "vsphere-operator-e2e"
	storageCOName     = "storage"
	csiDriverName     = "csi.vsphere.vmware.com"
	disabledCondition = "VMwareVSphereControllerDisabled"

	csiDriverNameSpace          = "openshift-cluster-csi-drivers"
	csiControllerDeploymentName = "vmware-vsphere-csi-driver-controller"
	csiNodeDaemonSetName        = "vmware-vsphere-csi-driver-node"
)

type disableConditionStatus int

const (
	operatorDisabled = iota
	operatorEnabled
	clusterCSIDriverNotFound
)

var (
	WaitPollInterval = time.Second
	WaitPollTimeout  = 10 * time.Minute
)

//	TestStorageRemoval tests the removal of the storage operator from the cluster
//
// by marking the ClusterCSIDriver as removed and waiting for the storage resources to be removed.
// The test does not verify removal of each storage resource but only deployment and daemonset.
// It also verifies if storage can be restored by marking managmentState as Managed
var _ = Describe("[sig-storage][platform:vsphere] vSphere Storage Operator Removal", Label("vSphere"), func() {
	var (
		kubeClient     *kubernetes.Clientset
		configClient   *configclient.Clientset
		operatorClient *operatorclient.Clientset
		ctx            context.Context
		cancel         context.CancelFunc
	)

	BeforeEach(func() {
		// Create Kubernetes and OpenShift clients
		clients, err := NewClients(clientName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clients")

		kubeClient = clients.KubeClient
		configClient = clients.ConfigClient
		operatorClient = clients.OperatorClient

		ctx, cancel = context.WithTimeout(context.Background(), WaitPollTimeout+5*time.Minute)
	})

	AfterEach(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("should remove and restore storage operator resources [Serial][Disruptive][Suite:openshift/conformance/serial]", Label("Serial"), func() {
		By("Waiting for storage to be available")
		waitForStorageAvailable(ctx, configClient, operatorClient, kubeClient)
		GinkgoWriter.Printf("✓ Storage operator is available\n")

		originalClusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterCSIDriver")
		originalManagementState := originalClusterCSIDriver.Spec.ManagementState

		defer func() {
			By("Restoring storage operator to cluster")
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), WaitPollTimeout+5*time.Minute)
			defer restoreCancel()

			restoreStorage(restoreCtx, operatorClient, configClient, kubeClient)
			GinkgoWriter.Printf("✓ Storage operator restored to %s\n", originalManagementState)
		}()

		By("Marking ClusterCSIDriver as removed")
		err = makeClusterCSIDriverRemoved(ctx, operatorClient)
		Expect(err).NotTo(HaveOccurred(), "Failed to update ClusterCSIDriver")
		GinkgoWriter.Printf("✓ ClusterCSIDriver set to Removed\n")

		By("Waiting for storage resources to be removed")
		err = waitForStorageResourceRemoval(ctx, operatorClient, kubeClient)
		Expect(err).NotTo(HaveOccurred(), "Failed to wait for storage resource removal")
		GinkgoWriter.Printf("✓ Storage resources have been removed\n")

		time.Sleep(10 * time.Second)

		By("Restoring storage operator to cluster")
		restoreStorage(ctx, operatorClient, configClient, kubeClient)
		GinkgoWriter.Printf("✓ Storage operator successfully restored\n")
	})
})

func restoreStorage(ctx context.Context, client *operatorclient.Clientset, configClient *configclient.Clientset, kubeClient *kubernetes.Clientset) {
	GinkgoWriter.Printf("Restoring storage operator to cluster\n")
	err := makeClusterCSIDriverManaged(ctx, client)
	Expect(err).NotTo(HaveOccurred(), "Failed to update ClusterCSIDriver")
	waitForStorageAvailable(ctx, configClient, client, kubeClient)
}

func waitForStorageResourceRemoval(ctx context.Context, client *operatorclient.Clientset, kubeClient *kubernetes.Clientset) error {
	// make sure storage to be removed
	GinkgoWriter.Printf("Waiting for storage resource to be removed\n")
	return wait.PollUntilContextTimeout(ctx, WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		disabledConditionStatusVar, err := checkDisabledCondition(pollContext, client)
		if err != nil {
			return false, err
		}
		done := disabledConditionStatusVar == operatorDisabled
		if done {
			deploymentCreated, err := checkForDeploymentCreation(pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = !deploymentCreated
		}

		if done {
			daemonsetCreated, err := checkForDaemonset(pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = !daemonsetCreated
		}
		return done, nil
	})
}

func waitForStorageAvailable(ctx context.Context, client *configclient.Clientset, operatorClient *operatorclient.Clientset, kubeClient *kubernetes.Clientset) {
	err := wait.PollUntilContextTimeout(ctx, WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		clusterOperator, err := client.ConfigV1().ClusterOperators().Get(pollContext, storageCOName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			GinkgoWriter.Printf("ClusterOperator/storage does not yet exist.\n")
			return false, nil
		}
		if err != nil {
			GinkgoWriter.Printf("Unable to retrieve ClusterOperator/storage: %v\n", err)
			return false, err
		}
		conditions := clusterOperator.Status.Conditions
		available := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorAvailable, configv1.ConditionTrue)
		notProgressing := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorProgressing, configv1.ConditionFalse)
		notDegraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionFalse)
		done := available && notProgressing && notDegraded

		if done {
			disableConditionStatusVar, err := checkDisabledCondition(pollContext, operatorClient)
			if err != nil {
				return false, err
			}
			done = disableConditionStatusVar == operatorEnabled
		}

		if done {
			deploymentCreated, err := checkForDeploymentCreation(pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = deploymentCreated
		}

		if done {
			daemonsetCreated, err := checkForDaemonset(pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = daemonsetCreated
		}

		GinkgoWriter.Printf("ClusterOperator/storage: Available: %v  Progressing: %v  Degraded: %v\n", available, !notProgressing, !notDegraded)
		return done, nil
	})
	Expect(err).NotTo(HaveOccurred(), "Storage operator should become available")
}

func checkDisabledCondition(ctx context.Context, operatorClient *operatorclient.Clientset) (disableConditionStatus, error) {
	clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Println("cluster-csi-driver does not yet exist.")
		return clusterCSIDriverNotFound, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve cluster-csi-driver: %v", err)
		return clusterCSIDriverNotFound, err
	}
	disabledCond := v1helpers.FindOperatorCondition(clusterCSIDriver.Status.Conditions, disabledCondition)
	if disabledCond != nil && disabledCond.Status == operatorapi.ConditionTrue {
		return operatorDisabled, nil
	}
	return operatorEnabled, nil
}

func makeClusterCSIDriverRemoved(ctx context.Context, client *operatorclient.Clientset) error {
	return wait.PollUntilContextTimeout(ctx, WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		clusterCSIDriver, err := client.OperatorV1().ClusterCSIDrivers().Get(pollContext, csiDriverName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if clusterCSIDriver.Spec.ManagementState == operatorapi.Removed {
			return true, nil
		}

		clusterCSIDriver.Spec.ManagementState = operatorapi.Removed
		_, err = client.OperatorV1().ClusterCSIDrivers().Update(pollContext, clusterCSIDriver, metav1.UpdateOptions{})
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func makeClusterCSIDriverManaged(ctx context.Context, client *operatorclient.Clientset) error {
	return wait.PollUntilContextTimeout(ctx, WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		clusterCSIDriver, err := client.OperatorV1().ClusterCSIDrivers().Get(pollContext, csiDriverName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if clusterCSIDriver.Spec.ManagementState == operatorapi.Managed {
			return true, nil
		}

		clusterCSIDriver.Spec.ManagementState = operatorapi.Managed
		_, err = client.OperatorV1().ClusterCSIDrivers().Update(pollContext, clusterCSIDriver, metav1.UpdateOptions{})
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func checkForDeploymentCreation(ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	deployment, err := kubeClient.AppsV1().Deployments(csiDriverNameSpace).Get(ctx, csiControllerDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Println("Deployment does not yet exist.")
		return false, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve deployment: %v", err)
		return false, err
	}
	return deployment != nil, nil
}

func checkForDaemonset(ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	daemonset, err := kubeClient.AppsV1().DaemonSets(csiDriverNameSpace).Get(ctx, csiNodeDaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Println("Daemonset does not yet exist.")
		return false, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve daemonset: %v", err)
		return false, err
	}
	return daemonset != nil, nil
}
