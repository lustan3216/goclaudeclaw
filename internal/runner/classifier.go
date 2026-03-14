// Package runner 包含 claude CLI 执行器和任务分类器。
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/lustan3216/goclaudeclaw/internal/util"
)

// TaskMode 表示任务应以何种方式运行。
type TaskMode int

const (
	// ModeForeground 前台模式：流式输出，等待完成后回复 Telegram。
	ModeForeground TaskMode = iota
	// ModeBackground 后台模式：立即告知用户"已在后台处理"，
	// 使用独立 goroutine 运行，不阻塞当前消息队列。
	ModeBackground
)

// classificationTimeout 分类器最多等待多久，超时则默认前台。
const classificationTimeout = 10 * time.Second

// classifyPromptTemplate 发给 claude 的分类 prompt 模板。
// 使用 fmt.Sprintf 格式化，%q 会对消息内容自动转义引号。
const classifyPromptTemplate = `You are a task classifier for an AI assistant.
Classify the following user message into exactly one of two modes:

BACKGROUND - Use this when the task is:
- Long-running (crawling, batch processing, large refactors, research)
- Independent (doesn't need back-and-forth with the user)
- Fire-and-forget (user wants results later, not a conversation)

FOREGROUND - Use this when the task is:
- Conversational or needs clarification
- Quick to complete
- Requires interactive feedback or follow-up questions

User message: %s

Reply with ONLY one word: BACKGROUND or FOREGROUND`

// Classifier 使用 claude CLI 对消息做轻量分类。
// 调用一次独立的 claude 进程，不复用主会话，避免污染上下文。
type Classifier struct {
	claudePath string // claude 二进制路径，默认 "claude"
}

// NewClassifier 创建分类器。claudePath 传空字符串则自动查找 PATH。
func NewClassifier(claudePath string) *Classifier {
	if claudePath == "" {
		claudePath = "claude"
	}
	return &Classifier{claudePath: claudePath}
}

// Classify 对 message 进行分类，返回 ModeForeground 或 ModeBackground。
// 任何错误（超时、claude 不可用等）都安全降级为前台模式。
func (c *Classifier) Classify(ctx context.Context, message string) TaskMode {
	ctx, cancel := context.WithTimeout(ctx, classificationTimeout)
	defer cancel()

	prompt := buildClassifyPrompt(message)

	// 使用 -p 单次 prompt 模式，--no-cache 避免缓存影响分类结果
	// 注意：分类器故意不传 --resume，确保是全新的无上下文调用
	cmd := exec.CommandContext(ctx, c.claudePath,
		"--dangerously-skip-permissions",
		"-p", prompt,
	)

	// 过滤 CLAUDECODE 环境变量，避免 claude 拒绝嵌套启动
	cmd.Env = filteredEnv()

	output, err := cmd.Output()
	if err != nil {
		slog.Warn("分类器调用失败，降级为前台模式",
			"err", err,
			"message_preview", util.Truncate(message, 50))
		return ModeForeground
	}

	result := strings.TrimSpace(strings.ToUpper(string(output)))

	// 只识别首行，防止模型输出多余内容
	if lines := strings.SplitN(result, "\n", 2); len(lines) > 0 {
		result = strings.TrimSpace(lines[0])
	}

	slog.Debug("消息分类结果",
		"result", result,
		"message_preview", util.Truncate(message, 50))

	if strings.Contains(result, "BACKGROUND") {
		return ModeBackground
	}
	// 默认前台，保守策略
	return ModeForeground
}

// buildClassifyPrompt 构建发给 claude 的分类 prompt。
// 使用 %q 动词让 fmt.Sprintf 自动对消息内容转义，防止注入。
func buildClassifyPrompt(message string) string {
	return fmt.Sprintf(classifyPromptTemplate, fmt.Sprintf("%q", message))
}

