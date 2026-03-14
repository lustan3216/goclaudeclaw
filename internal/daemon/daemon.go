// Package daemon manages process lifecycle: PID file, signal handling, and graceful shutdown.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PIDFile manages writing and cleanup of a PID file.
type PIDFile struct {
	path string
}

// NewPIDFile creates a PID file manager.
// path is the PID file path, e.g. "/var/run/claudeclaw.pid".
func NewPIDFile(path string) *PIDFile {
	return &PIDFile{path: path}
}

// Write writes the current process PID to the file.
// Returns an error if the file already exists and the corresponding process is still running (prevents duplicate starts).
func (p *PIDFile) Write() error {
	// Check if another instance is already running
	if existing, err := p.readExisting(); err == nil && existing > 0 {
		if isProcessRunning(existing) {
			return fmt.Errorf("another instance is already running (PID: %d), stop it first or remove %s", existing, p.path)
		}
		// The process for the old PID no longer exists; safe to overwrite
		slog.Info("stale PID file found, overwriting", "old_pid", existing)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return fmt.Errorf("failed to create PID file directory: %w", err)
	}

	pid := os.Getpid()
	content := strconv.Itoa(pid) + "\n"
	if err := os.WriteFile(p.path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	slog.Info("PID file created", "pid", pid, "path", p.path)
	return nil
}

// Remove deletes the PID file (called on program exit).
func (p *PIDFile) Remove() {
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove PID file", "path", p.path, "err", err)
		return
	}
	slog.Info("PID file removed", "path", p.path)
}

// readExisting reads the PID value from an existing PID file.
func (p *PIDFile) readExisting() (int, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file format: %w", err)
	}
	return pid, nil
}

// isProcessRunning checks whether a process with the given PID exists (Unix only).
func isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Send signal 0 to probe whether the process exists without actually signalling it
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// WaitForShutdown blocks until SIGINT or SIGTERM is received,
// then calls cancel() to trigger the graceful shutdown flow.
// Returns the received signal for the caller to log.
func WaitForShutdown(cancel context.CancelFunc) os.Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	slog.Info("shutdown signal received, starting graceful shutdown", "signal", sig)

	// Cancel root context to stop all child components
	cancel()

	// Stop signal listening to avoid blocking on a second signal
	signal.Stop(sigCh)

	return sig
}

// SetupLogger initializes structured logging (slog), choosing the level based on the debug flag.
func SetupLogger(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: debug, // include source location in debug mode
	})
	slog.SetDefault(slog.New(handler))
}
