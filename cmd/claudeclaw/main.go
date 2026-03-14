// goclaudeclaw — Telegram ↔ Claude Code CLI bridge daemon
//
// Key features:
//   - Multi-bot support (each bot runs in its own goroutine)
//   - Message debouncing with automatic foreground/background task classification
//   - Per-workspace serial execution queue
//   - Heartbeat scheduling with quiet windows
//   - Cron job scheduling
//   - Hot config reload (fsnotify)
//   - claude-mem shared memory integration
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lustan3216/claudeclaw/internal/bot"
	"github.com/lustan3216/claudeclaw/internal/buildinfo"
	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/daemon"
	"github.com/lustan3216/claudeclaw/internal/mcp"
	"github.com/lustan3216/claudeclaw/internal/memory"
	"github.com/lustan3216/claudeclaw/internal/runner"
	"github.com/lustan3216/claudeclaw/internal/scheduler"
	"github.com/lustan3216/claudeclaw/internal/session"
)

// version is injected at build time via ldflags into buildinfo.Version.
// Kept here for the cobra root command Version field.
var version = buildinfo.Version

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// cliFlags aggregates all command-line flags.
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

	root.PersistentFlags().StringVarP(&flags.configPath, "config", "c", "config.json", "path to config file")
	root.PersistentFlags().StringVar(&flags.pidFile, "pid-file", "", "path to PID file (omit to disable)")
	root.PersistentFlags().StringVar(&flags.claudePath, "claude", "claude", "path to claude binary")
	root.PersistentFlags().BoolVar(&flags.debug, "debug", false, "enable debug logging")

	// Subcommands
	root.AddCommand(newVersionCmd())
	root.AddCommand(newValidateCmd(flags))

	return root
}

// run is the daemon's main entry point: initializes all components, starts services, waits for signals.
func run(flags *cliFlags) error {
	daemon.SetupLogger(flags.debug)

	slog.Info("goclaudeclaw starting", "version", version, "config", flags.configPath)

	// ── 1. Load config ────────────────────────────────────────────────
	cfgManager, err := config.New(flags.configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	cfg := cfgManager.Get()

	// ── 2. PID file (optional) ──────────────────────────────────────────
	var pidFile *daemon.PIDFile
	if flags.pidFile != "" {
		pidFile = daemon.NewPIDFile(flags.pidFile)
		if err := pidFile.Write(); err != nil {
			return fmt.Errorf("PID file error: %w", err)
		}
		defer pidFile.Remove()
	}

	// ── 3. Root context (signal cancellation) ──────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 4. claude-mem health check ──────────────────────────────────────────
	memClient := memory.New(cfg.Memory.Endpoint)
	if err := memClient.Health(ctx); err != nil {
		// Warn only when memory service is unavailable — don't block startup
		slog.Warn("claude-mem service unreachable, memory features will be unavailable", "err", err)
	} else {
		slog.Info("claude-mem connected", "endpoint", cfg.Memory.Endpoint)
	}

	// ── 5. MCP config generation ─────────────────────────────────────────
	workspace := resolvePath(cfg.Workspace)
	if err := mcp.ApplyConfig(workspace, cfg.MCPs); err != nil {
		slog.Warn("MCP config generation failed, skipping", "err", err)
	}

	// ── 6. Session Manager ──────────────────────────────────────────
	sessionMgr := session.New()

	// ── 7. Runner Manager ───────────────────────────────────────────
	runnerMgr := runner.NewManager(cfg, sessionMgr, flags.claudePath)

	// ── 8. Telegram Bot Manager ─────────────────────────────────────
	botMgr, err := bot.NewManager(cfg, cfgManager, runnerMgr, sessionMgr)
	if err != nil {
		return fmt.Errorf("failed to initialize bot: %w", err)
	}

	// ── 9. Heartbeat ────────────────────────────────────────────────
	hb, err := scheduler.NewHeartbeat(&cfg.Heartbeat, runnerMgr, workspace, botMgr.Send)
	if err != nil {
		return fmt.Errorf("failed to initialize heartbeat: %w", err)
	}

	// ── 10. Cron Scheduler ──────────────────────────────────────────
	cronSched := scheduler.NewCronScheduler(runnerMgr, workspace)
	if err := cronSched.LoadJobs(ctx, cfg.CronJobs); err != nil {
		return fmt.Errorf("failed to load cron jobs: %w", err)
	}
	cronSched.Start()
	defer cronSched.Stop(ctx)

	// ── 11. Config hot-reload callback ───────────────────────────────────────────
	cfgManager.OnChange(func(newCfg *config.Config) {
		slog.Info("config hot-reload applied")
		runnerMgr.UpdateConfig(newCfg)
		botMgr.UpdateConfig(newCfg)
		if err := cronSched.LoadJobs(ctx, newCfg.CronJobs); err != nil {
			slog.Error("failed to hot-reload cron jobs", "err", err)
		}
		// Regenerate .mcp.json when MCP config changes
		if err := mcp.ApplyConfig(workspace, newCfg.MCPs); err != nil {
			slog.Warn("failed to hot-reload MCP config", "err", err)
		}
	})

	// ── 12. Start all services ──────────────────────────────────────────
	// Heartbeat (independent goroutine)
	go hb.Start(ctx)

	// All bots (each in its own goroutine, waiting internally for ctx cancellation)
	go botMgr.Run(ctx)

	slog.Info("all services started, waiting for signal...",
		"workspace", workspace,
		"bots", len(cfg.Bots))

	// ── 13. Wait for shutdown signal ─────────────────────────────────────────
	daemon.WaitForShutdown(cancel)

	slog.Info("graceful shutdown complete, goodbye ⚡")
	return nil
}

// resolvePath converts a relative path to an absolute path; "." expands to the current working directory.
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

// newVersionCmd returns the version subcommand.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version info",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("goclaudeclaw %s\n", version)
		},
	}
}

// newValidateCmd returns the validate subcommand for checking config file validity.
func newValidateCmd(flags *cliFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := config.New(flags.configPath)
			if err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}
			fmt.Println("✓ Config file is valid")
			return nil
		},
	}
}
