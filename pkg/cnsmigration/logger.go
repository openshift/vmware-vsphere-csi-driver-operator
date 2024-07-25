package cnsmigration

import (
	"github.com/fatih/color"
	"k8s.io/klog/v2"
)

func printError(msg string, options ...interface{}) {
	color.Set(color.FgRed)
	klog.Errorf(msg, options...)
	color.Unset()
}

func printInfo(format string, args ...interface{}) {
	color.Set(color.FgMagenta)
	klog.Infof(format, args...)
	color.Unset()
}
