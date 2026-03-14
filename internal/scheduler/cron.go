// cron.go implements config-driven cron job scheduling.
// Jobs use standard cron expressions with second-level precision (via robfig/cron v3).
package scheduler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/robfig/cron/v3"

	"github.com/lustan3216/claudeclaw/internal/config"
	"github.com/lustan3216/claudeclaw/internal/runner"
)

// CronScheduler manages the lifecycle of all cron jobs.
type CronScheduler struct {
	c         *cron.Cron
	runnerMgr *runner.Manager
	workspace string              // global default workspace
	entryIDs  map[string]cron.EntryID // job name → entry ID, for dynamic updates
}

// NewCronScheduler creates a CronScheduler.
// Uses a second-precision parser (5-field cron expression).
func NewCronScheduler(runnerMgr *runner.Manager, workspace string) *CronScheduler {
	c := cron.New(
		cron.WithSeconds(),                      // enable second-level precision
		cron.WithChain(cron.SkipIfStillRunning( // skip if previous run hasn't finished
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

// LoadJobs loads all cron jobs from config.
// Can be called repeatedly on hot-reload; removes all old jobs before re-registering.
func (s *CronScheduler) LoadJobs(ctx context.Context, jobs []config.CronJob) error {
	// Remove old jobs
	for _, id := range s.entryIDs {
		s.c.Remove(id)
	}
	s.entryIDs = make(map[string]cron.EntryID)

	for _, job := range jobs {
		if err := s.addJob(ctx, job); err != nil {
			return fmt.Errorf("failed to register cron job %q: %w", job.Name, err)
		}
	}

	slog.Info("cron jobs loaded", "count", len(jobs))
	return nil
}

// addJob registers a single cron job.
func (s *CronScheduler) addJob(ctx context.Context, job config.CronJob) error {
	ws := job.Workspace
	if ws == "" {
		ws = s.workspace
	}

	entryID, err := s.c.AddFunc(job.Schedule, func() {
		slog.Info("cron job triggered", "name", job.Name, "workspace", ws)
		s.runnerMgr.Submit(runner.Job{
			Ctx:       ctx,
			Workspace: ws,
			Prompt:    job.Prompt,
			Mode:      runner.ModeBackground, // cron jobs always run in background mode
			ResultCh:  nil,
		})
	})
	if err != nil {
		return err
	}

	s.entryIDs[job.Name] = entryID
	slog.Info("cron job registered",
		"name", job.Name,
		"schedule", job.Schedule,
		"workspace", ws)
	return nil
}

// Start starts the cron scheduler (non-blocking, uses internal goroutine).
func (s *CronScheduler) Start() {
	s.c.Start()
	slog.Info("cron scheduler started")
}

// Stop gracefully stops the cron scheduler, waiting for any running jobs to complete.
func (s *CronScheduler) Stop(ctx context.Context) {
	stopCtx := s.c.Stop()
	select {
	case <-stopCtx.Done():
		slog.Info("cron scheduler stopped")
	case <-ctx.Done():
		slog.Warn("timed out waiting for cron to stop, forcing exit")
	}
}

// Entries returns scheduling info for all registered jobs (used for /status display).
func (s *CronScheduler) Entries() []cron.Entry {
	return s.c.Entries()
}
