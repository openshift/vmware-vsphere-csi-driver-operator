package vspherecontroller

import (
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
)

func (c *VSphereController) createWebHookController() {
	webhookBytes, err := assets.ReadFile("webhook/deployment.yaml")
	if err != nil {
		panic("can not read webhook/deployment.yaml file")
	}
	webhookController := deploymentcontroller.NewDeploymentController(
		"VMwareVSphereDriverWebhookController",
		webhookBytes,
		c.eventRecorder,
		c.operatorClient,
		c.apiClients.KubeClient,
		c.apiClients.KubeInformers.InformersFor(defaultNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			c.apiClients.SecretInformer.Informer(),
			c.apiClients.ConfigInformers.Config().V1().Infrastructures().Informer(),
		},
		nil, // optionalManifestHooks
		WithSyncerImageHook("vsphere-webhook"),
		csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(c.apiClients.ConfigInformers),
		csidrivercontrollerservicecontroller.WithReplicasHook(c.apiClients.ConfigInformers),
		WithLogLevelDeploymentHook(),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			webhookSecretName,
			c.apiClients.SecretInformer,
		),
	)
	c.controllers = append(c.controllers, conditionalController{
		name:       webhookController.Name(),
		controller: webhookController,
	})
}
