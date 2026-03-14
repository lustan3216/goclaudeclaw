// cron.go 实现基于配置的定时任务调度。
// 任务定义使用标准 cron 表达式，支持秒级精度（通过 robfig/cron v3）。
package scheduler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/robfig/cron/v3"

	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/runner"
)

// CronScheduler 管理所有 cron 任务的生命周期。
type CronScheduler struct {
	c         *cron.Cron
	runnerMgr *runner.Manager
	workspace string // 全局默认 workspace
	entryIDs  map[string]cron.EntryID // job name → entry ID，用于动态更新
}

// NewCronScheduler 创建 CronScheduler。
// 使用秒级精度解析器（5 字段 cron 表达式）。
func NewCronScheduler(runnerMgr *runner.Manager, workspace string) *CronScheduler {
	c := cron.New(
		cron.WithSeconds(),                          // 支持秒级精度
		cron.WithChain(cron.SkipIfStillRunning(     // 上一次未完成时跳过
			cron.DefaultLogger,
		)),
		cron.WithLogger(cron.DefaultLogger),
	)

	return &CronScheduler{
		c:         c,
		runnerMgr: runnerMgr,
		workspace: workspace,
		entryIDs:  make(map[string]cron.EntryID),
	}
}

// LoadJobs 从配置加载所有 cron 任务。
// 可在热重载时重复调用，会先清除所有旧任务再重新注册。
func (s *CronScheduler) LoadJobs(ctx context.Context, jobs []config.CronJob) error {
	// 清除旧任务
	for _, id := range s.entryIDs {
		s.c.Remove(id)
	}
	s.entryIDs = make(map[string]cron.EntryID)

	for _, job := range jobs {
		if err := s.addJob(ctx, job); err != nil {
			return fmt.Errorf("注册 cron 任务 %q 失败: %w", job.Name, err)
		}
	}

	slog.Info("cron 任务加载完成", "count", len(jobs))
	return nil
}

// addJob 注册单条 cron 任务。
func (s *CronScheduler) addJob(ctx context.Context, job config.CronJob) error {
	ws := job.Workspace
	if ws == "" {
		ws = s.workspace
	}

	entryID, err := s.c.AddFunc(job.Schedule, func() {
		slog.Info("cron 任务触发", "name", job.Name, "workspace", ws)
		s.runnerMgr.Submit(runner.Job{
			Ctx:       ctx,
			Workspace: ws,
			Prompt:    job.Prompt,
			Mode:      runner.ModeBackground, // cron 任务均以后台模式运行
			ResultCh:  nil,
		})
	})
	if err != nil {
		return err
	}

	s.entryIDs[job.Name] = entryID
	slog.Info("已注册 cron 任务",
		"name", job.Name,
		"schedule", job.Schedule,
		"workspace", ws)
	return nil
}

// Start 启动 cron 调度器（非阻塞，内部使用 goroutine）。
func (s *CronScheduler) Start() {
	s.c.Start()
	slog.Info("cron 调度器已启动")
}

// Stop 优雅停止 cron 调度器，等待当前正在执行的任务完成。
func (s *CronScheduler) Stop(ctx context.Context) {
	stopCtx := s.c.Stop()
	select {
	case <-stopCtx.Done():
		slog.Info("cron 调度器已停止")
	case <-ctx.Done():
		slog.Warn("等待 cron 停止超时，强制退出")
	}
}

// Entries 返回所有已注册任务的调度信息（用于 /status 接口展示）。
func (s *CronScheduler) Entries() []cron.Entry {
	return s.c.Entries()
}
