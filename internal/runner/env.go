package runner

import (
	"os"
	"strings"
)

// filteredEnv returns the current process environment with CLAUDECODE vars removed
// (to prevent claude from refusing nested launches), optionally overriding ANTHROPIC_API_KEY
// or injecting CLAUDE_CODE_OAUTH_TOKEN for setup-token based credentials.
func filteredEnv(apiKey, oauthToken string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		if strings.Contains(e, "CLAUDECODE") {
			continue
		}
		if apiKey != "" && strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			continue
		}
		if oauthToken != "" && strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			continue
		}
		out = append(out, e)
	}
	if apiKey != "" {
		out = append(out, "ANTHROPIC_API_KEY="+apiKey)
	}
	if oauthToken != "" {
		out = append(out, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
	}
	return out
}
