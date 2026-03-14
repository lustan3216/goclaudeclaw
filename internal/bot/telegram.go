// Package bot 实现每个 Telegram Bot 的长轮询 goroutine。
// 每个 bot 独立运行，共享 runner.Manager 和 session.Manager。
package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"github.com/lustan3216/goclaudeclaw/internal/config"
	"github.com/lustan3216/goclaudeclaw/internal/runner"
	"github.com/lustan3216/goclaudeclaw/internal/session"
)

// Bot 封装单个 Telegram bot 的生命周期。
type Bot struct {
	api        *telego.Bot
	cfg        config.BotConfig
	dispatcher *Dispatcher
}

// NewBot 初始化 Bot，建立 Telegram API 连接。
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

	// 获取 bot 自身信息（用于填充 username）
	self, err := api.GetMe()
	if err != nil {
		return nil, err
	}

	// 若 bot 配置未设置 name，使用 Telegram 返回的 username 作为默认值
	if botCfg.Name == "" {
		botCfg.Name = self.Username
	}

	slog.Info("Telegram bot 已连接",
		"bot_name", botCfg.Name,
		"username", self.Username)

	// 注册指令选单，让用户在 Telegram 中看到 "/" 指令列表
	_ = api.SetMyCommands(&telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "help", Description: "查看所有指令"},
			{Command: "clear", Description: "清除 session，重载 MCP"},
			{Command: "usage", Description: "今日 token 用量统计"},
			{Command: "status", Description: "查看运行状态"},
			{Command: "update", Description: "立即重启并拉取最新版本"},
			{Command: "config", Description: "查看当前 MCP 配置"},
			{Command: "bg", Description: "强制后台模式运行任务"},
		},
	})

	dispatcher := NewDispatcher(api, botCfg, globalCfg, cfgMgr, runnerMgr, sessionMgr, workspace)

	return &Bot{
		api:        api,
		cfg:        botCfg,
		dispatcher: dispatcher,
	}, nil
}

// UpdateConfig 热重载：更新 bot 配置（不重建连接）。
func (b *Bot) UpdateConfig(cfg *config.Config) {
	// 找到对应名称的 bot 配置
	for _, bc := range cfg.Bots {
		if bc.Name == b.cfg.Name {
			b.cfg = bc
			b.dispatcher.UpdateConfig(cfg, bc)
			return
		}
	}
}

// Run 启动长轮询循环，阻塞直到 ctx 取消。
// 应在独立 goroutine 中调用。
func (b *Bot) Run(ctx context.Context) {
	slog.Info("启动 bot 长轮询", "bot", b.cfg.Name)

	updates, err := b.api.UpdatesViaLongPolling(
		// 显式指定 message_reaction（默认不包含，需明确声明）
		(&telego.GetUpdatesParams{}).WithAllowedUpdates("message", "message_reaction"),
		telego.WithLongPollingContext(ctx),
		telego.WithLongPollingRetryTimeout(3*time.Second),
	)
	if err != nil {
		slog.Error("启动长轮询失败", "bot", b.cfg.Name, "err", err)
		return
	}
	defer b.api.StopLongPolling()

	for {
		select {
		case <-ctx.Done():
			slog.Info("bot 收到停止信号，退出长轮询", "bot", b.cfg.Name)
			return

		case update, ok := <-updates:
			if !ok {
				// channel 被关闭（ctx 已取消或连接断开）
				slog.Warn("更新 channel 已关闭，bot 退出", "bot", b.cfg.Name)
				return
			}

			// 在独立 goroutine 处理，防止单条消息处理慢影响轮询
			// 注意：dispatcher 内部有防抖和串行队列，不会产生并发冲突
			go b.dispatcher.Handle(ctx, update)
		}
	}
}

// Manager 管理所有 bot 实例的生命周期。
type Manager struct {
	bots []*Bot
}

// NewManager 根据配置初始化所有 bot 实例。
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

// Run 并发启动所有 bot，阻塞直到所有 bot goroutine 退出。
// 通常由 ctx 取消触发退出。
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
	slog.Info("所有 bot 已退出")
}

// Send 使用第一个 bot 向指定 chat 发送消息（心跳等系统消息使用）。
func (m *Manager) Send(chatID int64, topicID int, text string) {
	if len(m.bots) == 0 {
		return
	}
	m.bots[0].dispatcher.reply(chatID, topicID, text)
}

// UpdateConfig 向所有 bot 广播配置更新。
func (m *Manager) UpdateConfig(cfg *config.Config) {
	for _, b := range m.bots {
		b.UpdateConfig(cfg)
	}
}
