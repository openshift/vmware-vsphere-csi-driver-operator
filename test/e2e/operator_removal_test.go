package e2e

import (
	"context"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	clusteroperatorhelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/test/library"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
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

func TestStorageRemoval(t *testing.T) {
	kubeConfig, err := library.NewClientConfigForTest()
	if err != nil {
		t.Fatalf("Failed to get kubeconfig: %v", err)
	}

	kubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))
	ocpOperatorClient := operatorclient.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))

	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))
	t.Logf("Waiting for storage to be available")
	waitForStorageAvailable(t, configClient, ocpOperatorClient, kubeClient)
	t.Logf("Marking ClusterCSIDriver as removed")
	err = makeClusterCSIDriverRemoved(context.TODO(), ocpOperatorClient)

	if err != nil {
		restoreStorage(t, ocpOperatorClient, configClient, kubeClient)
		t.Fatalf("Failed to update ClusterCSIDriver: %v", err)
	}
	t.Logf("removed storage operator from cluster")
	err = waitForStorageResourceRemoval(t, ocpOperatorClient, kubeClient)
	if err != nil {
		restoreStorage(t, ocpOperatorClient, configClient, kubeClient)
		t.Fatalf("Failed to wait for storage resource removal: %v", err)
	}
	time.Sleep(10 * time.Second)
	restoreStorage(t, ocpOperatorClient, configClient, kubeClient)
}

func restoreStorage(t *testing.T, client *operatorclient.Clientset, configClient *configclient.Clientset, kubeClient *kubernetes.Clientset) {
	t.Log("Restoring storage operator to cluster")
	err := makeClusterCSIDriverManaged(context.TODO(), client)
	if err != nil {
		t.Fatalf("Failed to update ClusterCSIDriver: %v", err)
	}
	waitForStorageAvailable(t, configClient, client, kubeClient)
}

func waitForStorageResourceRemoval(t *testing.T, client *operatorclient.Clientset, kubeClient *kubernetes.Clientset) error {
	// make sure storage to be removed
	klog.Infof("Waiting for storage resource to be removed")
	return wait.PollUntilContextTimeout(context.TODO(), WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		disabledConditionStatusVar, err := checkDisabledCondition(t, pollContext, client)
		if err != nil {
			return false, err
		}
		done := disabledConditionStatusVar == operatorDisabled
		if done {
			deploymentCreated, err := checkForDeploymentCreation(t, pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = !deploymentCreated
		}

		if done {
			daemonsetCreated, err := checkForDaemonset(t, pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = !daemonsetCreated
		}
		return done, nil
	})
}

func waitForStorageAvailable(t *testing.T, client *configclient.Clientset, operatorClient *operatorclient.Clientset, kubeClient *kubernetes.Clientset) {
	err := wait.PollUntilContextTimeout(context.TODO(), WaitPollInterval, WaitPollTimeout, false, func(pollContext context.Context) (bool, error) {
		clusterOperator, err := client.ConfigV1().ClusterOperators().Get(pollContext, storageCOName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			t.Log("ClusterOperator/storage does not yet exist.")
			return false, nil
		}
		if err != nil {
			t.Log("Unable to retrieve ClusterOperator/storage:", err)
			return false, err
		}
		conditions := clusterOperator.Status.Conditions
		available := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorAvailable, configv1.ConditionTrue)
		notProgressing := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorProgressing, configv1.ConditionFalse)
		notDegraded := clusteroperatorhelpers.IsStatusConditionPresentAndEqual(conditions, configv1.OperatorDegraded, configv1.ConditionFalse)
		done := available && notProgressing && notDegraded

		if done {
			disableConditionStatusVar, err := checkDisabledCondition(t, pollContext, operatorClient)
			if err != nil {
				return false, err
			}
			done = disableConditionStatusVar == operatorEnabled
		}

		if done {
			deploymentCreated, err := checkForDeploymentCreation(t, pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = deploymentCreated
		}

		if done {
			daemonsetCreated, err := checkForDaemonset(t, pollContext, kubeClient)
			if err != nil {
				return false, err
			}
			done = daemonsetCreated
		}

		t.Logf("ClusterOperator/storage: Available: %v  Progressing: %v  Degraded: %v\n", available, !notProgressing, !notDegraded)
		return done, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkDisabledCondition(t *testing.T, ctx context.Context, operatorClient *operatorclient.Clientset) (disableConditionStatus, error) {
	clusterCSIDriver, err := operatorClient.OperatorV1().ClusterCSIDrivers().Get(ctx, csiDriverName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		t.Log("cluster-csi-driver does not yet exist.")
		return clusterCSIDriverNotFound, nil
	}
	if err != nil {
		t.Log("Unable to retrieve cluster-csi-driver", err)
		return clusterCSIDriverNotFound, err
	}
	disabledCondition := v1helpers.FindOperatorCondition(clusterCSIDriver.Status.Conditions, disabledCondition)
	if disabledCondition != nil && disabledCondition.Status == operatorapi.ConditionTrue {
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
			klog.Infof("ClusterCSIDriver %s already in removed state", csiDriverName)
			return true, nil
		}

		clusterCSIDriver.Spec.ManagementState = operatorapi.Removed
		client.OperatorV1().ClusterCSIDrivers().Update(pollContext, clusterCSIDriver, metav1.UpdateOptions{})
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
		client.OperatorV1().ClusterCSIDrivers().Update(pollContext, clusterCSIDriver, metav1.UpdateOptions{})
		return false, nil
	})
}

func checkForDeploymentCreation(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	deployment, err := kubeClient.AppsV1().Deployments(csiDriverNameSpace).Get(ctx, csiControllerDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		t.Log("Deployment does not yet exist.")
		return false, nil
	}
	if err != nil {
		t.Logf("Unable to retrieve deployment: %v", err)
		return false, err
	}
	return deployment != nil, nil
}

func checkForDaemonset(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset) (bool, error) {
	daemonset, err := kubeClient.AppsV1().DaemonSets(csiDriverNameSpace).Get(ctx, csiNodeDaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		t.Log("Daemonset does not yet exist.")
		return false, nil
	}
	if err != nil {
		t.Logf("Unable to retrieve daemonset: %v", err)
		return false, err
	}
	return daemonset != nil, nil
}
