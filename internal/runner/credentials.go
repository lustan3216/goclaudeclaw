package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lustan3216/claudeclaw/internal/config"
)

// credentialsPath returns the path to ~/.claude/.credentials.json.
func credentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}

// swapCredential replaces the claudeAiOauth block in ~/.claude/.credentials.json
// with the provided credential, preserving all other keys (e.g. mcpOAuth).
// The write is atomic (temp file + rename).
func swapCredential(cred config.ClaudeCredential) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}

	// Read existing file; start with empty map if missing
	var root map[string]json.RawMessage
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("failed to parse credentials file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to read credentials file: %w", err)
	}
	if root == nil {
		root = make(map[string]json.RawMessage)
	}

	// Marshal the new claudeAiOauth block
	oauthBytes, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("failed to marshal credential: %w", err)
	}
	root["claudeAiOauth"] = json.RawMessage(oauthBytes)

	// Write atomically
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode credentials: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create .claude dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write temp credentials: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to replace credentials file: %w", err)
	}
	return nil
}
