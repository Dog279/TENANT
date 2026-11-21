//go:build debugattach

package procprotect

// Disabled is true in a build made with -tags debugattach: the controls are
// compiled out so a developer can attach a debugger. `tenant version` and
// `tenant doctor` say so.
const Disabled = true
