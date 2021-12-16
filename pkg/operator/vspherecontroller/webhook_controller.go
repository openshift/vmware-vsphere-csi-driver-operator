package vspherecontroller

import (
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
		nil, // optionalInformers
		nil, // optionalManifestHooks
		WithSyncerImageHook("vsphere-webhook"),
	)
	c.controllers = append(c.controllers, conditionalController{
		name:       webhookController.Name(),
		controller: webhookController,
	})
}
