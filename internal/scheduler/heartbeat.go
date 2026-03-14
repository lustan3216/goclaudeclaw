// Package scheduler implements heartbeat scheduling and cron job dispatch.
package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/runner"
	"github.com/lustan3216/claudeclaw/internal/util"
)

// SendFn is the Telegram send callback for heartbeat results, injected by the bot layer.
type SendFn func(chatID int64, topicID int, text string)

// Heartbeat sends periodic prompts to claude at the configured interval,
// with support for quiet windows (e.g. no disturbance at night) and timezone settings.
type Heartbeat struct {
	cfg       *config.HeartbeatConfig
	runnerMgr *runner.Manager
	workspace string
	loc       *time.Location
	ticker    *time.Ticker
	sendFn    SendFn // result send callback; nil means log only
}

// NewHeartbeat creates a heartbeat scheduler. sendFn sends heartbeat results to Telegram; nil logs only.
func NewHeartbeat(cfg *config.HeartbeatConfig, runnerMgr *runner.Manager, workspace string, sendFn SendFn) (*Heartbeat, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		slog.Warn("failed to load timezone, using UTC", "timezone", cfg.Timezone, "err", err)
		loc = time.UTC
	}

	return &Heartbeat{
		cfg:       cfg,
		runnerMgr: runnerMgr,
		workspace: workspace,
		loc:       loc,
		sendFn:    sendFn,
	}, nil
}

// Start runs the heartbeat loop, blocking until ctx is cancelled or Stop() is called.
// Should be run in its own goroutine.
func (h *Heartbeat) Start(ctx context.Context) {
	if !h.cfg.Enabled {
		slog.Debug("heartbeat disabled, skipping")
		return
	}

	interval := time.Duration(h.cfg.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	h.ticker = time.NewTicker(interval)
	defer h.ticker.Stop()

	slog.Info("heartbeat scheduler started",
		"interval", interval,
		"timezone", h.cfg.Timezone,
		"quiet_windows", len(h.cfg.QuietWindows))

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-h.ticker.C:
			localTime := t.In(h.loc)
			if h.isQuietTime(localTime) {
				slog.Debug("currently in quiet window, skipping heartbeat", "local_time", localTime.Format("15:04"))
				continue
			}
			h.fire(ctx, localTime)
		}
	}
}

// fire triggers one heartbeat: submits a prompt job to the runner and sends the result to Telegram.
func (h *Heartbeat) fire(ctx context.Context, t time.Time) {
	if h.sendFn == nil || h.cfg.ChatID == 0 {
		slog.Warn("heartbeat has no send target (chat_id), skipping", "time", t.Format("15:04"))
		return
	}

	prompt := h.buildPrompt()

	slog.Info("heartbeat fired", "time", t.Format("15:04"), "chat_id", h.cfg.ChatID, "prompt_preview", util.Truncate(prompt, 60))

	resultCh := make(chan runner.Result, 1)
	h.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: h.workspace,
		ChatID:    h.cfg.ChatID,
		TopicID:   h.cfg.TopicID,
		Prompt:    prompt,
		Mode:      runner.ModeBackground,
		ResultCh:  resultCh,
	})

	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("heartbeat job failed", "err", result.Err)
			return
		}
		output := strings.TrimSpace(result.Output)
		if output != "" && !strings.Contains(output, "HEARTBEAT_OK") {
			h.sendFn(h.cfg.ChatID, h.cfg.TopicID, output)
		}
	}()
}

// buildPrompt constructs the heartbeat prompt: reads .claudeclaw/heartbeat.md first,
// then falls back to the configured prompt.
// heartbeat.md is a user-defined checklist; the agent evaluates each item and decides whether to notify.
func (h *Heartbeat) buildPrompt() string {
	checklistPath := filepath.Join(h.workspace, ".claudeclaw", "heartbeat.md")
	if data, err := os.ReadFile(checklistPath); err == nil && len(data) > 0 {
		return "Please read the following heartbeat checklist and evaluate each item. If there is anything that needs attention, describe it directly; if everything is fine, reply with only HEARTBEAT_OK and nothing else.\n\n" +
			string(data)
	}
	if h.cfg.Prompt != "" {
		return h.cfg.Prompt
	}
	return "Check pending tasks, reminders, and anything that needs attention. If nothing needs action, reply HEARTBEAT_OK."
}

// isQuietTime checks whether the current time falls within any quiet window.
// Supports windows that span midnight, e.g. 23:00 ~ 08:00.
func (h *Heartbeat) isQuietTime(t time.Time) bool {
	current := timeOfDay(t)
	for _, w := range h.cfg.QuietWindows {
		start := parseTimeOfDay(w.Start)
		end := parseTimeOfDay(w.End)
		if inWindow(current, start, end) {
			return true
		}
	}
	return false
}

// timeOfDay returns the number of minutes since midnight for the given time.
func timeOfDay(t time.Time) int {
	return t.Hour()*60 + t.Minute()
}

// parseTimeOfDay parses an "HH:MM" string into minutes since midnight.
// Returns 0 on parse failure.
func parseTimeOfDay(s string) int {
	t, err := time.Parse("15:04", s)
	if err != nil {
		slog.Warn("invalid quiet window time format", "value", s, "expected", "HH:MM")
		return 0
	}
	return t.Hour()*60 + t.Minute()
}

// inWindow reports whether current falls within [start, end), supporting midnight crossover.
func inWindow(current, start, end int) bool {
	if start <= end {
		// Normal window, e.g. 09:00 ~ 17:00
		return current >= start && current < end
	}
	// Midnight-crossing window, e.g. 23:00 ~ 08:00
	return current >= start || current < end
}
