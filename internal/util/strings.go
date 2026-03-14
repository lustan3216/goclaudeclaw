// Package util 提供跨包共用的小工具函数。
package util

// Truncate 将字符串截断到 n 个 Unicode 码位，超出部分用 "..." 代替。
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
