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

// TaskMode indicates how a task should be run.
type TaskMode int

const (
	// ModeForeground streams output and replies to Telegram after completion.
	ModeForeground TaskMode = iota
	// ModeBackground runs in an independent goroutine and immediately notifies the user.
	ModeBackground
)

// ToolEventType indicates what happened with a tool call.
type ToolEventType int

const (
	ToolStarted   ToolEventType = iota // tool call started
	ToolCompleted                       // tool call finished
)

// ToolEvent is emitted when Claude calls any tool (Agent, Read, Write, Bash, etc.).
type ToolEvent struct {
	Type         ToolEventType
	ToolUseID    string // unique ID for this tool invocation
	ToolName     string // tool name: "Agent", "Read", "Bash", "Edit", etc.
	Summary      string // human-readable summary of what this tool call does
	SubagentType string // only for Agent tool: e.g. "Explore", "Plan"
}

// Result holds the claude execution result.
type Result struct {
	Output      string
	Err         error
	InputTokens int // total input tokens used (input + cache_read); 0 if unknown
}

// Job is the unit of work submitted to the serial queue.
type Job struct {
	Ctx              context.Context
	Workspace        string
	BotName          string   // bot name, used as session key
	ChatID           int64    // Telegram chat ID
	TopicID          int      // Telegram topic ID (forum thread); 0 means regular chat
	Prompt           string
	Mode             TaskMode
	AnthropicAPIKeys  []string                   // API keys to try in order; falls back to env ANTHROPIC_API_KEY if empty
	ClaudeCredentials []config.ClaudeCredential  // OAuth credential sets; tried in order after API keys are exhausted
	Model             string                     // model override passed as --model flag; empty = claude's default
	ResultCh         chan<- Result               // caller listens on this channel for the result
	ToolEventCh      chan<- ToolEvent            // optional: receives tool start/complete events (nil = no events)
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
		if job.ToolEventCh != nil {
			close(job.ToolEventCh)
		}
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
		// Close agent event channel before sending result so dispatcher
		// can drain all events before processing the final result.
		if job.ToolEventCh != nil {
			close(job.ToolEventCh)
		}
		if job.ResultCh != nil {
			job.ResultCh <- result
		}
	}
}

// rateLimitPhrases are substrings in stderr/stdout that indicate a key-related failure
// worth retrying with a different API key.
var rateLimitPhrases = []string{
	"rate_limit_error",
	"overloaded_error",
	"insufficient_credits",
	"credit_balance",
	"authentication_error",
	"invalid_api_key",
	"permission_error",
}

// isKeyError returns true if the output looks like a rate-limit, quota, or auth failure.
func isKeyError(output string) bool {
	lower := strings.ToLower(output)
	for _, phrase := range rateLimitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// execute actually runs the claude CLI and returns the output.
// Attempt order on auth/rate failures:
//  1. AnthropicAPIKeys (env-based auth) — tried in order
//  2. ClaudeCredentials (OAuth file-based auth) — each swaps ~/.claude/.credentials.json and forces a new session
//
// If neither is configured, a single attempt is made using the existing env/credentials as-is.
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

	// --- Phase 1: API keys ---
	// If no keys configured, one attempt with empty key (uses existing env/credentials file).
	keys := job.AnthropicAPIKeys
	if len(keys) == 0 {
		keys = []string{""}
	}
	var lastResult Result
	for i, key := range keys {
		lastResult = m.executeWithKey(job, sessionID, isNewSession, key, "")
		if lastResult.Err == nil {
			return lastResult
		}
		if i < len(keys)-1 && isKeyError(lastResult.Output+lastResult.Err.Error()) {
			slog.Warn("key error, retrying with next API key",
				"key_index", i, "err", lastResult.Err,
				"workspace", job.Workspace, "chat_id", job.ChatID)
			continue
		}
		break
	}

	// If the last API-key failure was not an auth/rate error, no point trying OAuth credentials.
	if lastResult.Err != nil && !isKeyError(lastResult.Output+lastResult.Err.Error()) {
		return lastResult
	}

	// --- Phase 2: Claude OAuth credentials ---
	// Two modes:
	//   - Full OAuth (access_token + refresh_token): swaps ~/.claude/.credentials.json, forces new session
	//   - Setup-token (access_token only):           injects CLAUDE_CODE_OAUTH_TOKEN env var
	for i, cred := range job.ClaudeCredentials {
		var credResult Result
		if cred.RefreshToken != "" {
			// Full OAuth credential — swap credentials file
			if err := swapCredential(cred); err != nil {
				slog.Warn("credential swap failed, skipping",
					"cred_index", i, "err", err)
				continue
			}
			slog.Info("retrying with Claude OAuth credential (file swap)",
				"cred_index", i, "workspace", job.Workspace, "chat_id", job.ChatID)
			credResult = m.executeWithKey(job, "", true, "", "") // force new session
		} else if cred.AccessToken != "" {
			// Setup-token credential — use CLAUDE_CODE_OAUTH_TOKEN env var
			slog.Info("retrying with Claude setup-token credential (env var)",
				"cred_index", i, "workspace", job.Workspace, "chat_id", job.ChatID)
			credResult = m.executeWithKey(job, "", true, "", cred.AccessToken)
		} else {
			slog.Warn("credential has no access_token, skipping", "cred_index", i)
			continue
		}
		lastResult = credResult
		if lastResult.Err == nil {
			return lastResult
		}
		if i < len(job.ClaudeCredentials)-1 && isKeyError(lastResult.Output+lastResult.Err.Error()) {
			slog.Warn("credential auth error, retrying with next credential",
				"cred_index", i, "err", lastResult.Err)
			continue
		}
		break
	}
	return lastResult
}

// streamEvent is the minimal structure for parsing stream-json NDJSON lines.
type streamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Message   json.RawMessage `json:"message"`
	Usage     json.RawMessage `json:"usage"`
	Content   json.RawMessage `json:"content"` // for user-type tool_result
}

// streamMessage is the assistant/user message envelope.
type streamMessage struct {
	Content []streamContentBlock `json:"content"`
}

// streamContentBlock is a single content block inside a message.
type streamContentBlock struct {
	Type       string          `json:"type"`
	Name       string          `json:"name,omitempty"`
	ID         string          `json:"id,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ContentVal json.RawMessage `json:"content,omitempty"` // tool_result content (can be string or array)
	Text       string          `json:"text,omitempty"`
}

// agentInput is the parsed input of an Agent tool_use call.
type agentInput struct {
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
}

// toolInput is a generic struct for extracting summaries from tool_use inputs.
type toolInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Command  string `json:"command"`
	Pattern  string `json:"pattern"`
	Prompt   string `json:"prompt"`
	Query    string `json:"query"`
	Skill    string `json:"skill"`
	Content  string `json:"content"`
}

// toolSummary extracts a human-readable one-line summary from a tool_use block.
func toolSummary(name string, input json.RawMessage) string {
	var ti toolInput
	_ = json.Unmarshal(input, &ti)

	switch name {
	case "Agent":
		var ai agentInput
		_ = json.Unmarshal(input, &ai)
		return ai.Description
	case "Read":
		return shortenPath(ti.FilePath)
	case "Write":
		return shortenPath(ti.FilePath)
	case "Edit":
		return shortenPath(ti.FilePath)
	case "Bash":
		return truncate(ti.Command, 60)
	case "Grep":
		s := ti.Pattern
		if ti.Path != "" {
			s += " in " + shortenPath(ti.Path)
		}
		return s
	case "Glob":
		return ti.Pattern
	case "Skill":
		return ti.Skill
	case "WebSearch", "WebFetch":
		if ti.Query != "" {
			return truncate(ti.Query, 60)
		}
		return ""
	default:
		return ""
	}
}

// shortenPath returns the last 2 path components for brevity.
func shortenPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}

// truncate returns the first n characters of s, adding "…" if truncated.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	// 去掉换行
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// streamResultUsage is the usage field in the result event.
type streamResultUsage struct {
	InputTokens            int `json:"input_tokens"`
	CacheReadInputTokens   int `json:"cache_read_input_tokens"`
	CacheCreateInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens           int `json:"output_tokens"`
}

// executeWithKey runs the claude CLI with a specific API key or OAuth token (empty = use env var).
func (m *Manager) executeWithKey(job Job, sessionID string, isNewSession bool, apiKey, oauthToken string) Result {
	args := m.buildArgs(job, sessionID)

	slog.Info("executing claude",
		"workspace", job.Workspace,
		"bot", job.BotName,
		"chat_id", job.ChatID,
		"topic_id", job.TopicID,
		"mode", job.Mode,
		"session_id", sessionID,
		"new_session", isNewSession,
		"has_key_override", apiKey != "",
		"has_oauth_token", oauthToken != "")

	cmd := exec.CommandContext(job.Ctx, m.claudePath, args...)
	cmd.Dir = job.Workspace
	cmd.Env = filteredEnv(apiKey, oauthToken)

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

	// Track active Agent tool_use IDs to match tool_results back to subagent starts
	activeAgents := make(map[string]agentInput)
	activeTools := make(map[string]string) // tool_use_id → tool name

	// Parsed output fields extracted from stream events
	var parsedSessionID string
	var parsedResult string
	var parsedUsage streamResultUsage
	var gotResult bool

	var stderrBuilder strings.Builder
	var rawOutputBuilder strings.Builder
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		// stream-json can produce large lines (e.g. tool results with file content)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			rawOutputBuilder.WriteString(line)
			rawOutputBuilder.WriteByte('\n')

			if !strings.HasPrefix(line, "{") {
				continue
			}

			var evt streamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}

			switch evt.Type {
			case "system":
				// init event carries session_id
				if evt.Subtype == "init" && evt.SessionID != "" {
					parsedSessionID = evt.SessionID
				}

			case "assistant":
				// Detect ALL tool_use calls in assistant message content
				if len(evt.Message) > 0 {
					var msg streamMessage
					if err := json.Unmarshal(evt.Message, &msg); err == nil {
						for _, block := range msg.Content {
							if block.Type == "tool_use" && block.ID != "" {
								summary := toolSummary(block.Name, block.Input)
								activeTools[block.ID] = block.Name
								if block.Name == "Agent" {
									var inp agentInput
									_ = json.Unmarshal(block.Input, &inp)
									activeAgents[block.ID] = inp
								}
								if job.ToolEventCh != nil {
									te := ToolEvent{
										Type:      ToolStarted,
										ToolUseID: block.ID,
										ToolName:  block.Name,
										Summary:   summary,
									}
									if block.Name == "Agent" {
										var inp agentInput
										_ = json.Unmarshal(block.Input, &inp)
										te.SubagentType = inp.SubagentType
									}
									job.ToolEventCh <- te
								}
							}
						}
					}
				}

			case "user":
				// Detect tool_result completions
				if len(evt.Message) > 0 {
					var msg streamMessage
					if err := json.Unmarshal(evt.Message, &msg); err == nil {
						for _, block := range msg.Content {
							if block.Type == "tool_result" && block.ToolUseID != "" {
								toolName, ok := activeTools[block.ToolUseID]
								if !ok {
									continue
								}
								delete(activeTools, block.ToolUseID)
								if job.ToolEventCh != nil {
									te := ToolEvent{
										Type:      ToolCompleted,
										ToolUseID: block.ToolUseID,
										ToolName:  toolName,
									}
									if toolName == "Agent" {
										if inp, ok := activeAgents[block.ToolUseID]; ok {
											te.Summary = inp.Description
											te.SubagentType = inp.SubagentType
										}
									}
									job.ToolEventCh <- te
								}
								delete(activeAgents, block.ToolUseID)
							}
						}
					}
				}

			case "result":
				parsedResult = evt.Result
				if len(evt.Usage) > 0 {
					_ = json.Unmarshal(evt.Usage, &parsedUsage)
				}
				if evt.SessionID != "" {
					parsedSessionID = evt.SessionID
				}
				gotResult = true
			}
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			slog.Debug("claude stderr", "line", line, "workspace", job.Workspace)
			stderrBuilder.WriteString(line)
			stderrBuilder.WriteByte('\n')
		}
	}()

	wg.Wait()
	if err := cmd.Wait(); err != nil {
		if job.Ctx.Err() != nil {
			return Result{Err: fmt.Errorf("job cancelled: %w", job.Ctx.Err())}
		}
		combinedOutput := rawOutputBuilder.String() + stderrBuilder.String()
		return Result{
			Output: combinedOutput,
			Err:    fmt.Errorf("claude exited with error: %w", err),
		}
	}

	// Persist session ID (works for both new and resumed sessions)
	if parsedSessionID != "" && parsedSessionID != sessionID {
		if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, parsedSessionID); err != nil {
			slog.Warn("failed to persist session ID", "err", err)
		} else {
			slog.Info("session persisted",
				"session_id", parsedSessionID,
				"bot", job.BotName,
				"chat_id", job.ChatID,
				"topic_id", job.TopicID)
		}
	}

	if gotResult {
		totalIn := parsedUsage.InputTokens + parsedUsage.CacheReadInputTokens + parsedUsage.CacheCreateInputTokens
		return Result{Output: strings.TrimSpace(parsedResult), InputTokens: totalIn}
	}

	// Fallback: no result event parsed — return raw output
	rawOutput := strings.TrimSpace(rawOutputBuilder.String())
	if newID := extractSessionID(rawOutput); newID != "" && newID != sessionID {
		if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, newID); err != nil {
			slog.Warn("failed to persist new session ID", "err", err)
		}
	}
	return Result{Output: rawOutput}
}

// extractToolResultContent extracts readable text from a tool_result content field.
// Content can be a JSON string, array of {type,text} objects, or raw bytes.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try as string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// buildArgs assembles the claude command-line arguments based on job config.
func (m *Manager) buildArgs(job Job, sessionID string) []string {
	args := []string{}

	// Non-interactive mode must skip permission prompts; otherwise claude blocks waiting for terminal input.
	args = append(args, "--dangerously-skip-permissions")

	// Always use stream-json for real-time subagent event parsing
	args = append(args, "--output-format", "stream-json", "--verbose")

	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	if job.Model != "" {
		args = append(args, "--model", job.Model)
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
