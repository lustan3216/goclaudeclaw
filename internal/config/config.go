// Package config handles loading, validating, and hot-reloading of YAML config files.
// Uses viper with fsnotify file watching; changes take effect automatically within 30 seconds.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// ClaudeCredential holds a single Claude OAuth credential set (claudeAiOauth).
// Used for multi-account fallback: on rate-limit/auth failure, the runner swaps
// ~/.claude/.credentials.json to the next credential and retries.
type ClaudeCredential struct {
	AccessToken      string   `mapstructure:"access_token"      json:"accessToken"`
	RefreshToken     string   `mapstructure:"refresh_token"     json:"refreshToken"`
	ExpiresAt        int64    `mapstructure:"expires_at"        json:"expiresAt,omitempty"`
	Scopes           []string `mapstructure:"scopes"            json:"scopes,omitempty"`
	SubscriptionType string   `mapstructure:"subscription_type" json:"subscriptionType,omitempty"`
	RateLimitTier    string   `mapstructure:"rate_limit_tier"   json:"rateLimitTier,omitempty"`
}

// BotConfig holds configuration for a single Telegram Bot.
type BotConfig struct {
	Name                string             `mapstructure:"name"`
	Token               string             `mapstructure:"token"`
	AllowedUsers        []int64            `mapstructure:"allowed_users"`
	OpenAIAPIKey        string             `mapstructure:"openai_api_key"`        // Whisper voice transcription; reads OPENAI_API_KEY env var if empty
	AnthropicAPIKeys    []string           `mapstructure:"anthropic_api_keys"`    // multiple keys tried in order; fallback on rate-limit/quota/auth errors
	ClaudeCredentials   []ClaudeCredential `mapstructure:"claude_credentials"`   // OAuth credential sets for multi-account fallback
	MemoryUpdateInterval   int `mapstructure:"memory_update_interval"`   // update memory.md every N successful completions; 0 = disabled
	MemoryCompressInterval int `mapstructure:"memory_compress_interval"` // compress memory.md every N memory updates; 0 = disabled
	MaxSessionTokens       int `mapstructure:"max_session_tokens"`       // reset session when input tokens exceed this; default 60000
}

// QuietWindow defines a heartbeat quiet period (local time).
type QuietWindow struct {
	Start string `mapstructure:"start"` // "23:00"
	End   string `mapstructure:"end"`   // "08:00"
}

// HeartbeatConfig holds heartbeat/scheduled prompt configuration.
type HeartbeatConfig struct {
	Enabled         bool          `mapstructure:"enabled"`
	IntervalMinutes int           `mapstructure:"interval_minutes"`
	Prompt          string        `mapstructure:"prompt"`
	QuietWindows    []QuietWindow `mapstructure:"quiet_windows"`
	Timezone        string        `mapstructure:"timezone"`
	ChatID          int64         `mapstructure:"chat_id"`   // Telegram chat ID to send heartbeat results to (required)
	TopicID         int           `mapstructure:"topic_id"`  // send to forum topic (0 = regular chat)
}

// SecurityConfig holds claude execution permission level.
type SecurityConfig struct {
	// Level options: locked | strict | moderate | unrestricted
	// - locked:        allow read-only operations only
	// - strict:        require user confirmation for every tool call
	// - moderate:      allow most operations, confirm dangerous ones (default)
	// - unrestricted:  skip all permission checks (--dangerously-skip-permissions)
	Level string `mapstructure:"level"`
}

// WebConfig holds built-in HTTP admin interface configuration.
type WebConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Host    string `mapstructure:"host"`
	Port    int    `mapstructure:"port"`
}

// CronJob holds configuration for a single cron job (markdown frontmatter format).
type CronJob struct {
	Name     string `mapstructure:"name"`
	Schedule string `mapstructure:"schedule"` // cron expression, e.g. "0 9 * * *"
	Prompt   string `mapstructure:"prompt"`
	Workspace string `mapstructure:"workspace"` // empty = use global workspace
}

// Config is the global configuration struct.
type Config struct {
	Workspace  string          `mapstructure:"workspace"`
	AutoUpdate bool            `mapstructure:"auto_update"` // true = run.sh watchdog auto git pull + rebuild before each restart
	Bots       []BotConfig     `mapstructure:"bots"`
	Heartbeat  HeartbeatConfig `mapstructure:"heartbeat"`
	Security   SecurityConfig  `mapstructure:"security"`
	Web        WebConfig       `mapstructure:"web"`
	CronJobs   []CronJob       `mapstructure:"cron_jobs"`
}

// Manager holds the current config and supports hot-reloading.
// All reads go through the Get() method for concurrency safety.
type Manager struct {
	mu      sync.RWMutex
	current *Config
	viper   *viper.Viper
	path    string // absolute path to the config file, used by ClaimOwner for atomic JSON writes

	// onChange is called after a successful config reload, allowing upper-layer components to react to changes.
	onChange []func(newCfg *Config)
}

// New loads the config file from the specified path and starts fsnotify hot-reload watching.
// configPath can be absolute or relative; supports .yaml / .toml formats.
func New(configPath string) (*Manager, error) {
	v := viper.New()
	v.SetConfigFile(configPath)
	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg, err := decode(v)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		current: cfg,
		viper:   v,
		path:    configPath,
	}

	// Start hot-reload: viper watches for file changes via fsnotify
	v.OnConfigChange(func(e fsnotify.Event) {
		slog.Info("config file changed, reloading", "file", e.Name, "op", e.Op.String())
		newCfg, err := decode(v)
		if err != nil {
			slog.Error("config reload failed, keeping old config", "err", err)
			return
		}
		m.mu.Lock()
		m.current = newCfg
		handlers := make([]func(*Config), len(m.onChange))
		copy(handlers, m.onChange)
		m.mu.Unlock()

		for _, fn := range handlers {
			fn(newCfg)
		}
		slog.Info("config reloaded successfully")
	})
	v.WatchConfig()

	return m, nil
}

// Get returns a read-only copy of the current config; concurrency-safe.
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Return a shallow copy to prevent callers from accidentally modifying the internal state
	c := *m.current
	return &c
}

// OnChange registers a config-change callback, called on hot-reload.
func (m *Manager) OnChange(fn func(newCfg *Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

// keyAliases maps user-friendly short names to viper config paths.
// Used by Telegram /set /unset /config commands.
var keyAliases = map[string]string{
	"auto_update":    "auto_update",
	"security_level": "security.level",
}

// Set sets a config value by viper path or user-friendly alias, writes to file, and triggers hot-reload callbacks.
func (m *Manager) Set(keyOrAlias, value string) error {
	viperKey := keyOrAlias
	if mapped, ok := keyAliases[keyOrAlias]; ok {
		viperKey = mapped
	}
	m.viper.Set(viperKey, value)
	if err := m.viper.WriteConfig(); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	newCfg, err := decode(m.viper)
	if err != nil {
		return fmt.Errorf("config parse failed: %w", err)
	}
	m.mu.Lock()
	m.current = newCfg
	handlers := make([]func(*Config), len(m.onChange))
	copy(handlers, m.onChange)
	m.mu.Unlock()
	for _, fn := range handlers {
		fn(newCfg)
	}
	return nil
}

// KnownAliases returns a copy of all supported user-friendly aliases (copy prevents accidental modification of the internal map).
func KnownAliases() map[string]string {
	out := make(map[string]string, len(keyAliases))
	for k, v := range keyAliases {
		out[k] = v
	}
	return out
}

// ClaimOwner appends userID to the named bot's allowed_users list, persisting the change atomically.
// It is safe to call for both first-owner claiming and /adduser additions.
// The method operates on raw JSON to avoid viper's unreliable nested-array serialisation.
func (m *Manager) ClaimOwner(botName string, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 读取原始 JSON 文件
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// 解析为通用 map 以便安全操作嵌套数组
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("failed to parse config JSON: %w", err)
	}

	// 定位目标 bot 并追加 userID
	botsRaw, _ := root["bots"].([]any)
	found := false
	for _, b := range botsRaw {
		botMap, ok := b.(map[string]any)
		if !ok {
			continue
		}
		name, _ := botMap["name"].(string)
		if name != botName {
			continue
		}

		// 检查是否已存在，避免重复
		existing, _ := botMap["allowed_users"].([]any)
		for _, v := range existing {
			switch id := v.(type) {
			case float64:
				if int64(id) == userID {
					return nil // 已存在，幂等返回
				}
			case int64:
				if id == userID {
					return nil
				}
			}
		}
		botMap["allowed_users"] = append(existing, userID)
		found = true
		break
	}
	if !found {
		return fmt.Errorf("bot %q not found in config", botName)
	}

	// 序列化并原子写回（写临时文件后 rename）
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	tmpPath := m.path + ".tmp"
	if err := os.WriteFile(tmpPath, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write temp config: %w", err)
	}
	if err := os.Rename(tmpPath, m.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically replace config: %w", err)
	}

	// 同步更新内存状态（fsnotify 也会在约 1 秒内触发热重载）
	if err := m.viper.ReadInConfig(); err == nil {
		if newCfg, err := decode(m.viper); err == nil {
			m.current = newCfg
			handlers := make([]func(*Config), len(m.onChange))
			copy(handlers, m.onChange)
			// 在持有锁之外调用回调，避免死锁
			go func() {
				for _, fn := range handlers {
					fn(newCfg)
				}
			}()
		}
	}

	slog.Info("ClaimOwner: user added to allowed_users", "bot", botName, "user_id", userID)
	return nil
}

// setDefaults sets viper default values to prevent panics when config fields are missing.
func setDefaults(v *viper.Viper) {
	v.SetDefault("workspace", ".")
	v.SetDefault("auto_update", true)
	v.SetDefault("heartbeat.enabled", false)
	v.SetDefault("heartbeat.interval_minutes", 15)
	v.SetDefault("heartbeat.timezone", "UTC")
	v.SetDefault("heartbeat.prompt", "Check pending tasks and summarize progress.")
	v.SetDefault("security.level", "moderate")
	v.SetDefault("web.enabled", false)
	v.SetDefault("web.host", "127.0.0.1")
	v.SetDefault("web.port", 4632)
}

// decode decodes the current viper state into a Config struct and performs basic validation.
func decode(v *viper.Viper) (*Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config parse failed: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return &cfg, nil
}

// validate performs validity checks on key fields.
func validate(cfg *Config) error {
	if len(cfg.Bots) == 0 {
		return fmt.Errorf("at least one bot must be configured")
	}
	for i, b := range cfg.Bots {
		if b.Token == "" {
			return fmt.Errorf("bots[%d] (%s) token must not be empty", i, b.Name)
		}
		// allowed_users may be empty — bot enters "awaiting owner" mode where the first sender becomes owner
	}
	switch cfg.Security.Level {
	case "locked", "strict", "moderate", "unrestricted":
	default:
		return fmt.Errorf("invalid security.level: %q, valid options: locked | strict | moderate | unrestricted", cfg.Security.Level)
	}
	return nil
}
