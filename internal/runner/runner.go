// Package runner executes claude CLI commands, maintaining a serial queue per workspace
// to ensure tasks in the same directory are executed in order, avoiding concurrent file write conflicts.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/memory"
	"github.com/lustan3216/claudeclaw/internal/session"
)

// Result holds the claude execution result.
type Result struct {
	Output      string
	Err         error
	InputTokens int // total input tokens used (input + cache_read); 0 if unknown
}

// Job is the unit of work submitted to the serial queue.
type Job struct {
	Ctx       context.Context
	Workspace string
	BotName   string // bot name, used as session key
	ChatID    int64  // Telegram chat ID
	TopicID   int    // Telegram topic ID (forum thread); 0 means regular chat
	Prompt    string
	Mode      TaskMode
	ResultCh  chan<- Result // caller listens on this channel for the result
}

// claudeJSONOutput is the output structure of claude --output-format json.
type claudeJSONOutput struct {
	SessionID string              `json:"session_id"`
	Result    string              `json:"result"`
	Usage     claudeJSONUsage     `json:"usage"`
}

// claudeJSONUsage holds token counts from a claude execution.
type claudeJSONUsage struct {
	InputTokens            int `json:"input_tokens"`
	CacheReadInputTokens   int `json:"cache_read_input_tokens"`
	CacheCreateInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens           int `json:"output_tokens"`
}

// queueKey uniquely identifies a serial queue: each chat+topic has its own queue and doesn't block others.
type queueKey struct {
	workspace string
	botName   string
	chatID    int64
	topicID   int
}

// Manager manages serial execution queues for all sessions.
// Each workspace+bot+chat+topic has its own queue to avoid different topics blocking each other.
type Manager struct {
	mu         sync.Mutex
	queues     map[queueKey]chan Job
	sessions   *session.Manager
	cfg        *config.Config
	claudePath string
}

// NewManager creates a Runner Manager.
func NewManager(cfg *config.Config, sessions *session.Manager, claudePath string) *Manager {
	return &Manager{
		queues:     make(map[queueKey]chan Job),
		sessions:   sessions,
		cfg:        cfg,
		claudePath: claudePath,
	}
}

// UpdateConfig updates the config reference on hot-reload (caller holds lock protection).
func (m *Manager) UpdateConfig(cfg *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// Submit enqueues a job into the serial queue for the corresponding chat+topic.
// For ModeBackground jobs, resultCh may be nil (caller does not wait for result).
func (m *Manager) Submit(job Job) {
	key := queueKey{job.Workspace, job.BotName, job.ChatID, job.TopicID}
	q := m.getOrCreateQueue(key)

	select {
	case q <- job:
		slog.Debug("job enqueued", "workspace", job.Workspace, "chat_id", job.ChatID, "topic_id", job.TopicID, "mode", job.Mode)
	case <-job.Ctx.Done():
		slog.Warn("context cancelled before job could be enqueued", "workspace", job.Workspace)
		if job.ResultCh != nil {
			job.ResultCh <- Result{Err: job.Ctx.Err()}
		}
	}
}

// getOrCreateQueue gets or creates the serial queue goroutine for the given chat+topic.
func (m *Manager) getOrCreateQueue(key queueKey) chan Job {
	m.mu.Lock()
	defer m.mu.Unlock()

	if q, ok := m.queues[key]; ok {
		return q
	}

	// Buffer size 32: allows short-term backlog to prevent Telegram message loss
	q := make(chan Job, 32)
	m.queues[key] = q
	go m.runQueue(key, q)
	slog.Info("serial execution queue created for chat+topic", "workspace", key.workspace, "chat_id", key.chatID, "topic_id", key.topicID)
	return q
}

// runQueue is the serial execution goroutine for each chat+topic,
// consuming jobs from the queue in order until the channel is closed.
func (m *Manager) runQueue(key queueKey, q <-chan Job) {
	for job := range q {
		result := m.execute(job)
		if job.ResultCh != nil {
			job.ResultCh <- result
		}
	}
}

// execute actually runs the claude CLI and returns the output.
// Logic:
//   - Known session → --output-format text --resume {sessionID}
//   - No session (new) → --output-format json, parse session_id from JSON output and persist
func (m *Manager) execute(job Job) Result {
	sessionID := m.sessions.Get(job.Workspace, job.BotName, job.ChatID, job.TopicID)
	isNewSession := sessionID == ""

	// Inject local memory for new sessions (saves tokens: resumed sessions already have context)
	// Uses section relevance scoring to only inject sections relevant to the current prompt
	prompt := job.Prompt
	if isNewSession {
		localMem := memory.NewLocalMemory(job.Workspace)
		if memContent, err := localMem.LoadRelevant(job.Prompt); err != nil {
			slog.Warn("failed to read local memory, skipping injection", "err", err)
		} else if memContent != "" {
			prompt = memory.InjectPrefix(memContent, prompt)
			slog.Debug("relevant memory injected", "memory_len", len(memContent))
		}

		// Inject preferences.md (behavioral rules Claude has written for itself)
		if prefsContent, err := localMem.LoadPreferences(); err != nil {
			slog.Warn("failed to read preferences, skipping", "err", err)
		} else if prefsContent != "" {
			prompt = memory.InjectPrefix(prefsContent, prompt)
			slog.Debug("preferences injected", "prefs_len", len(prefsContent))
		}

		// Inject bot persona so Claude identifies as "claudeclaw" in Telegram conversations
		const botPersona = "[claudeclaw context: You are responding via claudeclaw, a Telegram-to-Claude Code bridge. " +
			"When asked who you are, introduce yourself as claudeclaw. Keep responses concise for chat.]"
		prompt = memory.InjectPrefix(botPersona, prompt)
	}
	job.Prompt = prompt

	args := m.buildArgs(job, sessionID)

	slog.Info("executing claude",
		"workspace", job.Workspace,
		"bot", job.BotName,
		"chat_id", job.ChatID,
		"topic_id", job.TopicID,
		"mode", job.Mode,
		"session_id", sessionID,
		"new_session", isNewSession)

	cmd := exec.CommandContext(job.Ctx, m.claudePath, args...)
	cmd.Dir = job.Workspace

	// Filter CLAUDECODE env vars to prevent claude from refusing nested launches
	cmd.Env = filteredEnv()

	// Stream output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Err: fmt.Errorf("failed to get stdout pipe: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{Err: fmt.Errorf("failed to get stderr pipe: %w", err)}
	}

	if err := cmd.Start(); err != nil {
		return Result{Err: fmt.Errorf("failed to start claude: %w", err)}
	}

	// Concurrently read stdout and stderr
	var outputBuilder strings.Builder
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			outputBuilder.WriteString(line)
			outputBuilder.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Debug("claude stderr", "line", scanner.Text(), "workspace", job.Workspace)
		}
	}()

	wg.Wait()
	if err := cmd.Wait(); err != nil {
		// Context cancellation is normal — not treated as an error
		if job.Ctx.Err() != nil {
			return Result{Err: fmt.Errorf("job cancelled: %w", job.Ctx.Err())}
		}
		return Result{
			Output: outputBuilder.String(),
			Err:    fmt.Errorf("claude exited with error: %w", err),
		}
	}

	rawOutput := strings.TrimSpace(outputBuilder.String())

	// New session: parse JSON output to extract session_id and token usage
	if isNewSession {
		if jsonOut, err := parseJSONOutput(rawOutput); err == nil && jsonOut.SessionID != "" {
			if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, jsonOut.SessionID); err != nil {
				slog.Warn("failed to persist session ID", "err", err)
			} else {
				slog.Info("new session created and persisted",
					"session_id", jsonOut.SessionID,
					"bot", job.BotName,
					"chat_id", job.ChatID,
					"topic_id", job.TopicID)
			}
			totalIn := jsonOut.Usage.InputTokens + jsonOut.Usage.CacheReadInputTokens + jsonOut.Usage.CacheCreateInputTokens
			return Result{Output: strings.TrimSpace(jsonOut.Result), InputTokens: totalIn}
		}
		// JSON parse failed: fallback to legacy session ID extraction
		if newID := extractSessionID(rawOutput); newID != "" {
			if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, newID); err != nil {
				slog.Warn("failed to persist session ID (fallback mode)", "err", err)
			}
		}
		return Result{Output: rawOutput}
	}

	// Existing session (--resume mode): plain text output, check for new session ID (rotation case)
	if newID := extractSessionID(rawOutput); newID != "" && newID != sessionID {
		if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, newID); err != nil {
			slog.Warn("failed to persist new session ID", "err", err)
		}
	}

	return Result{Output: rawOutput}
}

// buildArgs assembles the claude command-line arguments based on job config.
func (m *Manager) buildArgs(job Job, sessionID string) []string {
	args := []string{}

	// Non-interactive mode must skip permission prompts; otherwise claude blocks waiting for terminal input.
	args = append(args, "--dangerously-skip-permissions")

	if sessionID != "" {
		// Existing session: use text output format to resume
		args = append(args, "--output-format", "text")
		args = append(args, "--resume", sessionID)
	} else {
		// New session: use JSON output format to capture session_id
		args = append(args, "--output-format", "json")
	}

	// Single-shot prompt mode (non-interactive)
	args = append(args, "-p", job.Prompt)

	if job.Mode == ModeBackground {
		slog.Debug("background job, running in silent mode")
	}

	return args
}

// parseJSONOutput parses the output of claude --output-format json.
// claude may output multiple lines; takes the last line (or first valid JSON object).
func parseJSONOutput(output string) (*claudeJSONOutput, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// Scan backwards from the last line for the first successfully parseable JSON
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var out claudeJSONOutput
		if err := json.Unmarshal([]byte(line), &out); err == nil {
			return &out, nil
		}
	}
	return nil, fmt.Errorf("no valid JSON output found")
}

// extractSessionID extracts a session ID from claude output (legacy format compatibility).
// claude output formats: [session: abc123] or Session ID: abc123
func extractSessionID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// Match [session: <id>]
		if strings.HasPrefix(line, "[session:") && strings.HasSuffix(line, "]") {
			id := strings.TrimPrefix(line, "[session:")
			id = strings.TrimSuffix(id, "]")
			return strings.TrimSpace(id)
		}
		// Match Session ID: <id>
		if strings.HasPrefix(line, "Session ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Session ID:"))
		}
	}
	return ""
}
