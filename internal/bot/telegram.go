// Package bot implements the long-polling goroutine for each Telegram Bot.
// Each bot runs independently, sharing runner.Manager and session.Manager.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"github.com/lustan3216/claudeclaw/internal/buildinfo"
	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/runner"
	"github.com/lustan3216/claudeclaw/internal/session"
)

// Bot encapsulates the lifecycle of a single Telegram bot.
type Bot struct {
	api        *telego.Bot
	cfg        config.BotConfig
	dispatcher *Dispatcher
}

// NewBot initializes a Bot and establishes the Telegram API connection.
func NewBot(
	botCfg config.BotConfig,
	globalCfg *config.Config,
	cfgMgr *config.Manager,
	runnerMgr *runner.Manager,
	sessionMgr *session.Manager,
	workspace string,
) (*Bot, error) {
	api, err := telego.NewBot(botCfg.Token)
	if err != nil {
		return nil, err
	}

	// Fetch bot's own info (to populate username)
	self, err := api.GetMe()
	if err != nil {
		return nil, err
	}

	// Use the Telegram-returned username as default if bot config has no name set
	if botCfg.Name == "" {
		botCfg.Name = self.Username
	}

	slog.Info("Telegram bot connected",
		"bot_name", botCfg.Name,
		"username", self.Username)

	// Register command menu so users see the "/" command list in Telegram
	_ = api.SetMyCommands(&telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "help", Description: "Show all commands"},
			{Command: "clear", Description: "Clear session and reload MCP"},
			{Command: "usage", Description: "Today's token usage stats"},
			{Command: "status", Description: "Show runtime status"},
			{Command: "update", Description: "Restart and pull latest version"},
			{Command: "config", Description: "Show current MCP config"},
			{Command: "bg", Description: "Force background mode for a task"},
		},
	})

	dispatcher := NewDispatcher(api, botCfg, globalCfg, cfgMgr, runnerMgr, sessionMgr, workspace)

	return &Bot{
		api:        api,
		cfg:        botCfg,
		dispatcher: dispatcher,
	}, nil
}

// UpdateConfig hot-reloads: updates bot config without rebuilding the connection.
func (b *Bot) UpdateConfig(cfg *config.Config) {
	// Find the matching bot config by name
	for _, bc := range cfg.Bots {
		if bc.Name == b.cfg.Name {
			b.cfg = bc
			b.dispatcher.UpdateConfig(cfg, bc)
			return
		}
	}
}

// sendRestartNotification checks for a pending restart notification and sends the changelog, then removes the file.
func (b *Bot) sendRestartNotification() {
	notifPath := b.dispatcher.restartNotifyPath()
	data, err := os.ReadFile(notifPath)
	if err != nil {
		return
	}
	_ = os.Remove(notifPath)

	var notif struct {
		ChatID    int64  `json:"chat_id"`
		TopicID   int    `json:"topic_id"`
		OldCommit string `json:"old_commit"`
	}
	if err := json.Unmarshal(data, &notif); err != nil {
		return
	}

	// Generate changelog (list of new commits)
	var changelogPart string
	if notif.OldCommit != "" {
		out, err := exec.Command("git", "-C", b.dispatcher.workspace,
			"log", notif.OldCommit+"..HEAD", "--oneline").Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			changelogPart = "\n\n*What's new*\n```\n" + strings.TrimSpace(string(out)) + "\n```"
		}
	}

	msg := fmt.Sprintf("✅ Restart complete, version `%s`%s", buildinfo.Version, changelogPart)
	b.dispatcher.reply(notif.ChatID, notif.TopicID, msg)
	slog.Info("restart notification sent", "chat_id", notif.ChatID, "topic_id", notif.TopicID)
}

// Run starts the long-polling loop, blocking until ctx is cancelled.
// Should be called in its own goroutine.
// Automatically reconnects if the polling connection drops (e.g. 409 Conflict on restart).
func (b *Bot) Run(ctx context.Context) {
	slog.Info("starting bot long polling", "bot", b.cfg.Name)

	// Check for a pending restart notification (/update triggers restart then sends)
	go func() {
		time.Sleep(2 * time.Second) // wait for bot to be fully ready
		b.sendRestartNotification()
	}()

	for {
		if err := b.runPollingOnce(ctx); err != nil {
			slog.Error("long polling failed", "bot", b.cfg.Name, "err", err)
		}

		select {
		case <-ctx.Done():
			slog.Info("bot stopped", "bot", b.cfg.Name)
			return
		default:
			// Polling exited unexpectedly (e.g. 409 Conflict on restart) — wait and reconnect
			slog.Warn("polling exited, reconnecting in 5s", "bot", b.cfg.Name)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// runPollingOnce runs one polling session until the channel closes or ctx is cancelled.
func (b *Bot) runPollingOnce(ctx context.Context) error {
	updates, err := b.api.UpdatesViaLongPolling(
		// Explicitly include message_reaction (not included by default, must be declared)
		(&telego.GetUpdatesParams{}).WithAllowedUpdates("message", "message_reaction"),
		telego.WithLongPollingContext(ctx),
		telego.WithLongPollingRetryTimeout(3*time.Second),
	)
	if err != nil {
		return err
	}
	defer b.api.StopLongPolling()

	for {
		select {
		case <-ctx.Done():
			return nil

		case update, ok := <-updates:
			if !ok {
				// channel closed (ctx cancelled or connection dropped)
				return nil
			}

			// Process in independent goroutine to prevent slow message handling from blocking polling
			go b.dispatcher.Handle(ctx, update)
		}
	}
}

// Manager manages the lifecycle of all bot instances.
type Manager struct {
	bots []*Bot
}

// NewManager initializes all bot instances from config.
func NewManager(
	cfg *config.Config,
	cfgMgr *config.Manager,
	runnerMgr *runner.Manager,
	sessionMgr *session.Manager,
) (*Manager, error) {
	var bots []*Bot
	for _, botCfg := range cfg.Bots {
		b, err := NewBot(botCfg, cfg, cfgMgr, runnerMgr, sessionMgr, cfg.Workspace)
		if err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return &Manager{bots: bots}, nil
}

// Run starts all bots concurrently, blocking until all bot goroutines exit.
// Exit is typically triggered by ctx cancellation.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range m.bots {
		wg.Add(1)
		bot := b
		go func() {
			defer wg.Done()
			bot.Run(ctx)
		}()
	}
	wg.Wait()
	slog.Info("all bots exited")
}

// Send uses the first bot to send a message to the specified chat (used for heartbeat and system messages).
func (m *Manager) Send(chatID int64, topicID int, text string) {
	if len(m.bots) == 0 {
		return
	}
	m.bots[0].dispatcher.reply(chatID, topicID, text)
}

// UpdateConfig broadcasts a config update to all bots.
func (m *Manager) UpdateConfig(cfg *config.Config) {
	for _, b := range m.bots {
		b.UpdateConfig(cfg)
	}
}
