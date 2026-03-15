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
	msgs    []pendingJob
	running bool
}

// cancelEmojis — users can cancel an in-progress task by reacting with one of these emojis.
var cancelEmojis = map[string]bool{"😱": true, "😭": true}

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

// handleCommand handles built-in commands: /start /help /clear /status /bg /set /unset /config.
func (d *Dispatcher) handleCommand(ctx context.Context, msg *telego.Message, topicID int, cmd string, args string) {
	chatID := msg.Chat.ID
	switch cmd {
	case "start", "help":
		d.reply(chatID, topicID, fmt.Sprintf(
			"⚡ *claudeclaw* `%s`\n\n"+
				"/clear · /bg `<task>` · /status · /usage\n"+
				"/adduser `<id>` · /update · /config\n"+
				"😱 😭 react to cancel\n\n"+
				"*Config keys*\n"+
				"`auto_update` `security_level`\n\n"+
				"`/set <key> <value>` · `/unset <key>`",
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
		// Save notification info; send to the triggering chat after restart
		d.saveRestartNotify(chatID, topicID)
		d.reply(chatID, topicID, "⏳ Restarting and pulling latest version, please wait...")
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0) // watchdog (run.sh) will auto git pull + rebuild + restart
		}()
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
		d.reply(chatID, topicID, fmt.Sprintf(
			"Bot: %s\nWorkspace: %s\nSecurity: %s\nTopic: %s",
			d.botCfg.Name, d.workspace, d.cfg.Security.Level, topicInfo,
		))
	case "bg":
		// Force background mode
		if args == "" {
			d.reply(chatID, topicID, "Usage: /bg <task description>")
			return
		}
		d.dispatchJob(ctx, chatID, topicID, msg.MessageID, args, runner.ModeBackground, nil)
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

	// Check if remote has new commits (no pull, just fetch one commit)
	fetchCmd := exec.Command("git", "-C", d.workspace, "fetch", "origin", "main", "--depth=1")
	fetchCmd.Env = os.Environ()
	if err := fetchCmd.Run(); err != nil {
		return
	}

	localCmd := exec.Command("git", "-C", d.workspace, "rev-parse", "HEAD")
	localOut, err := localCmd.Output()
	if err != nil {
		return
	}
	remoteCmd := exec.Command("git", "-C", d.workspace, "rev-parse", "origin/main")
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

	pullCmd := exec.Command("git", "-C", d.workspace, "pull", "origin", "main")
	pullCmd.Env = os.Environ()
	if err := pullCmd.Run(); err != nil {
		slog.Warn("auto_update: git pull failed", "err", err)
		return
	}

	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		gobin = "/data/go/go/bin/go"
	}
	versionCmd := exec.Command("git", "-C", d.workspace, "describe", "--tags", "--always")
	versionOut, _ := versionCmd.Output()
	version := strings.TrimSpace(string(versionOut))
	if version == "" {
		version = "dev"
	}

	ldflags := "-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=" + version
	buildCmd := exec.Command(gobin, "build", "-ldflags", ldflags, "-o", filepath.Join(d.workspace, "claudeclaw.new"), "./cmd/claudeclaw/")
	buildCmd.Dir = d.workspace
	buildCmd.Env = os.Environ()
	if err := buildCmd.Run(); err != nil {
		slog.Warn("auto_update: build failed", "err", err)
		_ = os.Remove(filepath.Join(d.workspace, "claudeclaw.new"))
		return
	}
	slog.Info("auto_update: new version ready, will take effect on next restart", "version", version)
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
		d.pendingMu.Unlock()
		return
	}
	tq.running = true
	batch := tq.msgs
	tq.msgs = nil
	d.pendingMu.Unlock()
	go d.runBatch(ctx, chatID, topicID, batch)
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

	// Background job: reply immediately to user, execute asynchronously
	if mode == runner.ModeBackground {
		d.replyTo(chatID, topicID, replyToID, "⏳ Processing in the background, will notify you when done.")

		resultCh := make(chan runner.Result, 1)
		d.runnerMgr.Submit(runner.Job{
			Ctx:       jobCtx,
			Workspace: d.workspace,
			BotName:   d.botCfg.Name,
			ChatID:    chatID,
			TopicID:   topicID,
			Prompt:    prompt,
			Mode:      mode,
			ResultCh:  resultCh,
		})

		go func() {
			defer cleanup()
			result := <-resultCh
			if result.Err != nil {
				if jobCtx.Err() != nil {
					// Cancelled — discard any pending messages for this topic and release the queue
					d.drainPending(chatID, topicID)
					return // reply already sent by handleReactionCancel
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

	// Foreground: call onDone only on non-cancelled completion
	if onDone != nil {
		defer func() {
			if jobCtx.Err() != nil {
				// Cancelled — discard pending messages and release the queue
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
		Ctx:       jobCtx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:    prompt,
		Mode:      mode,
		ResultCh:  resultCh,
	})

	// Renew typing every 4s until the result is ready
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
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

	// Check cancellation BEFORE calling cleanup (which itself calls jobCancel).
	// If already cancelled (user reacted with 😱/😭), handleReactionCancel already sent a reply.
	if jobCtx.Err() != nil {
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
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
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
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
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
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
	})

	go func() {
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
func (d *Dispatcher) replyTo(chatID int64, topicID int, replyToID int, text string) {
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
	if _, err := d.botAPI.SendMessage(params); err != nil {
		// Markdown parse failure: fall back to plain text and retry
		params.ParseMode = ""
		if _, err2 := d.botAPI.SendMessage(params); err2 != nil {
			slog.Error("failed to send Telegram message",
				"chat_id", chatID, "topic_id", topicID, "err", err2, "bot", d.botCfg.Name)
		}
	}
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
	out, _ := exec.Command("git", "-C", d.workspace, "rev-parse", "--short", "HEAD").Output()
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

