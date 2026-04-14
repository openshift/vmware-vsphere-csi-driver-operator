package e2e

import (
	"context"
	"fmt"
	"github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	clusteroperatorhelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	vSpherePlatformType  = "vsphere"
	platformTypeLabelKey = "node.openshift.io/platform-type"
	hybridTestClient     = "vsphere-hybrid-e2e"
	storageCOName        = "storage"
	csiDriverName        = "csi.vsphere.vmware.com"

	// Timeout constants
	testContextTimeout                 = 10 * time.Minute
	restoreContextTimeout              = 15 * time.Minute
	clusterCSIDriverUpdateTimeout      = 2 * time.Minute
	clusterCSIDriverUpdatePollInterval = 2 * time.Second
	operatorHealthTimeout              = 10 * time.Minute
	operatorHealthPollInterval         = 5 * time.Second
	operatorDegradationTimeout         = 5 * time.Minute
)

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
		// Create Kubernetes client
		loader := clientcmd.NewDefaultClientConfigLoadingRules()
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loader,
			&clientcmd.ConfigOverrides{ClusterInfo: api.Cluster{InsecureSkipTLSVerify: true}},
		)
		config, err := clientConfig.ClientConfig()
		Expect(err).NotTo(HaveOccurred(), "Failed to get kubeconfig")

		kubeClient = kubernetes.NewForConfigOrDie(rest.AddUserAgent(config, hybridTestClient))
		configClient = configclient.NewForConfigOrDie(rest.AddUserAgent(config, hybridTestClient))
		operatorClient = operatorclient.NewForConfigOrDie(rest.AddUserAgent(config, hybridTestClient))

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
			By("Listing all nodes in the cluster")
			nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list nodes")
			Expect(nodes.Items).NotTo(BeEmpty(), "Expected to find at least one node in the cluster")

			GinkgoWriter.Printf("Found %d nodes in the cluster\n", len(nodes.Items))

			By("Checking each node for the vSphere platform-type label")
			var nodesWithoutLabel []string
			var nodesWithVSphereLabel []string

			for _, node := range nodes.Items {
				platformType, hasLabel := node.Labels[platformTypeLabelKey]

				if !hasLabel {
					nodesWithoutLabel = append(nodesWithoutLabel, node.Name)
					GinkgoWriter.Printf("⚠ Node %s is missing the %s label (assuming bare metal node)\n",
						node.Name, platformTypeLabelKey)
				} else if platformType == vSpherePlatformType {
					nodesWithVSphereLabel = append(nodesWithVSphereLabel, node.Name)
					GinkgoWriter.Printf("✓ Node %s has vSphere platform-type label\n", node.Name)
				} else {
					GinkgoWriter.Printf("⚠ Node %s has platform-type=%s\n", node.Name, platformType)
				}
			}

			allNodesAreVSphere := len(nodesWithoutLabel) == 0 && len(nodesWithVSphereLabel) == len(nodes.Items)
			isHybridEnvironment := len(nodesWithVSphereLabel) > 0 && len(nodesWithoutLabel) > 0

			if allNodesAreVSphere {
				GinkgoWriter.Printf("✓ All %d nodes have vSphere platform-type label\n", len(nodes.Items))

				By("Verifying storage operator is healthy (all nodes are vSphere)")
				clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, storageCOName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterOperator/storage")

				conditions := clusterOperator.Status.Conditions
				available := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorAvailable, configv1.ConditionTrue)
				progressing := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorProgressing, configv1.ConditionTrue)
				degraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionTrue)

				GinkgoWriter.Printf("Storage ClusterOperator status - Available: %v, Progressing: %v, Degraded: %v\n",
					available, progressing, degraded)

				Expect(available).To(BeTrue(), "Storage operator should be Available")
				Expect(progressing).To(BeFalse(), "Storage operator should not be Progressing")
				Expect(degraded).To(BeFalse(), "Storage operator should not be Degraded")

				GinkgoWriter.Printf("✓ Storage operator is healthy\n")

			} else if isHybridEnvironment {
				GinkgoWriter.Printf("⚠ Hybrid environment detected: %d vSphere nodes, %d bare metal nodes\n",
					len(nodesWithVSphereLabel), len(nodesWithoutLabel))

				By("Verifying ClusterCSIDriver is set to Removed (hybrid environment with bare metal nodes)")
				clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "Failed to get ClusterCSIDriver")

				managementState := clusterCSIDriver.Spec.ManagementState
				GinkgoWriter.Printf("ClusterCSIDriver %s has managementState: %s\n", csiDriverName, managementState)

				Expect(managementState).To(Equal(operatorapi.Removed),
					"ClusterCSIDriver should have managementState=Removed in hybrid environment with bare metal nodes")

				GinkgoWriter.Printf("✓ ClusterCSIDriver is correctly set to Removed\n")

			} else {
				// Neither all vSphere nor hybrid - this is a failure on a vSphere platform
				Fail(fmt.Sprintf("Test labeled for vSphere platform but found %d vSphere nodes and %d unlabeled nodes - nodes should have %s label when VSphereMixedNodeEnv feature gate is enabled",
					len(nodesWithVSphereLabel), len(nodesWithoutLabel), platformTypeLabelKey))
			}
		})

		It("should degrade when ClusterCSIDriver is set to Managed in hybrid environment [Suite:openshift/conformance/serial]", Label("Serial"), ginkgo.Informing(), func() {
			By("Listing all nodes in the cluster")
			nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list nodes")
			Expect(nodes.Items).NotTo(BeEmpty(), "Expected to find at least one node in the cluster")

			By("Checking if this is a hybrid environment")
			var nodesWithoutLabel []string
			var nodesWithVSphereLabel []string

			for _, node := range nodes.Items {
				platformType, hasLabel := node.Labels[platformTypeLabelKey]
				if !hasLabel {
					nodesWithoutLabel = append(nodesWithoutLabel, node.Name)
				} else if platformType == vSpherePlatformType {
					nodesWithVSphereLabel = append(nodesWithVSphereLabel, node.Name)
				}
			}

			isHybridEnvironment := len(nodesWithoutLabel) > 0 && len(nodesWithVSphereLabel) > 0

			if !isHybridEnvironment {
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

				Eventually(func() error {
					currentCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(restoreCtx, csiDriverName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if currentCSIDriver.Spec.ManagementState == originalManagementState {
						return nil
					}
					currentCSIDriver.Spec.ManagementState = originalManagementState
					_, err = operatorClient.OperatorV1().ClusterCSIDrivers().Update(restoreCtx, currentCSIDriver, metav1.UpdateOptions{})
					return err
				}, clusterCSIDriverUpdateTimeout, clusterCSIDriverUpdatePollInterval).Should(Succeed(), "Failed to restore ClusterCSIDriver to original state")

				GinkgoWriter.Printf("✓ ClusterCSIDriver restored to %s\n", originalManagementState)

				By("Waiting for storage operator to return to healthy state")
				Eventually(func() bool {
					clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(restoreCtx, storageCOName, metav1.GetOptions{})
					if err != nil {
						GinkgoWriter.Printf("Failed to get ClusterOperator during cleanup: %v\n", err)
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
				}, operatorHealthTimeout, operatorHealthPollInterval).Should(BeTrue(), "Storage operator should return to healthy state after restoration")

				GinkgoWriter.Printf("✓ Storage operator is healthy again\n")
			}()

			By("Setting ClusterCSIDriver to Managed (should cause degradation)")
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
			}, clusterCSIDriverUpdateTimeout, clusterCSIDriverUpdatePollInterval).Should(Succeed(), "Failed to set ClusterCSIDriver to Managed")

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
