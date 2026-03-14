// Package memory manages reading and writing of local markdown memory files.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	localMemoryFile = ".claudeclaw/memory.md"
	maxMemoryBytes  = 2000 // limit injection size to avoid wasting tokens
)

// LocalMemory manages the local memory file for a workspace.
type LocalMemory struct {
	workspace string
}

// NewLocalMemory creates a local memory manager.
func NewLocalMemory(workspace string) *LocalMemory {
	return &LocalMemory{workspace: workspace}
}

// Load reads the memory file contents. Returns an empty string if the file doesn't exist (not an error).
func (m *LocalMemory) Load() (string, error) {
	path := filepath.Join(m.workspace, localMemoryFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if len(content) > maxMemoryBytes {
		content = content[:maxMemoryBytes] + "\n...(truncated)"
	}
	return content, nil
}

// Save writes memory file contents (atomic write, overwrites existing).
func (m *LocalMemory) Save(content string) error {
	path := filepath.Join(m.workspace, localMemoryFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadRelevant reads memory.md and returns injection content filtered by prompt relevance.
// Prioritizes "always"-tagged sections, then other sections by tag hit count.
// Returns an empty string if the file doesn't exist or has no content.
func (m *LocalMemory) LoadRelevant(prompt string) (string, error) {
	path := filepath.Join(m.workspace, localMemoryFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", nil
	}

	sections := ParseSections(content)
	selected := SelectRelevant(sections, prompt)
	return BuildInjection(selected), nil
}

// InjectPrefix prepends the memory content to the prompt.
// If memory is empty, returns the original prompt unchanged.
func InjectPrefix(memory, prompt string) string {
	if memory == "" {
		return prompt
	}
	return fmt.Sprintf("<memory>\n%s\n</memory>\n\n%s", memory, prompt)
}
