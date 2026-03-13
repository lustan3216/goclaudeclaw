// goclaudeclaw — Telegram ↔ Claude Code CLI 桥接守护进程
//
// 主要功能：
//   - 多 bot 支持（每个 bot 独立 goroutine）
//   - 消息防抖与前台/后台任务自动分类
//   - 每个 workspace 串行执行队列
//   - 心跳定时提示（含静默窗口）
//   - cron 任务调度
//   - 配置热重载（fsnotify）
//   - claude-mem 共享记忆集成
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lustan3216/goclaudeclaw/internal/bot"
	"github.com/lustan3216/goclaudeclaw/internal/config"
	"github.com/lustan3216/goclaudeclaw/internal/daemon"
	"github.com/lustan3216/goclaudeclaw/internal/memory"
	"github.com/lustan3216/goclaudeclaw/internal/runner"
	"github.com/lustan3216/goclaudeclaw/internal/scheduler"
	"github.com/lustan3216/goclaudeclaw/internal/session"
)

// version 在构建时通过 -ldflags "-X main.version=x.y.z" 注入。
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// cliFlags 聚合所有命令行标志。
type cliFlags struct {
	configPath string
	pidFile    string
	claudePath string
	debug      bool
}

func newRootCmd() *cobra.Command {
	flags := &cliFlags{}

	root := &cobra.Command{
		Use:     "goclaudeclaw",
		Short:   "Telegram ↔ Claude Code CLI bridge daemon",
		Long:    `goclaudeclaw bridges Telegram bots to Claude Code CLI with shared memory, cron scheduling, and multi-bot support.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(flags)
		},
	}

	root.PersistentFlags().StringVarP(&flags.configPath, "config", "c", "config.json", "配置文件路径")
	root.PersistentFlags().StringVar(&flags.pidFile, "pid-file", "", "PID 文件路径（留空则不写入）")
	root.PersistentFlags().StringVar(&flags.claudePath, "claude", "claude", "claude 二进制路径")
	root.PersistentFlags().BoolVar(&flags.debug, "debug", false, "开启调试日志")

	// 子命令
	root.AddCommand(newVersionCmd())
	root.AddCommand(newValidateCmd(flags))

	return root
}

// run 是守护进程的主入口：初始化所有组件，启动服务，等待信号。
func run(flags *cliFlags) error {
	daemon.SetupLogger(flags.debug)

	slog.Info("goclaudeclaw 启动", "version", version, "config", flags.configPath)

	// ── 1. 加载配置 ────────────────────────────────────────────────
	cfgManager, err := config.New(flags.configPath)
	if err != nil {
		return fmt.Errorf("配置加载失败: %w", err)
	}
	cfg := cfgManager.Get()

	// ── 2. PID 文件（可选）──────────────────────────────────────────
	var pidFile *daemon.PIDFile
	if flags.pidFile != "" {
		pidFile = daemon.NewPIDFile(flags.pidFile)
		if err := pidFile.Write(); err != nil {
			return fmt.Errorf("PID 文件错误: %w", err)
		}
		defer pidFile.Remove()
	}

	// ── 3. 根上下文（信号取消）──────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 4. claude-mem 健康检查 ──────────────────────────────────────
	memClient := memory.New(cfg.Memory.Endpoint)
	if err := memClient.Health(ctx); err != nil {
		// 记忆服务不可用时仅警告，不阻塞启动
		slog.Warn("claude-mem 服务不可达，记忆功能将不可用", "err", err)
	} else {
		slog.Info("claude-mem 连接正常", "endpoint", cfg.Memory.Endpoint)
	}

	// ── 5. Session Manager ──────────────────────────────────────────
	workspace := resolvePath(cfg.Workspace)
	sessionMgr := session.New()

	// ── 6. Runner Manager ───────────────────────────────────────────
	runnerMgr := runner.NewManager(cfg, sessionMgr, flags.claudePath)

	// ── 7. Telegram Bot Manager ─────────────────────────────────────
	botMgr, err := bot.NewManager(cfg, runnerMgr, sessionMgr)
	if err != nil {
		return fmt.Errorf("初始化 bot 失败: %w", err)
	}

	// ── 8. Heartbeat ────────────────────────────────────────────────
	hb, err := scheduler.NewHeartbeat(&cfg.Heartbeat, runnerMgr, workspace, botMgr.Send)
	if err != nil {
		return fmt.Errorf("初始化心跳失败: %w", err)
	}

	// ── 9. Cron Scheduler ───────────────────────────────────────────
	cronSched := scheduler.NewCronScheduler(runnerMgr, workspace)
	if err := cronSched.LoadJobs(ctx, cfg.CronJobs); err != nil {
		return fmt.Errorf("加载 cron 任务失败: %w", err)
	}
	cronSched.Start()
	defer cronSched.Stop(ctx)

	// ── 10. 配置热重载回调 ───────────────────────────────────────────
	cfgManager.OnChange(func(newCfg *config.Config) {
		slog.Info("配置热重载生效")
		runnerMgr.UpdateConfig(newCfg)
		botMgr.UpdateConfig(newCfg)
		if err := cronSched.LoadJobs(ctx, newCfg.CronJobs); err != nil {
			slog.Error("热重载 cron 任务失败", "err", err)
		}
	})

	// ── 11. 启动各服务 ──────────────────────────────────────────────
	// 心跳（独立 goroutine）
	go hb.Start(ctx)

	// 所有 bot（各自独立 goroutine，内部等待 ctx 取消）
	go botMgr.Run(ctx)

	slog.Info("所有服务已启动，等待信号...",
		"workspace", workspace,
		"bots", len(cfg.Bots))

	// ── 12. 等待停止信号 ─────────────────────────────────────────────
	daemon.WaitForShutdown(cancel)

	slog.Info("优雅关闭完成，再见 ⚡")
	return nil
}

// resolvePath 将相对路径转为绝对路径，"." 展开为当前工作目录。
func resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// newVersionCmd 返回 version 子命令。
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本信息",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("goclaudeclaw %s\n", version)
		},
	}
}

// newValidateCmd 返回 validate 子命令，用于校验配置文件合法性。
func newValidateCmd(flags *cliFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "校验配置文件",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := config.New(flags.configPath)
			if err != nil {
				return fmt.Errorf("配置无效: %w", err)
			}
			fmt.Println("✓ 配置文件有效")
			return nil
		},
	}
}
