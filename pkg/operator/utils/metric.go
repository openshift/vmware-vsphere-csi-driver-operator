package utils

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	failureReason = "failure_reason"
	condition     = "condition"
)

var (
	InstallErrorMetric = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "vsphere_csi_driver_error",
			Help:           "vSphere driver installation error",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{failureReason, condition},
	)
)

func init() {
	legacyregistry.MustRegister(InstallErrorMetric)
}
