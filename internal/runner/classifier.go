// Package runner contains the claude CLI executor and task classifier.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/lustan3216/claudeclaw/internal/util"
)

// TaskMode indicates how a task should be run.
type TaskMode int

const (
	// ModeForeground foreground mode: stream output, reply to Telegram after completion.
	ModeForeground TaskMode = iota
	// ModeBackground background mode: immediately tell the user "processing in background",
	// runs in an independent goroutine without blocking the current message queue.
	ModeBackground
)

// classificationTimeout is the maximum time the classifier will wait before defaulting to foreground.
const classificationTimeout = 10 * time.Second

// classifyPromptTemplate is the classification prompt template sent to claude.
// Formatted with fmt.Sprintf; %q auto-escapes quotes in the message content.
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

// Classifier uses the claude CLI to do lightweight message classification.
// Spawns an independent claude process; does not reuse the main session to avoid context pollution.
type Classifier struct {
	claudePath string // path to the claude binary; defaults to "claude"
}

// NewClassifier creates a classifier. Pass an empty string for claudePath to auto-find in PATH.
func NewClassifier(claudePath string) *Classifier {
	if claudePath == "" {
		claudePath = "claude"
	}
	return &Classifier{claudePath: claudePath}
}

// Classify classifies a message, returning ModeForeground or ModeBackground.
// Any error (timeout, claude unavailable, etc.) safely falls back to foreground mode.
func (c *Classifier) Classify(ctx context.Context, message string) TaskMode {
	ctx, cancel := context.WithTimeout(ctx, classificationTimeout)
	defer cancel()

	prompt := buildClassifyPrompt(message)

	// Use -p single-shot prompt mode; intentionally no --resume to ensure a clean context-free call
	cmd := exec.CommandContext(ctx, c.claudePath,
		"--dangerously-skip-permissions",
		"-p", prompt,
	)

	// Filter CLAUDECODE env vars to prevent claude from refusing nested launches
	cmd.Env = filteredEnv()

	output, err := cmd.Output()
	if err != nil {
		slog.Warn("classifier call failed, falling back to foreground mode",
			"err", err,
			"message_preview", util.Truncate(message, 50))
		return ModeForeground
	}

	result := strings.TrimSpace(strings.ToUpper(string(output)))

	// Only look at the first line to guard against the model outputting extra content
	if lines := strings.SplitN(result, "\n", 2); len(lines) > 0 {
		result = strings.TrimSpace(lines[0])
	}

	slog.Debug("message classification result",
		"result", result,
		"message_preview", util.Truncate(message, 50))

	if strings.Contains(result, "BACKGROUND") {
		return ModeBackground
	}
	// Default to foreground — conservative strategy
	return ModeForeground
}

// buildClassifyPrompt constructs the classification prompt sent to claude.
// Uses the %q verb so fmt.Sprintf auto-escapes message content to prevent injection.
func buildClassifyPrompt(message string) string {
	return fmt.Sprintf(classifyPromptTemplate, fmt.Sprintf("%q", message))
}
