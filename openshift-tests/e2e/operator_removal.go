package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"

	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	clusteroperatorhelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	removalTestClient     = "vsphere-operator-removal-e2e"
	disabledConditionName = "VMwareVSphereControllerDisabled"

	csiDriverNameSpace          = "openshift-cluster-csi-drivers"
	csiControllerDeploymentName = "vmware-vsphere-csi-driver-controller"
	csiNodeDaemonSetName        = "vmware-vsphere-csi-driver-node"

	waitPollInterval = 1 * time.Second
	waitPollTimeout  = 10 * time.Minute
)

type disableConditionStatus int

const (
	operatorDisabled = iota
	operatorEnabled
	clusterCSIDriverNotFound
)

var _ = Describe("[sig-storage][platform:vsphere] vSphere CSI Driver Operator Removal", Label("vSphere", "Conformance"), func() {
	var (
		kubeClient     *kubernetes.Clientset
		configClient   *configclient.Clientset
		operatorClient *operatorclient.Clientset
		ctx            context.Context
		cancel         context.CancelFunc
	)

	BeforeEach(func() {
		var err error
		ctx, cancel, kubeClient, configClient, operatorClient, err = NewE2EClientsFromDefaultKubeconfig(removalTestClient, testContextTimeout)
		Expect(err).NotTo(HaveOccurred(), "Failed to get kubeconfig")
	})

	AfterEach(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("should successfully remove and restore storage resources [Suite:openshift/conformance/serial]", Label("Serial"), ginkgo.Informing(), func() {
		By("Waiting for storage to be available")
		waitForStorageAvailable(ctx, configClient, operatorClient, kubeClient)

		// Ensure we restore storage even if the test fails
		DeferCleanup(func() {
			By("Restoring storage operator to cluster")
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), testContextTimeout)
			defer restoreCancel()
			restoreStorage(restoreCtx, operatorClient, configClient, kubeClient)
		})

		By("Marking ClusterCSIDriver as removed")
		makeClusterCSIDriverRemoved(ctx, operatorClient)
		GinkgoWriter.Printf("✓ ClusterCSIDriver marked as removed\n")

		By("Waiting for storage resources to be removed")
		waitForStorageResourceRemoval(ctx, operatorClient, kubeClient)
		time.Sleep(10 * time.Second)

		// restoreStorage() is called in DeferCleanup
	})
})

// Helper function to check the disabled condition status
func checkDisabledCondition(ctx context.Context, operatorClient *operatorclient.Clientset) (disableConditionStatus, error) {
	clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Printf("ClusterCSIDriver does not yet exist\n")
		return clusterCSIDriverNotFound, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve ClusterCSIDriver: %v\n", err)
		return clusterCSIDriverNotFound, err
	}
	disabledCondition := v1helpers.FindOperatorCondition(clusterCSIDriver.Status.Conditions, disabledConditionName)
	if disabledCondition != nil && disabledCondition.Status == operatorapi.ConditionTrue {
		return operatorDisabled, nil
	}
	return operatorEnabled, nil
}

// Helper function to check if deployment exists
func checkForDeploymentCreation(ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	deployment, err := kubeClient.AppsV1().Deployments(csiDriverNameSpace).Get(ctx, csiControllerDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Printf("Deployment does not exist\n")
		return false, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve deployment: %v\n", err)
		return false, err
	}
	return deployment != nil, nil
}

// Helper function to check if daemonset exists
func checkForDaemonset(ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	daemonset, err := kubeClient.AppsV1().DaemonSets(csiDriverNameSpace).Get(ctx, csiNodeDaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		GinkgoWriter.Printf("DaemonSet does not exist\n")
		return false, nil
	}
	if err != nil {
		GinkgoWriter.Printf("Unable to retrieve daemonset: %v\n", err)
		return false, err
	}
	return daemonset != nil, nil
}

// Helper function to update ClusterCSIDriver management state to Removed
func makeClusterCSIDriverRemoved(ctx context.Context, operatorClient *operatorclient.Clientset) {
	Eventually(func() error {
		clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if clusterCSIDriver.Spec.ManagementState == operatorapi.Removed {
			return nil
		}
		clusterCSIDriver.Spec.ManagementState = operatorapi.Removed
		_, err = operatorClient.OperatorV1().ClusterCSIDrivers().Update(ctx, clusterCSIDriver, metav1.UpdateOptions{})
		return err
	}, testContextTimeout, waitPollInterval).Should(Succeed(), "Failed to set ClusterCSIDriver to Removed state")
}

// Helper function to update ClusterCSIDriver management state to Managed
func makeClusterCSIDriverManaged(ctx context.Context, operatorClient *operatorclient.Clientset) {
	Eventually(func() error {
		clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if clusterCSIDriver.Spec.ManagementState == operatorapi.Managed {
			return nil
		}
		clusterCSIDriver.Spec.ManagementState = operatorapi.Managed
		_, err = operatorClient.OperatorV1().ClusterCSIDrivers().Update(ctx, clusterCSIDriver, metav1.UpdateOptions{})
		return err
	}, testContextTimeout, waitPollInterval).Should(Succeed(), "Failed to set ClusterCSIDriver to Managed state")
}

// Helper function to wait for storage resources to be removed
func waitForStorageResourceRemoval(ctx context.Context, operatorClient *operatorclient.Clientset, kubeClient *kubernetes.Clientset) {
	GinkgoWriter.Printf("Waiting for storage resources to be removed\n")
	Eventually(func() bool {
		disabledConditionStatusVar, err := checkDisabledCondition(ctx, operatorClient)
		if err != nil {
			GinkgoWriter.Printf("Error checking disabled condition: %v\n", err)
			return false
		}
		if disabledConditionStatusVar != operatorDisabled {
			GinkgoWriter.Printf("Operator not yet disabled (status: %d)\n", disabledConditionStatusVar)
			return false
		}

		deploymentCreated, err := checkForDeploymentCreation(ctx, kubeClient)
		if err != nil {
			GinkgoWriter.Printf("Error checking deployment: %v\n", err)
			return false
		}
		if deploymentCreated {
			GinkgoWriter.Printf("Deployment still exists\n")
			return false
		}

		daemonsetCreated, err := checkForDaemonset(ctx, kubeClient)
		if err != nil {
			GinkgoWriter.Printf("Error checking daemonset: %v\n", err)
			return false
		}
		if daemonsetCreated {
			GinkgoWriter.Printf("DaemonSet still exists\n")
			return false
		}

		GinkgoWriter.Printf("✓ All storage resources removed\n")
		return true
	}, waitPollTimeout, waitPollInterval).Should(BeTrue(), "Storage resources should be removed")
}

// Helper function to wait for storage to be available
func waitForStorageAvailable(ctx context.Context, configClient *configclient.Clientset, operatorClient *operatorclient.Clientset, kubeClient *kubernetes.Clientset) {

	Eventually(func() bool {
		clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, storageCOName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			GinkgoWriter.Printf("ClusterOperator/storage does not yet exist\n")
			return false
		}
		if err != nil {
			GinkgoWriter.Printf("Unable to retrieve ClusterOperator/storage: %v\n", err)
			return false
		}

		conditions := clusterOperator.Status.Conditions
		available := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorAvailable, configv1.ConditionTrue)
		notProgressing := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorProgressing, configv1.ConditionFalse)
		notDegraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionFalse)
		done := available && notProgressing && notDegraded

		if done {
			disableConditionStatusVar, err := checkDisabledCondition(ctx, operatorClient)
			if err != nil {
				GinkgoWriter.Printf("Error checking disabled condition: %v\n", err)
				return false
			}
			done = disableConditionStatusVar == operatorEnabled
		}

		if done {
			deploymentCreated, err := checkForDeploymentCreation(ctx, kubeClient)
			if err != nil {
				GinkgoWriter.Printf("Error checking deployment: %v\n", err)
				return false
			}
			done = deploymentCreated
		}

		if done {
			daemonsetCreated, err := checkForDaemonset(ctx, kubeClient)
			if err != nil {
				GinkgoWriter.Printf("Error checking daemonset: %v\n", err)
				return false
			}
			done = daemonsetCreated
		}
		GinkgoWriter.Printf("ClusterOperator/storage: Available: %v  Progressing: %v  Degraded: %v\n", available, !notProgressing, !notDegraded)

		return done
	}, waitPollTimeout, waitPollInterval).Should(BeTrue(), "Storage should be available")
}

// Helper function to restore storage operator
func restoreStorage(ctx context.Context, operatorClient *operatorclient.Clientset, configClient *configclient.Clientset, kubeClient *kubernetes.Clientset) {
	GinkgoWriter.Printf("Restoring storage operator to cluster\n")
	makeClusterCSIDriverManaged(ctx, operatorClient)
	waitForStorageAvailable(ctx, configClient, operatorClient, kubeClient)
}
