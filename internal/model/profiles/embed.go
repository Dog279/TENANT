// Package profiles holds the shipped Profile YAML defaults that ship
// with the binary via go:embed. User overrides at
// ~/.config/tenant/profiles/*.yaml are loaded by the registry on top
// and merged by Profile ID.
package profiles

import "embed"

//go:embed *.yaml
var Embedded embed.FS
