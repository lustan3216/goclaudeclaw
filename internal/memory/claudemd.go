// Package memory contains CLAUDE.md management functionality.
// Maintains a managed block inside the CLAUDE.md file under the workspace directory,
// used to inject bot-related system-level context into Claude without overwriting user-customized content.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ManagedBlockStart is the start marker for the managed block.
	ManagedBlockStart = "<!-- goclaudeclaw:managed:start -->"
	// ManagedBlockEnd is the end marker for the managed block.
	ManagedBlockEnd = "<!-- goclaudeclaw:managed:end -->"

	claudeMDFilename = "CLAUDE.md"
)

// EnsureClaudeMD ensures CLAUDE.md exists under the workspace and contains the managed block.
//   - If the file doesn't exist: create it with the managed block.
//   - If the file exists but has no managed block: append the managed block at the end.
//   - If the file exists and already has a managed block: replace the managed block content.
//
// Content outside the managed block is always preserved and never overwritten.
func EnsureClaudeMD(workspace string, content string) error {
	path := filepath.Join(workspace, claudeMDFilename)

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read CLAUDE.md: %w", err)
		}
		// File doesn't exist, create it
		return writeClaudeMD(path, buildManagedBlock(content))
	}

	// File exists, replace or append the managed block
	merged := mergeManagedBlock(string(existing), content)
	return writeClaudeMD(path, merged)
}

// UpdateManagedBlock updates only the managed block content in CLAUDE.md.
// Equivalent to EnsureClaudeMD if the file or managed block doesn't exist.
func UpdateManagedBlock(workspace string, content string) error {
	return EnsureClaudeMD(workspace, content)
}

// ReadManagedBlock reads the managed block content from CLAUDE.md.
// Returns an empty string if the file doesn't exist or has no managed block.
func ReadManagedBlock(workspace string) string {
	path := filepath.Join(workspace, claudeMDFilename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	_, inner, ok := extractManagedBlock(string(raw))
	if !ok {
		return ""
	}
	return strings.TrimSpace(inner)
}

// mergeManagedBlock replaces the managed block in the existing file content; appends if not found.
func mergeManagedBlock(existing string, newContent string) string {
	before, _, ok := extractManagedBlock(existing)
	if !ok {
		// No managed block found, append to the end
		trimmed := strings.TrimRight(existing, "\n")
		return trimmed + "\n\n" + buildManagedBlock(newContent) + "\n"
	}

	// Find content after the end marker
	endIdx := strings.Index(existing, ManagedBlockEnd)
	after := ""
	if endIdx >= 0 {
		after = existing[endIdx+len(ManagedBlockEnd):]
	}

	// Reassemble: before + new managed block + after
	result := strings.TrimRight(before, "\n")
	if result != "" {
		result += "\n\n"
	}
	result += buildManagedBlock(newContent)
	afterTrimmed := strings.TrimLeft(after, "\n")
	if afterTrimmed != "" {
		result += "\n\n" + afterTrimmed
	} else {
		result += "\n"
	}
	return result
}

// extractManagedBlock extracts the managed block from file content.
// Returns before (content before the block), inner (content inside the block), ok (whether the block was found).
func extractManagedBlock(content string) (before, inner string, ok bool) {
	startIdx := strings.Index(content, ManagedBlockStart)
	if startIdx < 0 {
		return content, "", false
	}
	endIdx := strings.Index(content, ManagedBlockEnd)
	if endIdx < 0 || endIdx < startIdx {
		return content, "", false
	}
	before = content[:startIdx]
	inner = content[startIdx+len(ManagedBlockStart) : endIdx]
	return before, inner, true
}

// buildManagedBlock builds the complete managed block string.
func buildManagedBlock(content string) string {
	return ManagedBlockStart + "\n" + strings.TrimSpace(content) + "\n" + ManagedBlockEnd
}

// writeClaudeMD atomically writes CLAUDE.md (write temp file then rename).
func writeClaudeMD(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write temp CLAUDE.md: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename CLAUDE.md: %w", err)
	}
	return nil
}
