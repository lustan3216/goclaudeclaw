// Package scheduler 实现心跳定时提示和 cron 任务调度。
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/lustan3216/goclaudeclaw/internal/config"
	"github.com/lustan3216/goclaudeclaw/internal/runner"
)

// SendFn 是心跳结果的 Telegram 发送回调，由 bot 层注入。
type SendFn func(chatID int64, topicID int, text string)

// Heartbeat 按配置间隔向 claude 发送定期 prompt，
// 支持静默窗口（如夜间不打扰）和时区设置。
type Heartbeat struct {
	cfg       *config.HeartbeatConfig
	runnerMgr *runner.Manager
	workspace string
	loc       *time.Location
	ticker    *time.Ticker
	stopCh    chan struct{}
	sendFn    SendFn // 结果发送回调，nil 表示仅记日志
}

// NewHeartbeat 创建心跳调度器。sendFn 用于将心跳结果发送到 Telegram，nil 则只记日志。
func NewHeartbeat(cfg *config.HeartbeatConfig, runnerMgr *runner.Manager, workspace string, sendFn SendFn) (*Heartbeat, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		slog.Warn("时区加载失败，使用 UTC", "timezone", cfg.Timezone, "err", err)
		loc = time.UTC
	}

	return &Heartbeat{
		cfg:       cfg,
		runnerMgr: runnerMgr,
		workspace: workspace,
		loc:       loc,
		stopCh:    make(chan struct{}),
		sendFn:    sendFn,
	}, nil
}

// Start 启动心跳循环，阻塞直到 ctx 取消或 Stop() 调用。
// 应在独立 goroutine 中运行。
func (h *Heartbeat) Start(ctx context.Context) {
	if !h.cfg.Enabled {
		slog.Debug("心跳未启用，跳过")
		return
	}

	interval := time.Duration(h.cfg.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	h.ticker = time.NewTicker(interval)
	defer h.ticker.Stop()

	slog.Info("心跳调度器已启动",
		"interval", interval,
		"timezone", h.cfg.Timezone,
		"quiet_windows", len(h.cfg.QuietWindows))

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case t := <-h.ticker.C:
			localTime := t.In(h.loc)
			if h.isQuietTime(localTime) {
				slog.Debug("当前处于静默窗口，跳过心跳", "local_time", localTime.Format("15:04"))
				continue
			}
			h.fire(ctx, localTime)
		}
	}
}

// Stop 停止心跳调度器。
func (h *Heartbeat) Stop() {
	close(h.stopCh)
}

// fire 触发一次心跳：向 runner 提交 prompt 任务，并将结果发送到 Telegram。
func (h *Heartbeat) fire(ctx context.Context, t time.Time) {
	if h.sendFn == nil || h.cfg.ChatID == 0 {
		slog.Warn("心跳未配置发送目标（chat_id），跳过", "time", t.Format("15:04"))
		return
	}

	prompt := h.cfg.Prompt
	if prompt == "" {
		prompt = "Check pending tasks and provide a brief status update."
	}

	slog.Info("触发心跳", "time", t.Format("15:04"), "chat_id", h.cfg.ChatID, "prompt_preview", truncate(prompt, 60))

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
			slog.Warn("心跳任务失败", "err", result.Err)
			return
		}
		if result.Output != "" {
			h.sendFn(h.cfg.ChatID, h.cfg.TopicID, result.Output)
		}
	}()
}

// isQuietTime 检查当前时间是否在任意静默窗口内。
// 支持跨午夜的窗口，例如 23:00 ~ 08:00。
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

// timeOfDay 返回时间点距当天 0:00 的分钟数。
func timeOfDay(t time.Time) int {
	return t.Hour()*60 + t.Minute()
}

// parseTimeOfDay 将 "HH:MM" 格式字符串解析为当天分钟数。
// 解析失败返回 0。
func parseTimeOfDay(s string) int {
	t, err := time.Parse("15:04", s)
	if err != nil {
		slog.Warn("静默时间格式错误", "value", s, "expected", "HH:MM")
		return 0
	}
	return t.Hour()*60 + t.Minute()
}

// inWindow 判断 current 是否在 [start, end) 窗口内，支持跨午夜。
func inWindow(current, start, end int) bool {
	if start <= end {
		// 普通窗口，如 09:00 ~ 17:00
		return current >= start && current < end
	}
	// 跨午夜窗口，如 23:00 ~ 08:00
	return current >= start || current < end
}

// truncate 截断字符串（复用 runner 包同名函数逻辑，避免循环依赖）。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
