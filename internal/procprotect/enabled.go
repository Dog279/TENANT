//go:build !debugattach

package procprotect

// Disabled is true in a build made with -tags debugattach.
const Disabled = false
