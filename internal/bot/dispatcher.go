// Package bot implements Telegram message routing, debouncing, and foreground/background task dispatch.
// Supports Telegram Forum Topics: each topic has its own Claude session;
// topicID=0 means a regular chat (non-topic message).
package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mymmrac/telego"

	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/buildinfo"
	"github.com/lustan3216/claudeclaw/internal/runner"
	"github.com/lustan3216/claudeclaw/internal/session"
)

// chatTopicKey uniquely identifies a chat+topic for session keying.
type chatTopicKey struct {
	chatID  int64
	topicID int
}

// cancelEntry holds cancellation info for a task that can be cancelled via reaction.
type cancelEntry struct {
	cancel  context.CancelFunc
	topicID int
}

// pendingJob holds a buffered message waiting to be dispatched.
type pendingJob struct {
	replyToID int
	text      string
	mode      runner.TaskMode
}

// topicQueue is the per-topic pending buffer and running state.
type topicQueue struct {
	msgs     []pendingJob
	running  bool
	debounce *time.Timer // debounce timer for batching rapid messages
}

// debounceDelay is how long to wait for additional messages before dispatching.
// Telegram splits long pastes into multiple messages; this groups them together.
const debounceDelay = 800 * time.Millisecond

// cancelEmojis — users can cancel an in-progress task by reacting with one of these emojis.
var cancelEmojis = map[string]bool{"😱": true, "😭": true}

// toolActivity tracks a single tool call's state for display.
type toolActivity struct {
	toolUseID    string
	toolName     string // "Agent", "Read", "Bash", etc.
	summary      string // human-readable summary
	subagentType string // only for Agent
	done         bool
}

// httpClient for Whisper API calls, 60s timeout.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// downloadClient for Telegram file downloads, 120s timeout.
var downloadClient = &http.Client{Timeout: 120 * time.Second}

// Dispatcher handles message routing and task dispatch.
// Each bot instance shares one Dispatcher, distinguished by chatID+topicID.
type Dispatcher struct {
	mu sync.Mutex

	countsMu         sync.Mutex
	completionCounts map[chatTopicKey]int // chat+topic → successful completion count (triggers memory update)
	memUpdateCount   int                  // global memory update count (triggers memory.md compression)

	autoUpdateMu      sync.Mutex
	autoUpdateRunning bool // prevents multiple concurrent background updates

	cancelMu       sync.Mutex
	cancelReactions map[int]cancelEntry // trigger message ID → cancel info (for reaction cancellation)

	pendingMu    sync.Mutex
	topicPending map[chatTopicKey]*topicQueue

	summarizeMu    sync.Mutex
	summarizeActive map[chatTopicKey]bool // 防止 summarize 循環觸發

	runnerMgr  *runner.Manager
	sessionMgr *session.Manager
	cfg        *config.Config
	cfgMgr     *config.Manager
	botCfg     config.BotConfig
	botAPI     *telego.Bot
	workspace  string
}

// NewDispatcher creates a message dispatcher.
func NewDispatcher(
	botAPI *telego.Bot,
	botCfg config.BotConfig,
	cfg *config.Config,
	cfgMgr *config.Manager,
	runnerMgr *runner.Manager,
	sessionMgr *session.Manager,
	workspace string,
) *Dispatcher {
	return &Dispatcher{
		completionCounts: make(map[chatTopicKey]int),
		cancelReactions:  make(map[int]cancelEntry),
		topicPending:     make(map[chatTopicKey]*topicQueue),
		summarizeActive:  make(map[chatTopicKey]bool),
		runnerMgr:        runnerMgr,
		sessionMgr:       sessionMgr,
		cfg:              cfg,
		cfgMgr:           cfgMgr,
		botCfg:           botCfg,
		botAPI:           botAPI,
		workspace:        workspace,
	}
}

// UpdateConfig updates config on hot-reload (caller should invoke this in the config-change callback).
func (d *Dispatcher) UpdateConfig(cfg *config.Config, botCfg config.BotConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	d.botCfg = botCfg
}

// Handle receives a single message from Telegram and dispatches it immediately.
func (d *Dispatcher) Handle(ctx context.Context, update telego.Update) {
	// Handle reaction cancellation: user reacts with 😱 or 😭 to cancel an in-progress task
	if update.MessageReaction != nil {
		d.handleReactionCancel(update.MessageReaction)
		return
	}

	if update.Message == nil {
		return
	}
	msg := update.Message

	if msg.From == nil {
		return
	}

	// 无主模式：第一个发送消息的用户成为 owner
	if d.isOwnerless() {
		d.claimOwner(ctx, msg)
		return
	}

	// 权限检查：未授权用户静默丢弃，不作任何响应
	if !d.isAllowed(msg.From.ID) {
		slog.Debug("silently dropped unauthorized user", "user_id", msg.From.ID, "bot", d.botCfg.Name)
		return
	}

	// Extract topic ID: use MessageThreadID for forum topic messages, else 0
	topicID := 0
	if msg.IsTopicMessage {
		topicID = msg.MessageThreadID
	}

	// When auto_update=true, check and pull the latest version in the background on each message
	if d.cfgMgr != nil && d.cfgMgr.Get().AutoUpdate {
		go d.triggerAutoUpdate()
	}

	// Handle forum topic lifecycle events (service messages with no text content)
	if msg.ForumTopicCreated != nil {
		topicName := msg.ForumTopicCreated.Name
		threadID := msg.MessageThreadID
		slog.Info("new topic created",
			"topic_name", topicName,
			"thread_id", threadID,
			"chat_id", msg.Chat.ID)
		// Session is lazy-created when the first real message arrives
		d.reply(msg.Chat.ID, threadID, "✓ Ready — this topic has its own conversation session")
		return
	}

	if msg.ForumTopicClosed != nil {
		slog.Info("topic closed",
			"thread_id", msg.MessageThreadID,
			"chat_id", msg.Chat.ID)
		// Session is kept in storage, no other action needed
		return
	}

	if msg.ForumTopicReopened != nil {
		slog.Info("topic reopened",
			"thread_id", msg.MessageThreadID,
			"chat_id", msg.Chat.ID)
		d.reply(msg.Chat.ID, msg.MessageThreadID, "✓ Topic reopened, continuing original session")
		return
	}

	// Handle built-in commands (telego has no IsCommand/Command helper, parse manually)
	if cmd, args, ok := parseCommand(msg); ok {
		d.handleCommand(ctx, msg, topicID, cmd, args)
		return
	}

	// Handle voice messages: download ogg file then call Whisper API for transcription
	if msg.Voice != nil {
		voiceText, err := d.transcribeVoice(msg.Voice.FileID, msg.Chat.ID)
		if err != nil {
			slog.Error("voice transcription failed", "err", err, "chat_id", msg.Chat.ID)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ Voice transcription failed: %v", err))
			return
		}
		text := "[Voice transcription]: " + voiceText
		d.submitOrQueue(ctx, msg.Chat.ID, topicID, msg.MessageID, text, runner.ModeForeground)
		return
	}

	// Handle photo messages: take the highest-resolution PhotoSize, base64-encode and embed in prompt for Claude Vision
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1] // last item has the highest resolution
		savedPath, err := d.downloadTelegramFile(photo.FileID, msg.Chat.ID, "photo.jpg")
		if err != nil {
			slog.Error("failed to download photo", "err", err, "chat_id", msg.Chat.ID)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ Photo download failed: %v", err))
			return
		}
		caption := strings.TrimSpace(msg.Caption)

		// Try to read and base64-encode the image; fall back to file path mode if over 5MB
		const maxImageBytes = 5 * 1024 * 1024
		var text string
		if imgBytes, readErr := os.ReadFile(savedPath); readErr == nil && len(imgBytes) <= maxImageBytes {
			b64 := base64.StdEncoding.EncodeToString(imgBytes)
			text = fmt.Sprintf("[Image]\n<image>\n<media_type>image/jpeg</media_type>\n<data>%s</data>\n</image>", b64)
		} else {
			// File too large or read failed, fall back to path mode
			if readErr != nil {
				slog.Warn("failed to read image file, falling back to path mode", "err", readErr, "path", savedPath)
			} else {
				slog.Warn("image exceeds 5MB, falling back to path mode", "size", len(imgBytes), "path", savedPath)
			}
			text = fmt.Sprintf("[User sent an image: %s]", savedPath)
		}
		if caption != "" {
			text += "\n" + caption
		}
		d.submitOrQueue(ctx, msg.Chat.ID, topicID, msg.MessageID, text, runner.ModeForeground)
		return
	}

	// Handle document messages (PDFs, generic files, etc.)
	if msg.Document != nil {
		doc := msg.Document
		filename := doc.FileName
		if filename == "" {
			filename = doc.FileUniqueID // use unique ID when no original filename
		}
		savedPath, err := d.downloadTelegramFile(doc.FileID, msg.Chat.ID, filename)
		if err != nil {
			slog.Error("failed to download file", "err", err, "chat_id", msg.Chat.ID, "filename", filename)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ File download failed: %v", err))
			return
		}
		caption := strings.TrimSpace(msg.Caption)
		mimeType := doc.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		var text string
		if mimeType == "application/pdf" {
			text = fmt.Sprintf("[User sent a PDF file, please use the Read tool to view its contents: %s]", savedPath)
		} else {
			text = fmt.Sprintf("[User sent a file: %s (%s)]", savedPath, mimeType)
		}
		if caption != "" {
			text += "\n" + caption
		}
		d.submitOrQueue(ctx, msg.Chat.ID, topicID, msg.MessageID, text, runner.ModeForeground)
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	d.submitOrQueue(ctx, msg.Chat.ID, topicID, msg.MessageID, text, runner.ModeForeground)
}

// parseCommand detects whether a message is a bot command.
// Returns the command name (without slash), argument string, and whether it is a command.
// telego provides no IsCommand/Command helper methods; parsing is done manually via Entities.
func parseCommand(msg *telego.Message) (cmd string, args string, ok bool) {
	if !strings.HasPrefix(msg.Text, "/") {
		return "", "", false
	}
	// Confirm the first entity type is bot_command
	for _, e := range msg.Entities {
		if e.Type == telego.EntityTypeBotCommand && e.Offset == 0 {
			// Extract the command part, e.g. "/clear@botname" → "clear"
			cmdFull := msg.Text[1:e.Length] // strip leading "/"
			if at := strings.IndexByte(cmdFull, '@'); at >= 0 {
				cmdFull = cmdFull[:at]
			}
			// Arguments are the remaining text after the command (trimmed)
			rest := strings.TrimSpace(msg.Text[e.Length:])
			return cmdFull, rest, true
		}
	}
	return "", "", false
}

// handleCommand handles built-in commands: /start /help /clear /status /bg /set /unset /config /models /model.
func (d *Dispatcher) handleCommand(ctx context.Context, msg *telego.Message, topicID int, cmd string, args string) {
	chatID := msg.Chat.ID
	switch cmd {
	case "start", "help":
		d.reply(chatID, topicID, fmt.Sprintf(
			"⚡ *claudeclaw* `%s`\n\n"+
				"*Session*\n"+
				"`/clear` — reset current session\n"+
				"`/status` — show status & version\n"+
				"`/usage` — view token usage\n\n"+
				"*Tasks*\n"+
				"`/bg <task>` — run in background\n"+
				"😱 😭 react to cancel a running task\n\n"+
				"*Models*\n"+
				"`/models` — list available models\n"+
				"`/model <name>` — switch model\n\n"+
				"*Config*\n"+
				"`/config` — show current settings\n"+
				"`/set <key> <value>` — update a setting\n"+
				"`/unset <key>` — clear a setting\n"+
				"Keys: `auto_update` · `security_level`\n\n"+
				"*Admin*\n"+
				"`/adduser <id>` — authorize a user\n"+
				"`/update` — pull latest & restart",
			buildinfo.Version,
		))
	case "config":
		if d.cfgMgr == nil {
			d.reply(chatID, topicID, "❌ Config manager not initialized")
			return
		}
		cfg := d.cfgMgr.Get()
		d.reply(chatID, topicID, fmt.Sprintf(
			"Current settings:\n"+
				"  security_level = %s\n"+
				"  auto_update    = %v",
			cfg.Security.Level,
			cfg.AutoUpdate,
		))
	case "set":
		if d.cfgMgr == nil {
			d.reply(chatID, topicID, "❌ Config manager not initialized")
			return
		}
		parts := strings.SplitN(args, " ", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			d.reply(chatID, topicID, "Usage: /set <key> <value>\n\nSettable: auto_update, security_level")
			return
		}
		key, value := parts[0], parts[1]
		if err := d.cfgMgr.Set(key, value); err != nil {
			d.reply(chatID, topicID, fmt.Sprintf("❌ Set failed: %v", err))
			return
		}
		// Clear session so the next message rebuilds it with new config
		_ = d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID)
		d.reply(chatID, topicID, fmt.Sprintf("✓ %s updated, session reset", key))
	case "unset":
		if d.cfgMgr == nil {
			d.reply(chatID, topicID, "❌ Config manager not initialized")
			return
		}
		if args == "" {
			d.reply(chatID, topicID, "Usage: /unset <key>\n\nSettable: auto_update, security_level")
			return
		}
		if err := d.cfgMgr.Set(args, ""); err != nil {
			d.reply(chatID, topicID, fmt.Sprintf("❌ Clear failed: %v", err))
			return
		}
		_ = d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID)
		d.reply(chatID, topicID, fmt.Sprintf("✓ %s cleared, session reset", args))
	case "update":
		d.reply(chatID, topicID, "⏳ Pulling latest code and rebuilding...")
		go d.selfUpdate(chatID, topicID)
	case "clear":
		if err := d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID); err != nil {
			slog.Error("failed to clear session", "err", err, "chat_id", chatID, "topic_id", topicID)
			d.reply(chatID, topicID, fmt.Sprintf("❌ Failed to clear session: %v", err))
			return
		}
		d.reply(chatID, topicID, "✓ Session cleared, next message will start a new session.")
	case "status":
		topicInfo := "none (regular chat)"
		if topicID > 0 {
			topicInfo = fmt.Sprintf("Topic #%d", topicID)
		}
		model := d.botCfg.Model
		if model == "" {
			model = "default"
		}
		d.reply(chatID, topicID, fmt.Sprintf(
			"Bot: %s\nWorkspace: %s\nSecurity: %s\nTopic: %s\nModel: %s\nVersion: %s",
			d.botCfg.Name, d.workspace, d.cfg.Security.Level, topicInfo, model, buildinfo.Version,
		))
	case "bg":
		// Force background mode
		if args == "" {
			d.reply(chatID, topicID, "Usage: /bg <task description>")
			return
		}
		d.dispatchJob(ctx, chatID, topicID, msg.MessageID, args, runner.ModeBackground, nil)
	case "models":
		type modelEntry struct{ alias, full string }
		available := []modelEntry{
			{"opus", "claude-opus-4-6"},
			{"sonnet", "claude-sonnet-4-6"},
			{"haiku", "claude-haiku-4-5"},
		}
		current := d.botCfg.Model
		if current == "" {
			current = "default"
		}
		var lines []string
		for _, m := range available {
			mark := "  "
			if current == m.alias || current == m.full {
				mark = "✓ "
			}
			lines = append(lines, fmt.Sprintf("%s%-8s  %s", mark, m.alias, m.full))
		}
		if current == "default" {
			lines = append(lines, "✓ default   (claude's built-in default)")
		} else {
			lines = append(lines, "  default   (claude's built-in default)")
		}
		d.reply(chatID, topicID, "Available models:\n"+strings.Join(lines, "\n")+"\n\nUse /model <alias> or /model <full-name>")
	case "model":
		if args == "" {
			current := d.botCfg.Model
			if current == "" {
				current = "default"
			}
			d.reply(chatID, topicID, fmt.Sprintf("Current model: %s\nUsage: /model <name>", current))
			return
		}
		d.botCfg.Model = args
		_ = d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID)
		d.reply(chatID, topicID, fmt.Sprintf("✓ Model set to %s, session reset.\n(restarting bot resets to config.json value)", args))
	case "usage":
		d.reply(chatID, topicID, d.buildUsageReport())
	case "adduser":
		d.handleAddUser(ctx, msg, topicID, args)
	default:
		d.reply(chatID, topicID, "Unknown command, send /help to see available commands.")
	}
}

// buildUsageReport calculates today's token usage from ~/.claude/projects/.
// triggerAutoUpdate checks GitHub for new commits and, if found, git pulls + rebuilds → claudeclaw.new in the background.
// Uses autoUpdateRunning flag to prevent concurrency.
func (d *Dispatcher) triggerAutoUpdate() {
	d.autoUpdateMu.Lock()
	if d.autoUpdateRunning {
		d.autoUpdateMu.Unlock()
		return
	}
	d.autoUpdateRunning = true
	d.autoUpdateMu.Unlock()

	defer func() {
		d.autoUpdateMu.Lock()
		d.autoUpdateRunning = false
		d.autoUpdateMu.Unlock()
	}()

	repo := repoDir()

	// Check if remote has new commits (no pull, just fetch one commit)
	fetchCmd := exec.Command("git", "-C", repo, "fetch", "origin", "main", "--depth=1")
	fetchCmd.Env = os.Environ()
	if err := fetchCmd.Run(); err != nil {
		return
	}

	localCmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	localOut, err := localCmd.Output()
	if err != nil {
		return
	}
	remoteCmd := exec.Command("git", "-C", repo, "rev-parse", "origin/main")
	remoteOut, err := remoteCmd.Output()
	if err != nil {
		return
	}

	local := strings.TrimSpace(string(localOut))
	remote := strings.TrimSpace(string(remoteOut))
	if local == remote {
		return // already up to date
	}

	// New version available, pull and build
	slog.Info("auto_update: new version detected, building in background", "local", local[:8], "remote", remote[:8])

	pullCmd := exec.Command("git", "-C", repo, "pull", "origin", "main")
	pullCmd.Env = os.Environ()
	if err := pullCmd.Run(); err != nil {
		slog.Warn("auto_update: git pull failed", "err", err)
		return
	}

	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		gobin = "/data/go/go/bin/go"
	}
	versionCmd := exec.Command("git", "-C", repo, "describe", "--tags", "--always")
	versionOut, _ := versionCmd.Output()
	version := strings.TrimSpace(string(versionOut))
	if version == "" {
		version = "dev"
	}

	ldflags := "-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=" + version
	buildCmd := exec.Command(gobin, "build", "-ldflags", ldflags, "-o", filepath.Join(repo, "claudeclaw.new"), "./cmd/claudeclaw/")
	buildCmd.Dir = repo
	buildCmd.Env = os.Environ()
	if err := buildCmd.Run(); err != nil {
		slog.Warn("auto_update: build failed", "err", err)
		_ = os.Remove(filepath.Join(repo, "claudeclaw.new"))
		return
	}
	slog.Info("auto_update: new version ready, will take effect on next restart", "version", version)
}

// repoDir returns the directory containing the running binary (i.e. the git repo root).
// This is NOT necessarily d.workspace — the binary may live in a subdirectory.
func repoDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return filepath.Dir(exe)
	}
	return filepath.Dir(real)
}

// selfUpdate pulls latest code, rebuilds the binary, swaps it, and re-execs the process.
// No run.sh watchdog needed — the binary restarts itself.
func (d *Dispatcher) selfUpdate(chatID int64, topicID int) {
	repo := repoDir()

	// 1. git pull
	pullCmd := exec.Command("git", "-C", repo, "pull", "origin", "main")
	pullCmd.Env = os.Environ()
	if out, err := pullCmd.CombinedOutput(); err != nil {
		d.reply(chatID, topicID, fmt.Sprintf("❌ git pull failed: %s", strings.TrimSpace(string(out))))
		return
	}

	// 2. go build
	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		gobin = "/data/go/go/bin/go"
	}
	versionOut, _ := exec.Command("git", "-C", repo, "describe", "--tags", "--always").Output()
	version := strings.TrimSpace(string(versionOut))
	if version == "" {
		version = "dev"
	}

	newBin := filepath.Join(repo, "claudeclaw.new")
	ldflags := "-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=" + version
	buildCmd := exec.Command(gobin, "build", "-ldflags", ldflags, "-o", newBin, "./cmd/claudeclaw/")
	buildCmd.Dir = repo
	buildCmd.Env = os.Environ()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		d.reply(chatID, topicID, fmt.Sprintf("❌ Build failed: %s", strings.TrimSpace(string(out))))
		_ = os.Remove(newBin)
		return
	}

	// 3. swap binary
	currentBin := filepath.Join(repo, "claudeclaw")
	if err := os.Rename(newBin, currentBin); err != nil {
		d.reply(chatID, topicID, fmt.Sprintf("❌ Binary swap failed: %v", err))
		return
	}

	// 4. save restart notification so the new process sends changelog
	d.saveRestartNotify(chatID, topicID)

	slog.Info("selfUpdate: re-execing", "version", version, "repo", repo)

	// 5. re-exec: replace current process with the new binary
	if err := syscall.Exec(currentBin, os.Args, os.Environ()); err != nil {
		// If exec fails, notify and continue running the old code
		d.reply(chatID, topicID, fmt.Sprintf("❌ Restart failed: %v", err))
	}
}

func (d *Dispatcher) buildUsageReport() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "❌ Unable to read usage data"
	}

	// Convert workspace path to Claude's project key (replace / with -)
	projectKey := strings.ReplaceAll(d.workspace, "/", "-")
	projectDir := filepath.Join(homeDir, ".claude", "projects", projectKey)

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return "❌ Session records not found (" + projectDir + ")"
	}

	type usageStats struct {
		inputTokens  int64
		outputTokens int64
		cacheCreate  int64
		cacheRead    int64
		sessions     int
		messages     int
	}

	today := time.Now().UTC().Format("2006-01-02")
	todayStats := usageStats{}
	totalStats := usageStats{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		fpath := filepath.Join(projectDir, entry.Name())
		data, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}

		sessionCounted := false
		sessionTodayCounted := false

		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var record struct {
				Timestamp string `json:"timestamp"`
				Message   struct {
					Usage struct {
						InputTokens            int64 `json:"input_tokens"`
						OutputTokens           int64 `json:"output_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
						CacheReadInputTokens   int64 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				continue
			}
			u := record.Message.Usage
			if u.InputTokens == 0 && u.OutputTokens == 0 {
				continue
			}

			totalStats.inputTokens += u.InputTokens
			totalStats.outputTokens += u.OutputTokens
			totalStats.cacheCreate += u.CacheCreationInputTokens
			totalStats.cacheRead += u.CacheReadInputTokens
			totalStats.messages++
			if !sessionCounted {
				totalStats.sessions++
				sessionCounted = true
			}

			if strings.HasPrefix(record.Timestamp, today) {
				todayStats.inputTokens += u.InputTokens
				todayStats.outputTokens += u.OutputTokens
				todayStats.cacheCreate += u.CacheCreationInputTokens
				todayStats.cacheRead += u.CacheReadInputTokens
				todayStats.messages++
				if !sessionTodayCounted {
					todayStats.sessions++
					sessionTodayCounted = true
				}
			}
		}
	}

	fmtK := func(n int64) string {
		if n >= 1000 {
			return fmt.Sprintf("%.1fk", float64(n)/1000)
		}
		return fmt.Sprintf("%d", n)
	}

	return fmt.Sprintf(
		"📊 *Token Usage*\n\n"+
			"*Today (%s)*\n"+
			"```\n"+
			"Input       %s\n"+
			"Output      %s\n"+
			"Cache write %s\n"+
			"Cache hit   %s\n"+
			"Messages    %d  (%d sessions)\n"+
			"```\n\n"+
			"*All time*\n"+
			"```\n"+
			"Input       %s\n"+
			"Output      %s\n"+
			"Cache write %s\n"+
			"Cache hit   %s\n"+
			"Messages    %d  (%d sessions)\n"+
			"```\n\n"+
			"Check your credit balance at: console.anthropic.com",
		today,
		fmtK(todayStats.inputTokens), fmtK(todayStats.outputTokens),
		fmtK(todayStats.cacheCreate), fmtK(todayStats.cacheRead),
		todayStats.messages, todayStats.sessions,
		fmtK(totalStats.inputTokens), fmtK(totalStats.outputTokens),
		fmtK(totalStats.cacheCreate), fmtK(totalStats.cacheRead),
		totalStats.messages, totalStats.sessions,
	)
}

// submitOrQueue buffers the message for the given chat+topic.
// If no job is running, flushes the buffer immediately as a batch.
// If a job is running, the message waits; runBatch will pick it up when done.
func (d *Dispatcher) submitOrQueue(ctx context.Context, chatID int64, topicID, replyToID int, text string, mode runner.TaskMode) {
	key := chatTopicKey{chatID, topicID}
	d.pendingMu.Lock()
	tq := d.topicPending[key]
	if tq == nil {
		tq = &topicQueue{}
		d.topicPending[key] = tq
	}
	tq.msgs = append(tq.msgs, pendingJob{replyToID, text, mode})
	if tq.running {
		// 任务正在执行，消息排队等任务完成后合并
		d.pendingMu.Unlock()
		return
	}
	// 没有任务在跑，用 debounce 等待后续消息（Telegram 拆分长消息）
	if tq.debounce != nil {
		tq.debounce.Stop()
	}
	tq.debounce = time.AfterFunc(debounceDelay, func() {
		d.pendingMu.Lock()
		tq := d.topicPending[key]
		if tq == nil || len(tq.msgs) == 0 {
			d.pendingMu.Unlock()
			return
		}
		tq.running = true
		tq.debounce = nil
		batch := tq.msgs
		tq.msgs = nil
		d.pendingMu.Unlock()
		d.runBatch(ctx, chatID, topicID, batch)
	})
	d.pendingMu.Unlock()
}

// runBatch dispatches a batch of pending messages as a single combined job.
// When done, checks for more pending messages and continues if any.
func (d *Dispatcher) runBatch(ctx context.Context, chatID int64, topicID int, batch []pendingJob) {
	key := chatTopicKey{chatID, topicID}

	// React 👀 on all messages except the last (dispatchJob handles the last one)
	for i := 0; i < len(batch)-1; i++ {
		d.react(chatID, batch[i].replyToID, "👀")
	}

	// Combine all texts; use last message's replyToID and mode
	var texts []string
	for _, j := range batch {
		texts = append(texts, j.text)
	}
	last := batch[len(batch)-1]

	d.dispatchJob(ctx, chatID, topicID, last.replyToID, strings.Join(texts, "\n"), last.mode, func() {
		d.pendingMu.Lock()
		tq := d.topicPending[key]
		if tq == nil || len(tq.msgs) == 0 {
			if tq != nil {
				tq.running = false
			}
			d.pendingMu.Unlock()
			return
		}
		next := tq.msgs
		tq.msgs = nil
		d.pendingMu.Unlock()
		go d.runBatch(ctx, chatID, topicID, next)
	})
}

// drainPending clears and discards all pending messages for a topic and marks it as not running.
// Called on cancellation so queued messages are not auto-dispatched after a cancel.
func (d *Dispatcher) drainPending(chatID int64, topicID int) {
	key := chatTopicKey{chatID, topicID}
	d.pendingMu.Lock()
	if tq := d.topicPending[key]; tq != nil {
		if tq.debounce != nil {
			tq.debounce.Stop()
			tq.debounce = nil
		}
		tq.msgs = nil
		tq.running = false
	}
	d.pendingMu.Unlock()
}

// dispatchJob submits a job to the runner and handles Telegram replies.
// replyToID is the ID of the last message that triggered this job; it is quoted in the reply.
// onDone is called when the job completes successfully; pass nil if not needed.
func (d *Dispatcher) dispatchJob(ctx context.Context, chatID int64, topicID int, replyToID int, prompt string, mode runner.TaskMode, onDone func()) {
	// React with 👀 on receipt
	d.react(chatID, replyToID, "👀")

	// Create an independent cancel and register it in cancelReactions for reaction cancellation (😱/😭)
	jobCtx, jobCancel := context.WithCancel(ctx)
	d.cancelMu.Lock()
	d.cancelReactions[replyToID] = cancelEntry{cancel: jobCancel, topicID: topicID}
	d.cancelMu.Unlock()
	cleanup := func() {
		d.cancelMu.Lock()
		delete(d.cancelReactions, replyToID)
		d.cancelMu.Unlock()
		jobCancel()
	}

	// Create tool event channel for real-time progress visibility
	toolEventCh := make(chan runner.ToolEvent, 32)

	// For background jobs, send status message upfront; for foreground, send on first tool event
	var statusMsgID int
	if mode == runner.ModeBackground {
		statusMsgID = d.replyTo(chatID, topicID, replyToID, "⏳ Processing in the background...")
	}

	// Start tool event listener goroutine — edits the status message with live progress
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		var activities []toolActivity
		var lastText string
		for evt := range toolEventCh {
			slog.Debug("tool event received in dispatcher",
				"type", evt.Type,
				"tool", evt.ToolName,
				"summary", evt.Summary,
				"tool_use_id", evt.ToolUseID)
			switch evt.Type {
			case runner.ToolStarted:
				activities = append(activities, toolActivity{
					toolUseID:    evt.ToolUseID,
					toolName:     evt.ToolName,
					summary:      evt.Summary,
					subagentType: evt.SubagentType,
				})
			case runner.ToolCompleted:
				for i := range activities {
					if activities[i].toolUseID == evt.ToolUseID {
						activities[i].done = true
						break
					}
				}
			}
			// 构建进度文本
			text := d.buildProgressText(activities, mode == runner.ModeBackground)
			if text == lastText {
				continue // 避免重复编辑相同内容
			}
			lastText = text
			slog.Debug("updating progress message",
				"status_msg_id", statusMsgID,
				"activities", len(activities),
				"text_len", len(text))
			if statusMsgID == 0 {
				// 前台任务：首次工具事件时发送新消息
				statusMsgID = d.replyTo(chatID, topicID, replyToID, text)
				slog.Debug("sent new progress message", "msg_id", statusMsgID)
			} else {
				// 编辑已有的状态消息
				_, err := d.botAPI.EditMessageText(&telego.EditMessageTextParams{
					ChatID:    telego.ChatID{ID: chatID},
					MessageID: statusMsgID,
					Text:      text,
					ParseMode: telego.ModeMarkdown,
				})
				if err != nil {
					slog.Warn("failed to edit progress message", "err", err, "msg_id", statusMsgID)
				}
			}
		}
	}()

	// Background job: execute asynchronously (status message already sent above)
	if mode == runner.ModeBackground {

		resultCh := make(chan runner.Result, 1)
		d.runnerMgr.Submit(runner.Job{
			Ctx:               jobCtx,
			Workspace:         d.workspace,
			BotName:           d.botCfg.Name,
			ChatID:            chatID,
			TopicID:           topicID,
			Prompt:            prompt,
			Mode:              mode,
			AnthropicAPIKeys:  d.botCfg.AnthropicAPIKeys,
			ClaudeCredentials: d.botCfg.ClaudeCredentials,
			Model:             d.botCfg.Model,
			ResultCh:          resultCh,
			ToolEventCh:       toolEventCh,
		})

		go func() {
			defer cleanup()
			result := <-resultCh
			<-agentDone // wait for all agent events to be processed
			if result.Err != nil {
				if jobCtx.Err() != nil {
					d.drainPending(chatID, topicID)
					return
				}
				d.replyTo(chatID, topicID, replyToID, fmt.Sprintf("❌ Background job failed: %v", result.Err))
				return
			}
			d.react(chatID, replyToID, "✅")
			d.sendOutputTo(chatID, topicID, replyToID, result.Output)
			d.maybeUpdateMemory(ctx, chatID, topicID)
			d.maybeSummarizeSession(ctx, chatID, topicID, result.InputTokens)
			if onDone != nil {
				onDone()
			}
		}()
		return
	}

	// Foreground: call onDone only on non-cancelled completion.
	// userCancelled is set to true only when the user explicitly cancels (reaction 😱/😭),
	// NOT when cleanup() calls jobCancel() at the end of a normal run.
	var userCancelled bool
	if onDone != nil {
		defer func() {
			if userCancelled {
				// User explicitly cancelled — discard pending messages and release the queue
				d.drainPending(chatID, topicID)
				return
			}
			onDone()
		}()
	}

	// Foreground job: keep sending typing action until complete, then reply with result
	// Send the first typing immediately so the user sees feedback right away
	firstTypingParams := &telego.SendChatActionParams{
		ChatID: telego.ChatID{ID: chatID},
		Action: telego.ChatActionTyping,
	}
	if topicID > 0 {
		firstTypingParams.MessageThreadID = topicID
	}
	if err := d.botAPI.SendChatAction(firstTypingParams); err != nil {
		slog.Warn("SendChatAction failed", "err", err, "chat_id", chatID, "topic_id", topicID)
	}

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:               jobCtx,
		Workspace:         d.workspace,
		BotName:           d.botCfg.Name,
		ChatID:            chatID,
		TopicID:           topicID,
		Prompt:            prompt,
		Mode:              mode,
		AnthropicAPIKeys:  d.botCfg.AnthropicAPIKeys,
		ClaudeCredentials: d.botCfg.ClaudeCredentials,
		Model:             d.botCfg.Model,
		ResultCh:          resultCh,
		ToolEventCh:       toolEventCh,
	})

	// Renew typing every 3s until the result is ready
	// Telegram's typing indicator expires in ~5s, 3s gives 2s of margin against network jitter
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingDone:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				params := &telego.SendChatActionParams{
					ChatID: telego.ChatID{ID: chatID},
					Action: telego.ChatActionTyping,
				}
				if topicID > 0 {
					params.MessageThreadID = topicID
				}
				if err := d.botAPI.SendChatAction(params); err != nil {
					slog.Warn("failed to renew typing", "err", err, "chat_id", chatID, "topic_id", topicID)
				}
			}
		}
	}()

	result := <-resultCh
	close(typingDone)
	<-agentDone // wait for all agent events to be processed

	// Check cancellation BEFORE calling cleanup (which itself calls jobCancel).
	// If already cancelled (user reacted with 😱/😭), handleReactionCancel already sent a reply.
	if jobCtx.Err() != nil {
		userCancelled = true
		cleanup()
		return
	}
	cleanup()

	if result.Err != nil {
		d.replyTo(chatID, topicID, replyToID, fmt.Sprintf("❌ Execution failed: %v", result.Err))
		return
	}
	d.react(chatID, replyToID, "✅")
	d.sendOutputTo(chatID, topicID, replyToID, result.Output)
	d.maybeUpdateMemory(ctx, chatID, topicID)
	d.maybeSummarizeSession(ctx, chatID, topicID, result.InputTokens)
}

// toolIcon returns an emoji icon for the given tool name.
func toolIcon(name string) string {
	switch name {
	case "Agent":
		return "🤖"
	case "Read":
		return "📖"
	case "Write":
		return "📝"
	case "Edit":
		return "✏️"
	case "Bash":
		return "🖥️"
	case "Grep":
		return "🔍"
	case "Glob":
		return "📂"
	case "Skill":
		return "⚡"
	case "WebSearch", "WebFetch":
		return "🌐"
	case "TodoWrite":
		return "📋"
	default:
		return "🔧"
	}
}

// buildProgressText builds a status message showing live tool call progress.
// Shows Agent calls as prominent items, other tools as a compact activity log.
func (d *Dispatcher) buildProgressText(activities []toolActivity, isBackground bool) string {
	var b strings.Builder
	if isBackground {
		b.WriteString("⏳ Processing in the background...\n\n")
	}

	// 只显示最近 8 条活动，避免消息过长
	start := 0
	if len(activities) > 8 {
		start = len(activities) - 8
	}

	for _, a := range activities[start:] {
		icon := toolIcon(a.toolName)
		if a.done {
			icon = "✅"
		}

		if a.toolName == "Agent" {
			// Agent 行显示完整信息
			agentType := a.subagentType
			if agentType == "" {
				agentType = "general"
			}
			b.WriteString(fmt.Sprintf("%s %s _(%s)_\n", icon, escapeMarkdown(a.summary), escapeMarkdown(agentType)))
		} else {
			// 其他工具紧凑显示
			summary := a.summary
			if summary != "" {
				b.WriteString(fmt.Sprintf("%s `%s` %s\n", icon, a.toolName, escapeMarkdown(summary)))
			} else {
				b.WriteString(fmt.Sprintf("%s `%s`\n", icon, a.toolName))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// escapeMarkdown escapes special Markdown characters for Telegram MarkdownV1.
func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"`", "\\`",
		"[", "\\[",
	)
	return replacer.Replace(s)
}

// maybeUpdateMemory silently triggers a memory.md update every N successful completions.
// Sends no message to the user; results are only logged.
func (d *Dispatcher) maybeUpdateMemory(ctx context.Context, chatID int64, topicID int) {
	interval := d.botCfg.MemoryUpdateInterval
	if interval <= 0 {
		return
	}

	key := chatTopicKey{chatID, topicID}
	d.countsMu.Lock()
	d.completionCounts[key]++
	count := d.completionCounts[key]
	d.countsMu.Unlock()

	if count%interval != 0 {
		return
	}

	slog.Info("triggering memory update", "chat_id", chatID, "topic_id", topicID, "count", count)

	prompt := "Based on the conversation above, do two things silently:\n\n" +
		"1. Update .claudeclaw/memory.md — use section markers with relevance tags:\n" +
		"   <!-- section: global tags: always -->\n" +
		"   ## Global Preferences\n" +
		"   (keep under 200 words total for global section)\n\n" +
		"   <!-- section: topic tags: tag1,tag2 -->\n" +
		"   ## Topic\n" +
		"   (keep each section under 200 words)\n\n" +
		"2. Check for behavioral patterns: if you've noticed a consistent preference or habit in this conversation " +
		"that isn't already in .claudeclaw/preferences.md, add ONE short rule (1 sentence max) to .claudeclaw/preferences.md. " +
		"Only add if you're confident — skip if uncertain. The preferences file is for permanent behavior rules, not facts.\n\n" +
		"Do not reply after completing."

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:            prompt,
		Mode:              runner.ModeForeground,
		AnthropicAPIKeys:  d.botCfg.AnthropicAPIKeys,
		ClaudeCredentials: d.botCfg.ClaudeCredentials,
		ResultCh:          resultCh,
	})

	// Discard result, log only; on success check if memory.md needs compression
	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("memory update failed", "err", result.Err, "chat_id", chatID, "topic_id", topicID)
			return
		}
		slog.Info("memory update complete", "chat_id", chatID, "topic_id", topicID)
		d.maybeCompressMemory(ctx, chatID, topicID)
	}()
}

// maybeCompressMemory silently compresses memory.md every N memory updates, deduplicating and trimming it.
func (d *Dispatcher) maybeCompressMemory(ctx context.Context, chatID int64, topicID int) {
	interval := d.botCfg.MemoryCompressInterval
	if interval <= 0 {
		return
	}

	d.countsMu.Lock()
	d.memUpdateCount++
	count := d.memUpdateCount
	d.countsMu.Unlock()

	if count%interval != 0 {
		return
	}

	slog.Info("triggering memory.md compression", "count", count)

	prompt := "Compress .claudeclaw/memory.md in the working directory:\n" +
		"1. Keep the file under 3000 bytes total.\n" +
		"2. Remove duplicates, merge similar entries, delete outdated facts.\n" +
		"3. If the file is still over 3000 bytes after deduplication, move the oldest or least-relevant non-global sections to .claudeclaw/vault/" + currentYearMonth() + ".md (append).\n" +
		"4. Overwrite memory.md with the trimmed result.\n" +
		"Do not reply after completing."

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:            prompt,
		Mode:              runner.ModeForeground,
		AnthropicAPIKeys:  d.botCfg.AnthropicAPIKeys,
		ClaudeCredentials: d.botCfg.ClaudeCredentials,
		ResultCh:          resultCh,
	})

	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("memory.md compression failed", "err", result.Err)
		} else {
			slog.Info("memory.md compression complete")
		}
	}()
}

// maybeSummarizeSession summarizes the conversation into memory.md and resets the session
// when input tokens exceed max_session_tokens (default 60000).
// The next conversation starts from a fresh session; continuity is maintained via memory.md injection.
func (d *Dispatcher) maybeSummarizeSession(ctx context.Context, chatID int64, topicID int, inputTokens int) {
	if inputTokens <= 0 {
		return
	}
	maxTokens := d.botCfg.MaxSessionTokens
	if maxTokens == 0 {
		maxTokens = 60000
	}
	if inputTokens < maxTokens {
		return
	}

	// 防止 summarize 循環：summarize 自身的回應也會觸發 token 檢查，
	// 如果不擋住，會無限 summarize→check→summarize
	key := chatTopicKey{chatID: chatID, topicID: topicID}
	d.summarizeMu.Lock()
	if d.summarizeActive[key] {
		d.summarizeMu.Unlock()
		slog.Info("summarize already in progress, skipping",
			"chat_id", chatID, "topic_id", topicID)
		return
	}
	d.summarizeActive[key] = true
	d.summarizeMu.Unlock()

	slog.Info("session token threshold exceeded, triggering summarize+reset",
		"input_tokens", inputTokens, "max_session_tokens", maxTokens,
		"chat_id", chatID, "topic_id", topicID)

	prompt := "Write a structured session brief to .claudeclaw/memory.md.\n" +
		"1. Find the current '## Session Brief' section (tagged 'session-brief'). If it exists, FIRST append its content to .claudeclaw/vault/" + currentYearMonth() + ".md (creating the file if needed, appending if it already exists).\n" +
		"2. Replace the section with a new brief in this exact format (under 150 words total):\n\n" +
		"<!-- section: session-brief tags: always -->\n" +
		"## Session Brief\n" +
		"**Date:** " + time.Now().UTC().Format("2006-01-02") + "\n" +
		"**Done:** (one line — what was completed today)\n" +
		"**Decided:** (one line — key decisions made)\n" +
		"**Pending:** (one line — open items)\n\n" +
		"Do not reply after completing."

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:            prompt,
		Mode:              runner.ModeForeground,
		AnthropicAPIKeys:  d.botCfg.AnthropicAPIKeys,
		ClaudeCredentials: d.botCfg.ClaudeCredentials,
		ResultCh:          resultCh,
	})

	go func() {
		defer func() {
			d.summarizeMu.Lock()
			delete(d.summarizeActive, key)
			d.summarizeMu.Unlock()
		}()

		result := <-resultCh
		if result.Err != nil {
			slog.Warn("conversation summarize failed, keeping original session", "err", result.Err, "chat_id", chatID, "topic_id", topicID)
			return
		}
		// After successful summarize, clear session so next conversation starts fresh (memory.md has the summary)
		if err := d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID); err != nil {
			slog.Warn("failed to clear session", "err", err)
		} else {
			slog.Info("conversation summarized, session reset", "chat_id", chatID, "topic_id", topicID)
		}
	}()
}

// sendOutputTo handles long output: the first chunk quotes the trigger message; subsequent chunks are sent directly.
func (d *Dispatcher) sendOutputTo(chatID int64, topicID int, replyToID int, output string) {
	if output == "" {
		d.replyTo(chatID, topicID, replyToID, "✓ Done (no output)")
		return
	}

	const maxLen = 4000
	runes := []rune(output)
	first := true

	for len(runes) > 0 {
		chunk := runes
		if len(chunk) > maxLen {
			chunk = runes[:maxLen]
			runes = runes[maxLen:]
		} else {
			runes = nil
		}
		if first {
			d.replyTo(chatID, topicID, replyToID, string(chunk))
			first = false
		} else {
			d.reply(chatID, topicID, string(chunk))
		}
	}
}

// replyTo replies to a specific message (quote); falls back to a plain send if replyToID <= 0.
func (d *Dispatcher) replyTo(chatID int64, topicID int, replyToID int, text string) int {
	params := &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		Text:      text,
		ParseMode: telego.ModeMarkdown,
	}
	if topicID > 0 {
		params.MessageThreadID = topicID
	}
	if replyToID > 0 {
		params.ReplyParameters = &telego.ReplyParameters{MessageID: replyToID}
	}
	sent, err := d.botAPI.SendMessage(params)
	if err != nil {
		// Markdown parse failure: fall back to plain text and retry
		params.ParseMode = ""
		sent, err = d.botAPI.SendMessage(params)
		if err != nil {
			slog.Error("failed to send Telegram message",
				"chat_id", chatID, "topic_id", topicID, "err", err, "bot", d.botCfg.Name)
			return 0
		}
	}
	if sent != nil {
		return sent.MessageID
	}
	return 0
}

// reply sends a text message to the specified chat (optionally a topic) without quoting any message.
func (d *Dispatcher) reply(chatID int64, topicID int, text string) {
	d.replyTo(chatID, topicID, 0, text)
}

// handleReactionCancel checks newly added user reactions: if it is a cancel emoji (😱/😭), cancels the corresponding task.
func (d *Dispatcher) handleReactionCancel(r *telego.MessageReactionUpdated) {
	for _, reaction := range r.NewReaction {
		emoji, ok := reaction.(*telego.ReactionTypeEmoji)
		if !ok || !cancelEmojis[emoji.Emoji] {
			continue
		}
		d.cancelMu.Lock()
		entry, found := d.cancelReactions[r.MessageID]
		if found {
			delete(d.cancelReactions, r.MessageID)
		}
		d.cancelMu.Unlock()

		if found {
			entry.cancel()
			// Remove 👀, replace with 🛑 to indicate cancellation
			_ = d.botAPI.SetMessageReaction(&telego.SetMessageReactionParams{
				ChatID:    telego.ChatID{ID: r.Chat.ID},
				MessageID: r.MessageID,
				Reaction:  []telego.ReactionType{},
			})
			d.reply(r.Chat.ID, entry.topicID, "🛑 Cancelled")
		}
		return
	}
}

// react adds an emoji reaction to a message; errors are only logged and don't affect the main flow.
func (d *Dispatcher) react(chatID int64, messageID int, emoji string) {
	_ = d.botAPI.SetMessageReaction(&telego.SetMessageReactionParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: messageID,
		Reaction:  []telego.ReactionType{&telego.ReactionTypeEmoji{Type: "emoji", Emoji: emoji}},
	})
}

// isAllowed checks whether a user is in the whitelist.
func (d *Dispatcher) isAllowed(userID int64) bool {
	d.mu.Lock()
	allowed := d.botCfg.AllowedUsers
	d.mu.Unlock()

	for _, id := range allowed {
		if id == userID {
			return true
		}
	}
	return false
}

// isOwnerless returns true when allowed_users is empty, meaning the bot is awaiting its first owner.
// This is checked under the dispatcher lock to be concurrency-safe.
func (d *Dispatcher) isOwnerless() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.botCfg.AllowedUsers) == 0
}

// claimOwner persists the sender as the bot owner and sends a Telegram-guided setup flow.
// Called only while the bot is in ownerless mode; subsequent reloads via fsnotify or the in-memory
// update in ClaimOwner will flip isOwnerless() to false, so this path is only exercised once per bot.
func (d *Dispatcher) claimOwner(ctx context.Context, msg *telego.Message) {
	userID := msg.From.ID
	topicID := 0
	if msg.IsTopicMessage {
		topicID = msg.MessageThreadID
	}

	if d.cfgMgr == nil {
		return
	}
	if err := d.cfgMgr.ClaimOwner(d.botCfg.Name, userID); err != nil {
		slog.Error("failed to claim owner", "err", err, "user_id", userID, "bot", d.botCfg.Name)
		return
	}

	slog.Info("owner claimed", "user_id", userID, "bot", d.botCfg.Name)

	cfg := d.cfgMgr.Get()
	secLevel := cfg.Security.Level
	workspace := d.workspace

	d.reply(msg.Chat.ID, topicID, fmt.Sprintf(
		"⚡ *Welcome! You're the owner of this bot.*\n\n"+
			"Let's finish setup — everything can be configured right here in Telegram.\n\n"+
			"*📁 Workspace* (currently: `%s`)\n"+
			"This is the directory Claude Code has access to.\n"+
			"Change it by editing `config.json` and restarting.\n\n"+
			"*🔒 Security* (currently: `%s`)\n"+
			"`moderate` — auto-approves most ops _(recommended)_\n"+
			"`strict` — confirms every tool call\n"+
			"`unrestricted` — no prompts at all\n"+
			"→ `/set security_level strict`\n\n"+
			"*👥 Add more users*\n"+
			"`/adduser <telegram_id>`\n\n"+
			"All set? Just send me a message to get started. /help for all commands.",
		workspace, secLevel,
	))
}

// handleAddUser processes the /adduser command, adding a new Telegram user ID to allowed_users.
func (d *Dispatcher) handleAddUser(ctx context.Context, msg *telego.Message, topicID int, args string) {
	chatID := msg.Chat.ID
	args = strings.TrimSpace(args)
	if args == "" {
		d.reply(chatID, topicID, "Usage: `/adduser <telegram_user_id>`\nTip: ask them to message @userinfobot to get their ID.")
		return
	}
	newID, err := strconv.ParseInt(args, 10, 64)
	if err != nil || newID <= 0 {
		d.reply(chatID, topicID, "❌ Invalid user ID. Must be a number, e.g. `/adduser 123456789`")
		return
	}
	if d.cfgMgr == nil {
		d.reply(chatID, topicID, "❌ Config manager unavailable")
		return
	}
	if err := d.cfgMgr.ClaimOwner(d.botCfg.Name, newID); err != nil {
		d.reply(chatID, topicID, fmt.Sprintf("❌ Failed to add user: %v", err))
		return
	}
	d.reply(chatID, topicID, fmt.Sprintf("✓ User `%d` added.", newID))
}

// openAIAPIKey returns a valid OpenAI API key: prefers BotConfig field, then falls back to env var.
func (d *Dispatcher) openAIAPIKey() string {
	if k := d.botCfg.OpenAIAPIKey; k != "" {
		return k
	}
	return os.Getenv("OPENAI_API_KEY")
}

// transcribeVoice downloads a Telegram voice file (ogg) and transcribes it to text via the Whisper API.
func (d *Dispatcher) transcribeVoice(fileID string, chatID int64) (string, error) {
	apiKey := d.openAIAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("OpenAI API key not configured (openai_api_key or OPENAI_API_KEY)")
	}

	// Download ogg file to inbox
	savedPath, err := d.downloadTelegramFile(fileID, chatID, "voice.ogg")
	if err != nil {
		return "", fmt.Errorf("failed to download voice file: %w", err)
	}

	// Read file contents
	audioBytes, err := os.ReadFile(savedPath)
	if err != nil {
		return "", fmt.Errorf("failed to read voice file: %w", err)
	}

	// Build multipart request body
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// model field
	if err := mw.WriteField("model", "whisper-1"); err != nil {
		return "", fmt.Errorf("failed to write multipart model field: %w", err)
	}

	// file field
	fw, err := mw.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return "", fmt.Errorf("failed to create multipart file field: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return "", fmt.Errorf("failed to write audio data: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Send request to Whisper API
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Whisper API call failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read Whisper response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Whisper API returned error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response: {"text": "..."}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse Whisper response: %w", err)
	}

	slog.Info("voice transcription complete",
		"chat_id", chatID,
		"chars", len(result.Text),
		"bot", d.botCfg.Name)

	return result.Text, nil
}

// downloadTelegramFile downloads a file via the Telegram Bot API,
// saves it to {workspace}/.claudeclaw/inbox/{chatID}/{filename},
// and returns the local absolute path.
func (d *Dispatcher) downloadTelegramFile(fileID string, chatID int64, filename string) (string, error) {
	// Step 1: query Telegram for the file path
	file, err := d.botAPI.GetFile(&telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}
	if file.FilePath == "" {
		return "", fmt.Errorf("Telegram returned empty file path")
	}

	// Build download URL
	downloadURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", d.botCfg.Token, file.FilePath)

	// Step 2: create directory {workspace}/.claudeclaw/inbox/{chatID}/
	inboxDir := filepath.Join(d.workspace, ".claudeclaw", "inbox", fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create inbox directory: %w", err)
	}

	// Target path: add timestamp prefix if file already exists to avoid overwriting
	destPath := filepath.Join(inboxDir, filename)
	if _, statErr := os.Stat(destPath); statErr == nil {
		// File already exists, add timestamp prefix
		ts := time.Now().Format("20060102_150405_")
		destPath = filepath.Join(inboxDir, ts+filename)
	}

	// Step 3: HTTP download
	resp, err := downloadClient.Get(downloadURL)
	if err != nil {
		return "", fmt.Errorf("HTTP download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Telegram returned non-200 status: %d", resp.StatusCode)
	}

	// Write to local file
	f, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create local file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	slog.Info("file downloaded",
		"chat_id", chatID,
		"filename", filename,
		"dest", destPath,
		"bot", d.botCfg.Name)

	return destPath, nil
}

// restartNotifyPath returns the path to the restart notification file.
func (d *Dispatcher) restartNotifyPath() string {
	return filepath.Join(d.workspace, ".claudeclaw", "restart_notify.json")
}

// saveRestartNotify saves the notification target before exiting so that the changelog can be sent after restart.
func (d *Dispatcher) saveRestartNotify(chatID int64, topicID int) {
	type notifData struct {
		ChatID    int64  `json:"chat_id"`
		TopicID   int    `json:"topic_id"`
		OldCommit string `json:"old_commit"`
	}
	out, _ := exec.Command("git", "-C", repoDir(), "rev-parse", "--short", "HEAD").Output()
	data, _ := json.Marshal(notifData{
		ChatID:    chatID,
		TopicID:   topicID,
		OldCommit: strings.TrimSpace(string(out)),
	})
	_ = os.MkdirAll(filepath.Join(d.workspace, ".claudeclaw"), 0o755)
	_ = os.WriteFile(d.restartNotifyPath(), data, 0o644)
}

// currentYearMonth returns the current UTC year-month string for vault file naming.
func currentYearMonth() string {
	return time.Now().UTC().Format("2006-01")
}


