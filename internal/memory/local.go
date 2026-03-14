// Package memory 管理本地 markdown 记忆文件的读写。
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	localMemoryFile = ".claudeclaw/memory.md"
	maxMemoryBytes  = 2000 // 限制注入大小，避免 token 浪费
)

// LocalMemory 管理工作区本地记忆文件。
type LocalMemory struct {
	workspace string
}

// NewLocalMemory 创建本地记忆管理器。
func NewLocalMemory(workspace string) *LocalMemory {
	return &LocalMemory{workspace: workspace}
}

// Load 读取记忆文件内容。如果文件不存在，返回空字符串（不视为错误）。
func (m *LocalMemory) Load() (string, error) {
	path := filepath.Join(m.workspace, localMemoryFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取记忆文件失败: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if len(content) > maxMemoryBytes {
		content = content[:maxMemoryBytes] + "\n...(truncated)"
	}
	return content, nil
}

// Save 写入记忆文件内容（原子写，覆盖）。
func (m *LocalMemory) Save(content string) error {
	path := filepath.Join(m.workspace, localMemoryFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadRelevant 读取 memory.md，按 prompt 相关性选出 sections 后返回注入内容。
// 优先注入 "always" tagged sections，再按 tag 命中数注入其他相关 sections。
// 若文件不存在或无内容，返回空字符串。
func (m *LocalMemory) LoadRelevant(prompt string) (string, error) {
	path := filepath.Join(m.workspace, localMemoryFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取记忆文件失败: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", nil
	}

	sections := ParseSections(content)
	selected := SelectRelevant(sections, prompt)
	return BuildInjection(selected), nil
}

// InjectPrefix 将记忆内容拼接到 prompt 前面。
// 若记忆为空则直接返回原 prompt。
func InjectPrefix(memory, prompt string) string {
	if memory == "" {
		return prompt
	}
	return fmt.Sprintf("<memory>\n%s\n</memory>\n\n%s", memory, prompt)
}
