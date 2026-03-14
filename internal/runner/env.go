package runner

import (
	"os"
	"strings"
)

// filteredEnv 返回当前进程环境变量，去除含 "CLAUDECODE" 的条目，
// 避免 claude 拒绝嵌套启动。
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
