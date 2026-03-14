// Package session manages Claude session ID persistence for each bot/chat/topic.
// Session files are stored at workspace/.claudeclaw/sessions/{botName}/{chatID}/{topicID}.json,
// ensuring the --resume flag can restore the last conversation context after a restart.
// Each Telegram topic (forum thread) has its own Claude session; topicID=0 means a regular chat.
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

// SessionData is the JSON structure of a session file, compatible with claudeclaw format.
type SessionData struct {
	SessionID  string `json:"sessionId"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt"`
}

// sessionKey is the composite key for a session (bot + chat + topic).
type sessionKey struct {
	botName string
	chatID  int64
	topicID int
}

// Manager manages session IDs for multiple bot/chat/topic combinations; concurrency-safe.
type Manager struct {
	mu       sync.RWMutex
	sessions map[sessionKey]string // key → session ID (in-memory cache)
}

// New returns a new Manager instance.
func New() *Manager {
	return &Manager{
		sessions: make(map[sessionKey]string),
	}
}

// Get returns the session ID for the given bot/chat/topic, updating lastUsedAt and flushing to disk.
// Reads from in-memory cache first; falls back to disk on a cache miss.
// Returns an empty string if no session is known (caller should run without --resume).
func (m *Manager) Get(workspace, botName string, chatID int64, topicID int) string {
	key := sessionKey{botName, chatID, topicID}

	m.mu.RLock()
	id, ok := m.sessions[key]
	m.mu.RUnlock()
	if ok {
		// Update lastUsedAt
		_ = m.touchLastUsed(workspace, botName, chatID, topicID, id)
		return id
	}

	// Load from disk and populate cache
	data := m.load(workspace, botName, chatID, topicID)
	if data == nil {
		return ""
	}
	id = data.SessionID
	if id != "" {
		m.mu.Lock()
		m.sessions[key] = id
		m.mu.Unlock()
		// Update lastUsedAt
		_ = m.touchLastUsed(workspace, botName, chatID, topicID, id)
	}
	return id
}

// Set updates the session ID for the given bot/chat/topic and persists it to disk.
func (m *Manager) Set(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	key := sessionKey{botName, chatID, topicID}
	m.mu.Lock()
	m.sessions[key] = sessionID
	m.mu.Unlock()
	return m.save(workspace, botName, chatID, topicID, sessionID)
}

// Clear removes the session record for a bot/chat/topic (next run will start a new session).
func (m *Manager) Clear(workspace, botName string, chatID int64, topicID int) error {
	key := sessionKey{botName, chatID, topicID}
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	path := sessionFilePath(workspace, botName, chatID, topicID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove session file %s: %w", path, err)
	}
	slog.Info("session cleared",
		"workspace", workspace,
		"bot", botName,
		"chat_id", chatID,
		"topic_id", topicID)
	return nil
}

// touchLastUsed updates the lastUsedAt field in the session file.
func (m *Manager) touchLastUsed(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	path := sessionFilePath(workspace, botName, chatID, topicID)
	data, err := readSessionFile(path)
	if err != nil || data == nil {
		// Not an error if the file doesn't exist
		return nil
	}
	data.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	return writeSessionFile(path, data)
}

// load reads session data from disk.
func (m *Manager) load(workspace, botName string, chatID int64, topicID int) *SessionData {
	path := sessionFilePath(workspace, botName, chatID, topicID)
	data, err := readSessionFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read session file", "path", path, "err", err)
		}
		return nil
	}
	slog.Debug("session loaded from disk",
		"bot", botName,
		"chat_id", chatID,
		"topic_id", topicID,
		"session_id", data.SessionID)
	return data
}

// save writes the session ID to disk (atomic write: write temp file then rename).
// Preserves createdAt if the file already exists; otherwise uses the current time.
func (m *Manager) save(workspace, botName string, chatID int64, topicID int, sessionID string) error {
	path := sessionFilePath(workspace, botName, chatID, topicID)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Read existing file to preserve createdAt
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

// readSessionFile reads and parses a JSON session file.
func readSessionFile(path string) (*SessionData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data SessionData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to parse session file: %w", err)
	}
	return &data, nil
}

// writeSessionFile atomically writes a JSON session file.
func writeSessionFile(path string, data *SessionData) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize session data: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return fmt.Errorf("failed to write temp session file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename session file: %w", err)
	}
	slog.Debug("session persisted",
		"path", path,
		"session_id", data.SessionID)
	return nil
}

// sessionFilePath returns the full path to a session file.
// Format: {workspace}/.claudeclaw/sessions/{botName}/{chatID}/{topicID}.json
func sessionFilePath(workspace, botName string, chatID int64, topicID int) string {
	return filepath.Join(
		workspace,
		sessionDir,
		botName,
		fmt.Sprintf("%d", chatID),
		fmt.Sprintf("%d.json", topicID),
	)
}
