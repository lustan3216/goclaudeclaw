package runner

import (
	"os"
	"strings"
)

// filteredEnv returns the current process environment variables with any entries
// containing "CLAUDECODE" removed, to prevent claude from refusing nested launches.
func filteredEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.Contains(e, "CLAUDECODE") {
			out = append(out, e)
		}
	}
	return out
}
