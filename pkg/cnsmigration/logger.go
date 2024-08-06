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

func printErrorObject(err error) {
	color.Set(color.FgRed)
	klog.Error(err)
	color.Unset()
}

func printInfo(format string, args ...interface{}) {
	color.Set(color.FgMagenta)
	klog.Infof(format, args...)
	color.Unset()
}

func printGreenInfo(format string, args ...interface{}) {
	color.Set(color.FgGreen)
	klog.Infof(format, args...)
	color.Unset()
}
