package targetconfigcontroller

import (
	"context"
	"strings"
	"time"

	clustercsidriverinformer "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	clustercsidriverlister "github.com/openshift/client-go/operator/listers/operator/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"

	opv1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	infraGlobalName      = "cluster"
	cloudConfigNamespace = "openshift-config"
)

type TargetConfigController struct {
	name                   string
	targetNamespace        string
	kubeClient             kubernetes.Interface
	operatorClient         v1helpers.OperatorClient
	configMapLister        corelister.ConfigMapLister
	clusterCSIDriverLister clustercsidriverlister.ClusterCSIDriverLister
	infraLister            infralister.InfrastructureLister
}

func NewTargetConfigController(
	name,
	targetNamespace string,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	clusterCSIDriverInformer clustercsidriverinformer.ClusterCSIDriverInformer,
	recorder events.Recorder,
) factory.Controller {
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
	infraInformer := configInformer.Config().V1().Infrastructures()
	c := &TargetConfigController{
		name:                   name,
		targetNamespace:        targetNamespace,
		kubeClient:             kubeClient,
		operatorClient:         operatorClient,
		configMapLister:        configMapInformer.Lister(),
		infraLister:            infraInformer.Lister(),
		clusterCSIDriverLister: clusterCSIDriverInformer.Lister(),
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
		infraInformer.Informer(),
		operatorClient.Informer(),
		clusterCSIDriverInformer.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		20*time.Minute, // TODO: figure out what's the idead resync time for this controller
	).ToController(
		c.name,
		recorder.WithComponentSuffix("target-config-controller-"+strings.ToLower(name)),
	)
}

func (c TargetConfigController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState != opv1.Managed {
		return nil
	}

	removalConditions := map[string]bool{
		c.name + opv1.OperatorStatusTypeAvailable:   true,
		c.name + opv1.OperatorStatusTypeProgressing: true,
		c.name + opv1.OperatorStatusTypeDegraded:    true,
	}

	removeStaleConditionsFn := func(status *opv1.OperatorStatus) error {
		oldConditions := status.Conditions
		if oldConditions == nil {
			oldConditions = []opv1.OperatorCondition{}
		}
		newConditions := []opv1.OperatorCondition{}
		for _, oldCondition := range oldConditions {
			if _, ok := removalConditions[oldCondition.Type]; !ok {
				newConditions = append(newConditions, oldCondition)
			}
		}
		status.Conditions = newConditions
		return nil
	}

	_, _, err = v1helpers.UpdateStatus(
		ctx,
		c.operatorClient,
		removeStaleConditionsFn,
	)
	return err
}
