// Package buildinfo stores version information injected at build time.
// The main package sets Version via ldflags; other packages read it directly.
package buildinfo

// Version is injected at build time via -ldflags "-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=x.y.z".
var Version = "dev"
