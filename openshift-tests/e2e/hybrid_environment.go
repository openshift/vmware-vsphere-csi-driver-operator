package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"

	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	clusteroperatorhelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	vSpherePlatformType  = "vsphere"
	platformTypeLabelKey = "node.openshift.io/platform-type"
	hybridTestClient     = "vsphere-hybrid-e2e"

	// Timeout constants
	testContextTimeout                 = 10 * time.Minute
	restoreContextTimeout              = 15 * time.Minute
	clusterCSIDriverUpdateTimeout      = 2 * time.Minute
	clusterCSIDriverUpdatePollInterval = 2 * time.Second
	operatorHealthTimeout              = 10 * time.Minute
	operatorHealthPollInterval         = 5 * time.Second
	operatorDegradationTimeout         = 5 * time.Minute
)

// Helper function to categorize nodes by platform type
func categorizeNodesByPlatform(ctx context.Context, kubeClient *kubernetes.Clientset, verbose bool) (nodesWithVSphere, nodesWithoutLabel []string, err error) {
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}

	if len(nodes.Items) == 0 {
		return nil, nil, fmt.Errorf("no nodes found in the cluster")
	}

	if verbose {
		GinkgoWriter.Printf("Found %d nodes in the cluster\n", len(nodes.Items))
	}

	for _, node := range nodes.Items {
		platformType, hasLabel := node.Labels[platformTypeLabelKey]

		if hasLabel && platformType == vSpherePlatformType {
			nodesWithVSphere = append(nodesWithVSphere, node.Name)
			if verbose {
				GinkgoWriter.Printf("✓ Node %s has vSphere platform-type label\n", node.Name)
			}
		} else {
			nodesWithoutLabel = append(nodesWithoutLabel, node.Name)
			if verbose {
				if !hasLabel {
					GinkgoWriter.Printf("⚠ Node %s is missing the %s label (assuming bare metal node)\n",
						node.Name, platformTypeLabelKey)
				} else {
					GinkgoWriter.Printf("⚠ Node %s has platform-type=%s (non-vSphere)\n", node.Name, platformType)
				}
			}
		}
	}

	return nodesWithVSphere, nodesWithoutLabel, nil
}

// Helper function to check if environment is hybrid
func isHybridEnvironment(nodesWithVSphere, nodesWithoutLabel []string) bool {
	return len(nodesWithVSphere) > 0 && len(nodesWithoutLabel) > 0
}

// Helper function to verify ClusterCSIDriver management state
func verifyClusterCSIDriverState(ctx context.Context, operatorClient *operatorclient.Clientset, expectedState operatorapi.ManagementState, description string) {
	clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterCSIDriver")

	managementState := clusterCSIDriver.Spec.ManagementState
	GinkgoWriter.Printf("ClusterCSIDriver %s has managementState: %s\n", csiDriverName, managementState)

	Expect(managementState).To(Equal(expectedState), description)
	GinkgoWriter.Printf("✓ ClusterCSIDriver is correctly set to %s\n", expectedState)
}

// Helper function to update ClusterCSIDriver management state
func updateClusterCSIDriverState(ctx context.Context, operatorClient *operatorclient.Clientset, targetState operatorapi.ManagementState) {
	Eventually(func() error {
		clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if clusterCSIDriver.Spec.ManagementState == targetState {
			return nil
		}
		clusterCSIDriver.Spec.ManagementState = targetState
		_, err = operatorClient.OperatorV1().ClusterCSIDrivers().Update(ctx, clusterCSIDriver, metav1.UpdateOptions{})
		return err
	}, clusterCSIDriverUpdateTimeout, clusterCSIDriverUpdatePollInterval).Should(Succeed())
}

// Helper function to wait for operator health
func waitForOperatorHealth(ctx context.Context, configClient *configclient.Clientset) {
	Eventually(func() bool {
		clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, storageCOName, metav1.GetOptions{})
		if err != nil {
			GinkgoWriter.Printf("Failed to get ClusterOperator during health check: %v\n", err)
			return false
		}

		conditions := clusterOperator.Status.Conditions
		available := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorAvailable, configv1.ConditionTrue)
		progressing := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorProgressing, configv1.ConditionTrue)
		degraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionTrue)

		isHealthy := available && !progressing && !degraded
		if !isHealthy {
			GinkgoWriter.Printf("Waiting for healthy state - Available: %v, Progressing: %v, Degraded: %v\n",
				available, progressing, degraded)
		}

		return isHealthy
	}, operatorHealthTimeout, operatorHealthPollInterval).Should(BeTrue(), "Storage operator should be healthy")
}

// [OCPFeatureGate:VSphereMixedNodeEnv]
var _ = Describe("[sig-storage][OCPFeatureGate:VSphereMixedNodeEnv][platform:vsphere] vSphere Hybrid Environment", Label("vSphere", "Conformance"), func() {
	var (
		kubeClient     *kubernetes.Clientset
		configClient   *configclient.Clientset
		operatorClient *operatorclient.Clientset
		ctx            context.Context
		cancel         context.CancelFunc
	)

	BeforeEach(func() {
		// Create Kubernetes and OpenShift clients
		clients, err := NewClients(hybridTestClient)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clients")

		kubeClient = clients.KubeClient
		configClient = clients.ConfigClient
		operatorClient = clients.OperatorClient

		// Create context with timeout
		ctx, cancel = context.WithTimeout(context.Background(), testContextTimeout)
	})

	AfterEach(func() {
		if cancel != nil {
			cancel()
		}
	})

	Context("when VSphereMixedNodeEnv feature gate is enabled", func() {
		It("should validate node labels and verify appropriate operator state [Suite:openshift/conformance/parallel]", ginkgo.Informing(), func() {
			By("Checking node platform types")
			nodesWithVSphereLabel, nodesWithoutLabel, err := categorizeNodesByPlatform(ctx, kubeClient, true)
			Expect(err).NotTo(HaveOccurred(), "Failed to categorize nodes by platform")

			totalNodes := len(nodesWithVSphereLabel) + len(nodesWithoutLabel)
			allNodesAreVSphere := len(nodesWithoutLabel) == 0 && len(nodesWithVSphereLabel) > 0
			hybridEnv := isHybridEnvironment(nodesWithVSphereLabel, nodesWithoutLabel)

			if allNodesAreVSphere {
				GinkgoWriter.Printf("✓ All %d nodes have vSphere platform-type label\n", totalNodes)

				By("Verifying ClusterCSIDriver is set to Managed (all nodes are vSphere)")
				verifyClusterCSIDriverState(ctx, operatorClient, operatorapi.Managed,
					"ClusterCSIDriver should have managementState=Managed when all nodes are vSphere")

			} else if hybridEnv {
				GinkgoWriter.Printf("⚠ Hybrid environment detected: %d vSphere nodes, %d bare metal nodes\n",
					len(nodesWithVSphereLabel), len(nodesWithoutLabel))

				By("Verifying ClusterCSIDriver is set to Removed (hybrid environment with bare metal nodes)")
				verifyClusterCSIDriverState(ctx, operatorClient, operatorapi.Removed,
					"ClusterCSIDriver should have managementState=Removed in hybrid environment with bare metal nodes")

			} else {
				// Neither all vSphere nor hybrid - this is a failure on a vSphere platform
				Fail(fmt.Sprintf("Test labeled for vSphere platform but found %d vSphere nodes and %d unlabeled nodes - nodes should have %s label when VSphereMixedNodeEnv feature gate is enabled",
					len(nodesWithVSphereLabel), len(nodesWithoutLabel), platformTypeLabelKey))
			}
		})

		It("should degrade when ClusterCSIDriver is set to Managed in hybrid environment [Serial][Disruptive][Suite:openshift/conformance/serial]", Label("Serial"), ginkgo.Informing(), func() {
			By("Checking if this is a hybrid environment")
			nodesWithVSphereLabel, nodesWithoutLabel, err := categorizeNodesByPlatform(ctx, kubeClient, false)
			Expect(err).NotTo(HaveOccurred(), "Failed to categorize nodes by platform")

			hybridEnv := isHybridEnvironment(nodesWithVSphereLabel, nodesWithoutLabel)
			if !hybridEnv {
				Skip("Skipping test - not a hybrid environment (needs both vSphere and bare metal nodes)")
			}

			GinkgoWriter.Printf("✓ Hybrid environment confirmed: %d vSphere nodes, %d bare metal nodes\n",
				len(nodesWithVSphereLabel), len(nodesWithoutLabel))

			// Save the original management state
			originalClusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterCSIDriver")
			originalManagementState := originalClusterCSIDriver.Spec.ManagementState

			// Ensure we restore the original state
			defer func() {
				By(fmt.Sprintf("Restoring ClusterCSIDriver to original state: %s", originalManagementState))
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), restoreContextTimeout)
				defer restoreCancel()

				updateClusterCSIDriverState(restoreCtx, operatorClient, originalManagementState)
				GinkgoWriter.Printf("✓ ClusterCSIDriver restored to %s\n", originalManagementState)

				By("Waiting for storage operator to return to healthy state")
				waitForOperatorHealth(restoreCtx, configClient)
				GinkgoWriter.Printf("✓ Storage operator is healthy again\n")
			}()

			By("Setting ClusterCSIDriver to Managed (should cause degradation)")
			updateClusterCSIDriverState(ctx, operatorClient, operatorapi.Managed)
			GinkgoWriter.Printf("✓ ClusterCSIDriver set to Managed\n")

			By("Waiting for storage operator to report degraded or non-upgradeable state")
			Eventually(func() bool {
				clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, storageCOName, metav1.GetOptions{})
				if err != nil {
					GinkgoWriter.Printf("Failed to get ClusterOperator: %v\n", err)
					return false
				}

				conditions := clusterOperator.Status.Conditions
				degraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionTrue)
				notUpgradeable := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorUpgradeable, configv1.ConditionFalse)

				GinkgoWriter.Printf("Current state - Degraded: %v, Upgradeable: %v\n", degraded, !notUpgradeable)

				// We expect either degraded or not upgradeable in a hybrid environment
				return degraded || notUpgradeable
			}, operatorDegradationTimeout, operatorHealthPollInterval).Should(BeTrue(),
				"Storage operator should be degraded or non-upgradeable when managing a hybrid environment")

			// Verify the final state and check for expected error messages
			By("Verifying the operator reports appropriate error conditions")
			clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, storageCOName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterOperator")

			conditions := clusterOperator.Status.Conditions
			degraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionTrue)
			notUpgradeable := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorUpgradeable, configv1.ConditionFalse)

			Expect(degraded || notUpgradeable).To(BeTrue(),
				"Expected storage operator to be degraded or non-upgradeable in hybrid environment")

			// Check for condition message that indicates the issue
			for _, condition := range conditions {
				if (condition.Type == configv1.OperatorDegraded && condition.Status == configv1.ConditionTrue) ||
					(condition.Type == configv1.OperatorUpgradeable && condition.Status == configv1.ConditionFalse) {
					GinkgoWriter.Printf("Condition %s: %s - %s\n", condition.Type, condition.Reason, condition.Message)
				}
			}

			if degraded {
				GinkgoWriter.Printf("✓ Storage operator correctly reported as Degraded\n")
			}
			if notUpgradeable {
				GinkgoWriter.Printf("✓ Storage operator correctly reported as non-Upgradeable\n")
			}
		})
	})
})
