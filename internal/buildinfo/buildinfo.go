// Package buildinfo 存储构建时注入的版本信息。
// main 包通过 ldflags 设置 Version，其他包直接读取。
package buildinfo

// Version 在构建时通过 -ldflags "-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=x.y.z" 注入。
var Version = "dev"
