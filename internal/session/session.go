// Package session 管理每个 bot/chat/topic 的 Claude 会话 ID 持久化。
// 会话文件存储在 workspace/.claudeclaw/sessions/{botName}/{chatID}/{topicID}.json，
// 确保重启后 --resume 标志能恢复上次对话上下文。
// 每个 Telegram topic（论坛话题）拥有独立的 Claude 会话，topicID=0 表示普通聊天。
package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const sessionDir = ".claudeclaw/sessions"

// SessionData 会话文件的 JSON 结构，与 claudeclaw 格式兼容。
type SessionData struct {
	SessionID  string `json:"sessionId"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt"`
}

// sessionKey 会话的复合键（bot + chat + topic）。
type sessionKey struct {
	botName string
	chatID  int64
	topicID int
}

// Manager 管理多个 bot/chat/topic 的会话 ID，并发安全。
type Manager struct {
	mu       sync.RWMutex
	sessions map[sessionKey]string // key → session ID（内存缓存）
}

// New 返回一个新的 Manager 实例。
func New() *Manager {
	return &Manager{
		sessions: make(map[sessionKey]string),
	}
}

// Get 返回指定 bot/chat/topic 的会话 ID，同时更新 lastUsedAt 并回写磁盘。
// 优先从内存缓存读取；缓存未命中时从磁盘加载。
// 如果没有已知会话，返回空字符串（调用方应不带 --resume 运行）。
func (m *Manager) Get(workspace, botName string, chatID int64, topicID int) string {
	key := sessionKey{botName, chatID, topicID}

	m.mu.RLock()
	id, ok := m.sessions[key]
	m.mu.RUnlock()
	if ok {
		// 更新 lastUsedAt
		_ = m.touchLastUsed(workspace, botName, chatID, topicID, id)
		return id
	}

	// 从磁盘读取，并写入缓存
	data := m.load(workspace, botName, chatID, topicID)
	if data == nil {
		return ""
	}
	id = data.SessionID
	if id != "" {
		m.mu.Lock()
		m.sessions[key] = id
		m.mu.Unlock()
		// 更新 lastUsedAt
		_ = m.touchLastUsed(workspace, botName, chatID, topicID, id)
	}
	return id
}

// Set 更新指定 bot/chat/topic 的会话 ID，同时持久化到磁盘。
func (m *Manager) Set(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	key := sessionKey{botName, chatID, topicID}
	m.mu.Lock()
	m.sessions[key] = sessionID
	m.mu.Unlock()
	return m.save(workspace, botName, chatID, topicID, sessionID)
}

// Clear 清除 bot/chat/topic 的会话记录（下次运行将开启新会话）。
func (m *Manager) Clear(workspace, botName string, chatID int64, topicID int) error {
	key := sessionKey{botName, chatID, topicID}
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	path := sessionFilePath(workspace, botName, chatID, topicID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除会话文件失败 %s: %w", path, err)
	}
	slog.Info("会话已清除",
		"workspace", workspace,
		"bot", botName,
		"chat_id", chatID,
		"topic_id", topicID)
	return nil
}

// touchLastUsed 更新会话文件中的 lastUsedAt 字段。
func (m *Manager) touchLastUsed(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	path := sessionFilePath(workspace, botName, chatID, topicID)
	data, err := readSessionFile(path)
	if err != nil || data == nil {
		// 文件不存在时不视为错误，忽略
		return nil
	}
	data.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	return writeSessionFile(path, data)
}

// load 从磁盘读取会话数据。
func (m *Manager) load(workspace, botName string, chatID int64, topicID int) *SessionData {
	path := sessionFilePath(workspace, botName, chatID, topicID)
	data, err := readSessionFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("读取会话文件失败", "path", path, "err", err)
		}
		return nil
	}
	slog.Debug("从磁盘加载会话",
		"bot", botName,
		"chat_id", chatID,
		"topic_id", topicID,
		"session_id", data.SessionID)
	return data
}

// save 将会话 ID 写入磁盘（原子写：先写临时文件再重命名）。
// 若文件已存在，保留 createdAt；否则使用当前时间。
func (m *Manager) save(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	path := sessionFilePath(workspace, botName, chatID, topicID)

	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// 读取已有文件以保留 createdAt
	existing, _ := readSessionFile(path)
	createdAt := now
	if existing != nil && existing.CreatedAt != "" {
		createdAt = existing.CreatedAt
	}

	data := &SessionData{
		SessionID:  sessionID,
		CreatedAt:  createdAt,
		LastUsedAt: now,
	}

	return writeSessionFile(path, data)
}

// readSessionFile 读取并解析 JSON 会话文件。
func readSessionFile(path string) (*SessionData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data SessionData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("解析会话文件失败: %w", err)
	}
	return &data, nil
}

// writeSessionFile 原子写入 JSON 会话文件。
func writeSessionFile(path string, data *SessionData) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话数据失败: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return fmt.Errorf("写入临时会话文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("重命名会话文件失败: %w", err)
	}
	slog.Debug("会话已持久化",
		"path", path,
		"session_id", data.SessionID)
	return nil
}

// sessionFilePath 返回会话文件的完整路径。
// 格式：{workspace}/.claudeclaw/sessions/{botName}/{chatID}/{topicID}.json
func sessionFilePath(workspace, botName string, chatID int64, topicID int) string {
	return filepath.Join(
		workspace,
		sessionDir,
		botName,
		fmt.Sprintf("%d", chatID),
		fmt.Sprintf("%d.json", topicID),
	)
}
