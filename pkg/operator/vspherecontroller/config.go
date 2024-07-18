package vspherecontroller

import (
	"fmt"
	"strings"

	iniv1 "gopkg.in/ini.v1"
)

// iniConfig abstracts INI configuration handling. This struct is used to manage
// configuration settings, allowing for easier testing and future refactoring.
// Currently, it uses the ini.v1 package, but it is designed to facilitate
// migration to gcfg or other configuration management libraries in the future.
type iniConfig struct {
	file *iniv1.File
}

// newINIConfig creates a new iniConfig instance.
func newINIConfig(config string) (*iniConfig, error) {
	// We ignore inline comments when reading the INI file
	// because passwords may contain comment symbols (# and ;)
	options := iniv1.LoadOptions{IgnoreInlineComment: true}
	csiConfig, err := iniv1.LoadSources(options, []byte(config))
	if err != nil {
		return nil, fmt.Errorf("error loading ini file: %v", err)
	}
	return &iniConfig{file: csiConfig}, nil

}

// Set will set or update the value for a given key in the given section.
func (i *iniConfig) Set(section, key, value string) {
	i.file.Section(section).Key(key).SetValue(value)
}

// String returns the string representation of the INI configuration.
func (i *iniConfig) String() string {
	if i == nil {
		return ""
	}
	var b strings.Builder
	i.file.WriteTo(&b)
	return strings.TrimSpace(b.String())
}
