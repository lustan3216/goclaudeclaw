// Package util provides small utility functions shared across packages.
package util

// Truncate truncates a string to n Unicode code points, replacing the remainder with "...".
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
