// Package config 负责加载、验证和热重载 YAML 配置文件。
// 使用 viper 实现，支持 fsnotify 监听文件变化，30 秒内自动生效。
package config

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// BotConfig 单个 Telegram Bot 的配置。
type BotConfig struct {
	Name                string  `mapstructure:"name"`
	Token               string  `mapstructure:"token"`
	AllowedUsers        []int64 `mapstructure:"allowed_users"`
	DebounceMs          int     `mapstructure:"debounce_ms"`
	OpenAIAPIKey        string  `mapstructure:"openai_api_key"`        // Whisper 语音转文字，留空则读 OPENAI_API_KEY 环境变量
	MemoryUpdateInterval      int `mapstructure:"memory_update_interval"`      // 每 N 次成功完成后更新 memory.md，0 = 禁用
	SessionSummarizeInterval  int `mapstructure:"session_summarize_interval"`  // 每 N 次成功完成后摘要对话并重置 session，0 = 禁用
	MemoryCompressInterval    int `mapstructure:"memory_compress_interval"`    // 每 N 次 memory 更新后压缩 memory.md，0 = 禁用
}

// MCPsConfig 预置 MCP 服务器配置，token 留空则不启用该服务器。
type MCPsConfig struct {
	GitHub  MCPGitHubConfig  `mapstructure:"github"`
	Notion  MCPNotionConfig  `mapstructure:"notion"`
	Browser MCPBrowserConfig `mapstructure:"browser"`
	Brave   MCPBraveConfig   `mapstructure:"brave"`
}

// MCPGitHubConfig GitHub MCP 服务器（@modelcontextprotocol/server-github）。
type MCPGitHubConfig struct {
	Token string `mapstructure:"token"` // GitHub personal access token，留空则禁用
}

// MCPNotionConfig Notion MCP 服务器（@notionhq/notion-mcp-server）。
type MCPNotionConfig struct {
	Token string `mapstructure:"token"` // Notion integration token，留空则禁用
}

// MCPBrowserConfig 浏览器自动化 MCP（@modelcontextprotocol/server-puppeteer）。
type MCPBrowserConfig struct {
	Enabled bool `mapstructure:"enabled"` // true 启用，无需 token
}

// MCPBraveConfig Brave 搜索 MCP（@modelcontextprotocol/server-brave-search）。
type MCPBraveConfig struct {
	APIKey string `mapstructure:"api_key"` // Brave Search API key，留空则禁用
}

// QuietWindow 定义心跳静默时间段（本地时间）。
type QuietWindow struct {
	Start string `mapstructure:"start"` // "23:00"
	End   string `mapstructure:"end"`   // "08:00"
}

// HeartbeatConfig 心跳/定时提示配置。
type HeartbeatConfig struct {
	Enabled         bool          `mapstructure:"enabled"`
	IntervalMinutes int           `mapstructure:"interval_minutes"`
	Prompt          string        `mapstructure:"prompt"`
	QuietWindows    []QuietWindow `mapstructure:"quiet_windows"`
	Timezone        string        `mapstructure:"timezone"`
	ChatID          int64         `mapstructure:"chat_id"`   // 发送心跳结果的 Telegram chat ID（必填）
	TopicID         int           `mapstructure:"topic_id"`  // 发送到论坛 topic（0 = 普通聊天）
}

// MemoryConfig claude-mem / mem0 集成配置。
type MemoryConfig struct {
	Provider string `mapstructure:"provider"` // "claude-mem" | "mem0"
	Endpoint string `mapstructure:"endpoint"`
}

// SecurityConfig claude 执行权限级别。
type SecurityConfig struct {
	// Level 可选值: locked | strict | moderate | unrestricted
	// - locked:        仅允许只读操作
	// - strict:        需要用户确认每个工具调用
	// - moderate:      允许大多数操作，危险操作需确认（默认）
	// - unrestricted:  跳过所有权限检查（--dangerously-skip-permissions）
	Level string `mapstructure:"level"`
}

// WebConfig 内置 HTTP 管理接口配置。
type WebConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Host    string `mapstructure:"host"`
	Port    int    `mapstructure:"port"`
}

// CronJob 单条 cron 任务配置（markdown frontmatter 格式）。
type CronJob struct {
	Name     string `mapstructure:"name"`
	Schedule string `mapstructure:"schedule"` // cron 表达式，如 "0 9 * * *"
	Prompt   string `mapstructure:"prompt"`
	Workspace string `mapstructure:"workspace"` // 留空则使用全局 workspace
}

// Config 全局配置结构体。
type Config struct {
	Workspace string          `mapstructure:"workspace"`
	Bots      []BotConfig     `mapstructure:"bots"`
	Memory    MemoryConfig    `mapstructure:"memory"`
	Heartbeat HeartbeatConfig `mapstructure:"heartbeat"`
	Security  SecurityConfig  `mapstructure:"security"`
	Web       WebConfig       `mapstructure:"web"`
	CronJobs  []CronJob       `mapstructure:"cron_jobs"`
	MCPs      MCPsConfig      `mapstructure:"mcps"`
}

// Manager 持有当前配置并支持热重载。
// 所有读取操作通过 Get() 方法，保证并发安全。
type Manager struct {
	mu      sync.RWMutex
	current *Config
	viper   *viper.Viper

	// onChange 在配置成功重载后触发，允许上层组件响应变更。
	onChange []func(newCfg *Config)
}

// New 从指定路径加载配置文件，并启动 fsnotify 热重载监听。
// configPath 可以是绝对路径或相对路径，支持 .yaml / .toml 格式。
func New(configPath string) (*Manager, error) {
	v := viper.New()
	v.SetConfigFile(configPath)
	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg, err := decode(v)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		current: cfg,
		viper:   v,
	}

	// 启动热重载：viper 通过 fsnotify 监听文件变化
	v.OnConfigChange(func(e fsnotify.Event) {
		slog.Info("配置文件变更，重新加载", "file", e.Name, "op", e.Op.String())
		newCfg, err := decode(v)
		if err != nil {
			slog.Error("配置重载失败，保留旧配置", "err", err)
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
		slog.Info("配置重载成功")
	})
	v.WatchConfig()

	return m, nil
}

// Get 返回当前配置的只读副本，并发安全。
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 返回浅拷贝，防止调用方意外修改
	c := *m.current
	return &c
}

// OnChange 注册配置变更回调，热重载时调用。
func (m *Manager) OnChange(fn func(newCfg *Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

// setDefaults 设置 viper 默认值，避免配置文件缺字段时 panic。
func setDefaults(v *viper.Viper) {
	v.SetDefault("workspace", ".")
	v.SetDefault("heartbeat.enabled", false)
	v.SetDefault("heartbeat.interval_minutes", 15)
	v.SetDefault("heartbeat.timezone", "UTC")
	v.SetDefault("heartbeat.prompt", "Check pending tasks and summarize progress.")
	v.SetDefault("security.level", "moderate")
	v.SetDefault("web.enabled", false)
	v.SetDefault("web.host", "127.0.0.1")
	v.SetDefault("web.port", 4632)
	v.SetDefault("memory.provider", "claude-mem")
	v.SetDefault("memory.endpoint", "http://localhost:8080")
}

// decode 将 viper 当前状态解码为 Config 结构体，并做基本校验。
func decode(v *viper.Viper) (*Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}
	return &cfg, nil
}

// validate 对关键字段做合法性检查。
func validate(cfg *Config) error {
	if len(cfg.Bots) == 0 {
		return fmt.Errorf("至少需要配置一个 bot")
	}
	for i, b := range cfg.Bots {
		if b.Token == "" {
			return fmt.Errorf("bots[%d] (%s) token 不能为空", i, b.Name)
		}
		if len(b.AllowedUsers) == 0 {
			return fmt.Errorf("bots[%d] (%s) allowed_users 不能为空（防止公开访问）", i, b.Name)
		}
	}
	switch cfg.Security.Level {
	case "locked", "strict", "moderate", "unrestricted":
	default:
		return fmt.Errorf("security.level 无效值: %q，可选: locked | strict | moderate | unrestricted", cfg.Security.Level)
	}
	return nil
}
