// Package runner 执行 claude CLI 命令，每个 workspace 维护一个串行队列，
// 确保同一目录下的任务按序执行，避免并发写文件冲突。
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/lustan3216/goclaudeclaw/internal/config"
	"github.com/lustan3216/goclaudeclaw/internal/memory"
	"github.com/lustan3216/goclaudeclaw/internal/session"
)

// Result 包含 claude 执行结果。
type Result struct {
	Output string
	Err    error
}

// Job 是提交给串行队列的任务单元。
type Job struct {
	Ctx       context.Context
	Workspace string
	BotName   string // bot 名称，用于会话键
	ChatID    int64  // Telegram chat ID
	TopicID   int    // Telegram topic ID（论坛话题），0 表示普通聊天
	Prompt    string
	Mode      TaskMode
	ResultCh  chan<- Result // 调用方监听此 channel 获取结果
}

// claudeJSONOutput claude --output-format json 的输出结构。
// 新会话时使用此格式捕获 session_id。
type claudeJSONOutput struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	// 其他字段按需扩展
}

// queueKey 唯一标识一条串行队列：每个 chat+topic 独立排队，互不阻塞。
type queueKey struct {
	workspace string
	botName   string
	chatID    int64
	topicID   int
}

// Manager 管理所有会话的串行执行队列。
// 每个 workspace+bot+chat+topic 独立一条队列，避免不同话题互相阻塞。
type Manager struct {
	mu         sync.Mutex
	queues     map[queueKey]chan Job
	sessions   *session.Manager
	classifier *Classifier
	cfg        *config.Config
	claudePath string
}

// NewManager 创建 Runner Manager。
func NewManager(cfg *config.Config, sessions *session.Manager, claudePath string) *Manager {
	return &Manager{
		queues:     make(map[queueKey]chan Job),
		sessions:   sessions,
		classifier: NewClassifier(claudePath),
		cfg:        cfg,
		claudePath: claudePath,
	}
}

// UpdateConfig 热重载时更新配置引用（调用方加锁保护）。
func (m *Manager) UpdateConfig(cfg *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// Submit 将任务提交到对应 chat+topic 的串行队列。
// 对于 ModeBackground 任务，resultCh 可传 nil（调用方不等待结果）。
func (m *Manager) Submit(job Job) {
	key := queueKey{job.Workspace, job.BotName, job.ChatID, job.TopicID}
	q := m.getOrCreateQueue(key)

	select {
	case q <- job:
		slog.Debug("任务已入队", "workspace", job.Workspace, "chat_id", job.ChatID, "topic_id", job.TopicID, "mode", job.Mode)
	case <-job.Ctx.Done():
		slog.Warn("任务入队前上下文已取消", "workspace", job.Workspace)
		if job.ResultCh != nil {
			job.ResultCh <- Result{Err: job.Ctx.Err()}
		}
	}
}

// getOrCreateQueue 获取或创建 chat+topic 对应的串行队列 goroutine。
func (m *Manager) getOrCreateQueue(key queueKey) chan Job {
	m.mu.Lock()
	defer m.mu.Unlock()

	if q, ok := m.queues[key]; ok {
		return q
	}

	// 缓冲大小 32：允许短时间内积压，防止 Telegram 消息丢失
	q := make(chan Job, 32)
	m.queues[key] = q
	go m.runQueue(key, q)
	slog.Info("为 chat+topic 创建串行执行队列", "workspace", key.workspace, "chat_id", key.chatID, "topic_id", key.topicID)
	return q
}

// runQueue 是每个 chat+topic 的串行执行 goroutine，
// 按序消费队列中的任务，直到 channel 被关闭。
func (m *Manager) runQueue(key queueKey, q <-chan Job) {
	for job := range q {
		result := m.execute(job)
		if job.ResultCh != nil {
			job.ResultCh <- result
		}
	}
}

// execute 实际执行 claude CLI，返回输出结果。
// 逻辑：
//   - 有已知 session → --output-format text --resume {sessionID}
//   - 无 session（新会话）→ --output-format json，从 JSON 输出解析 session_id 并持久化
func (m *Manager) execute(job Job) Result {
	sessionID := m.sessions.Get(job.Workspace, job.BotName, job.ChatID, job.TopicID)
	isNewSession := sessionID == ""

	// 新会话时注入本地记忆（省 token：续会话已有 context）
	// 使用 section 相关性评分，只注入与当前 prompt 相关的 section
	prompt := job.Prompt
	if isNewSession {
		localMem := memory.NewLocalMemory(job.Workspace)
		if memContent, err := localMem.LoadRelevant(job.Prompt); err != nil {
			slog.Warn("读取本地记忆失败，跳过注入", "err", err)
		} else if memContent != "" {
			prompt = memory.InjectPrefix(memContent, prompt)
			slog.Debug("已注入相关记忆", "memory_len", len(memContent))
		}
	}
	job.Prompt = prompt

	args := m.buildArgs(job, sessionID)

	slog.Info("执行 claude",
		"workspace", job.Workspace,
		"bot", job.BotName,
		"chat_id", job.ChatID,
		"topic_id", job.TopicID,
		"mode", job.Mode,
		"session_id", sessionID,
		"new_session", isNewSession)

	cmd := exec.CommandContext(job.Ctx, m.claudePath, args...)
	cmd.Dir = job.Workspace

	// 过滤掉 CLAUDECODE 环境变量，避免 claude 拒绝嵌套启动
	filtered := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered

	// 流式读取输出
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Err: fmt.Errorf("获取 stdout pipe 失败: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{Err: fmt.Errorf("获取 stderr pipe 失败: %w", err)}
	}

	if err := cmd.Start(); err != nil {
		return Result{Err: fmt.Errorf("启动 claude 失败: %w", err)}
	}

	// 并发读取 stdout 和 stderr
	var outputBuilder strings.Builder
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			outputBuilder.WriteString(line)
			outputBuilder.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Debug("claude stderr", "line", scanner.Text(), "workspace", job.Workspace)
		}
	}()

	wg.Wait()
	if err := cmd.Wait(); err != nil {
		// 上下文取消是正常情况，不视为错误
		if job.Ctx.Err() != nil {
			return Result{Err: fmt.Errorf("任务被取消: %w", job.Ctx.Err())}
		}
		return Result{
			Output: outputBuilder.String(),
			Err:    fmt.Errorf("claude 退出错误: %w", err),
		}
	}

	rawOutput := strings.TrimSpace(outputBuilder.String())

	// 新会话：解析 JSON 输出，提取 session_id
	if isNewSession {
		if jsonOut, err := parseJSONOutput(rawOutput); err == nil && jsonOut.SessionID != "" {
			if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, jsonOut.SessionID); err != nil {
				slog.Warn("持久化 session ID 失败", "err", err)
			} else {
				slog.Info("新会话已创建并持久化",
					"session_id", jsonOut.SessionID,
					"bot", job.BotName,
					"chat_id", job.ChatID,
					"topic_id", job.TopicID)
			}
			return Result{Output: strings.TrimSpace(jsonOut.Result)}
		}
		// JSON 解析失败时降级：尝试旧格式 session ID 提取
		if newID := extractSessionID(rawOutput); newID != "" {
			if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, newID); err != nil {
				slog.Warn("持久化 session ID 失败（降级模式）", "err", err)
			}
		}
		return Result{Output: rawOutput}
	}

	// 已有会话（--resume 模式）：纯文本输出，检查是否有新 session ID（轮换情况）
	if newID := extractSessionID(rawOutput); newID != "" && newID != sessionID {
		if err := m.sessions.Set(job.Workspace, job.BotName, job.ChatID, job.TopicID, newID); err != nil {
			slog.Warn("持久化新 session ID 失败", "err", err)
		}
	}

	return Result{Output: rawOutput}
}

// buildArgs 根据任务配置组装 claude 命令行参数。
func (m *Manager) buildArgs(job Job, sessionID string) []string {
	args := []string{}

	// 非互动模式必须跳过权限确认，否则 claude 会等待 terminal 输入而卡死。
	// locked 模式仍需跳过（通过 system prompt 约束行为，而非 terminal 确认）。
	args = append(args, "--dangerously-skip-permissions")

	if sessionID != "" {
		// 已有会话：使用文本输出格式恢复会话
		args = append(args, "--output-format", "text")
		args = append(args, "--resume", sessionID)
	} else {
		// 新会话：使用 JSON 输出格式，捕获 session_id
		args = append(args, "--output-format", "json")
	}

	// 单次 prompt 模式（非交互）
	args = append(args, "-p", job.Prompt)

	if job.Mode == ModeBackground {
		slog.Debug("后台任务，使用静默执行模式")
	}

	return args
}

// parseJSONOutput 解析 claude --output-format json 的输出。
// claude 可能输出多行，取最后一行（或第一个有效 JSON 对象）。
func parseJSONOutput(output string) (*claudeJSONOutput, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// 从最后一行往前找，取第一个能解析成功的 JSON
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var out claudeJSONOutput
		if err := json.Unmarshal([]byte(line), &out); err == nil {
			return &out, nil
		}
	}
	return nil, fmt.Errorf("未找到有效 JSON 输出")
}

// extractSessionID 从 claude 输出中提取会话 ID（旧格式兼容）。
// claude 输出格式可能为: [session: abc123] 或 Session ID: abc123
func extractSessionID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// 匹配 [session: <id>]
		if strings.HasPrefix(line, "[session:") && strings.HasSuffix(line, "]") {
			id := strings.TrimPrefix(line, "[session:")
			id = strings.TrimSuffix(id, "]")
			return strings.TrimSpace(id)
		}
		// 匹配 Session ID: <id>
		if strings.HasPrefix(line, "Session ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Session ID:"))
		}
	}
	return ""
}
