// Package bot 实现 Telegram 消息路由、防抖和前台/后台任务分发。
// 支持 Telegram 论坛话题（Forum Topics）：每个 topic 拥有独立的 Claude 会话，
// topicID=0 表示普通聊天（非话题消息）。
package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"

	"github.com/lustan3216/goclaudeclaw/internal/config"
	"github.com/lustan3216/goclaudeclaw/internal/runner"
	"github.com/lustan3216/goclaudeclaw/internal/session"
)

// incomingMsg 是防抖窗口内收集的原始消息。
type incomingMsg struct {
	text       string
	from       string
	chatID     int64
	topicID    int // 0 = 普通聊天，>0 = 论坛话题 ID
	messageID  int // 用于回复指定消息
	receivedAt time.Time
}

// chatTopicKey 唯一标识一个 chat+topic 的防抖/会话键。
type chatTopicKey struct {
	chatID  int64
	topicID int
}

// debounceState 跟踪每个 chat+topic 的防抖状态。
type debounceState struct {
	timer    *time.Timer
	messages []incomingMsg
	mu       sync.Mutex
}

// Dispatcher 负责消息路由和防抖聚合。
// 每个 bot 实例共享同一个 Dispatcher，通过 chatID+topicID 区分会话。
type Dispatcher struct {
	mu       sync.Mutex
	debounce map[chatTopicKey]*debounceState // chat+topic → 防抖状态

	countsMu         sync.Mutex
	completionCounts map[chatTopicKey]int // chat+topic → 成功完成次数（用于触发记忆更新/摘要）
	memUpdateCount   int                  // 全局 memory 更新次数（用于触发 memory.md 压缩）

	runnerMgr  *runner.Manager
	sessionMgr *session.Manager
	classifier *runner.Classifier
	cfg        *config.Config
	botCfg     config.BotConfig
	botAPI     *telego.Bot
	workspace  string
}

// NewDispatcher 创建消息分发器。
func NewDispatcher(
	botAPI *telego.Bot,
	botCfg config.BotConfig,
	cfg *config.Config,
	runnerMgr *runner.Manager,
	sessionMgr *session.Manager,
	workspace string,
) *Dispatcher {
	return &Dispatcher{
		debounce:         make(map[chatTopicKey]*debounceState),
		completionCounts: make(map[chatTopicKey]int),
		runnerMgr:        runnerMgr,
		sessionMgr: sessionMgr,
		classifier: runner.NewClassifier("claude"),
		cfg:        cfg,
		botCfg:     botCfg,
		botAPI:     botAPI,
		workspace:  workspace,
	}
}

// UpdateConfig 热重载时更新配置（调用方应在配置变更回调中调用）。
func (d *Dispatcher) UpdateConfig(cfg *config.Config, botCfg config.BotConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
	d.botCfg = botCfg
}

// Handle 接收来自 Telegram 的单条消息，进入防抖队列。
func (d *Dispatcher) Handle(ctx context.Context, update telego.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message

	// 鉴权：只处理 allowed_users 中的用户
	if msg.From == nil || !d.isAllowed(msg.From.ID) {
		if msg.From != nil {
			slog.Warn("拒绝未授权用户",
				"user_id", msg.From.ID,
				"username", msg.From.Username,
				"bot", d.botCfg.Name)
		}
		return
	}

	// 提取 topic ID：论坛话题消息时使用 MessageThreadID，否则为 0
	topicID := 0
	if msg.IsTopicMessage {
		topicID = msg.MessageThreadID
	}

	// 处理论坛话题生命周期事件（服务消息，无文本内容）
	if msg.ForumTopicCreated != nil {
		topicName := msg.ForumTopicCreated.Name
		threadID := msg.MessageThreadID
		slog.Info("新 topic 已建立",
			"topic_name", topicName,
			"thread_id", threadID,
			"chat_id", msg.Chat.ID)
		// session 懒创建，首条真实消息到来时自动建立
		d.reply(msg.Chat.ID, threadID, "✓ 已就緒 — 這個 topic 有獨立的對話 session")
		return
	}

	if msg.ForumTopicClosed != nil {
		slog.Info("topic 已关闭",
			"thread_id", msg.MessageThreadID,
			"chat_id", msg.Chat.ID)
		// session 保留在存储中，无需其他操作
		return
	}

	if msg.ForumTopicReopened != nil {
		slog.Info("topic 已重新开启",
			"thread_id", msg.MessageThreadID,
			"chat_id", msg.Chat.ID)
		d.reply(msg.Chat.ID, msg.MessageThreadID, "✓ Topic 已重新開啟，繼續原有 session")
		return
	}

	// 处理内置命令（telego 没有 IsCommand/Command 方法，手动检测）
	if cmd, args, ok := parseCommand(msg); ok {
		d.handleCommand(ctx, msg, topicID, cmd, args)
		return
	}

	// 处理语音消息：下载 ogg 文件后调用 Whisper API 转文字
	if msg.Voice != nil {
		voiceText, err := d.transcribeVoice(msg.Voice.FileID, msg.Chat.ID)
		if err != nil {
			slog.Error("语音转文字失败", "err", err, "chat_id", msg.Chat.ID)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ 语音转文字失败: %v", err))
			return
		}
		text := "[语音转文字]: " + voiceText
		d.enqueueWithDebounce(ctx, chatTopicKey{msg.Chat.ID, topicID}, incomingMsg{
			text:       text,
			from:       msg.From.Username,
			chatID:     msg.Chat.ID,
			topicID:    topicID,
			messageID:  msg.MessageID,
			receivedAt: time.Now(),
		})
		return
	}

	// 处理图片消息：取最高分辨率的 PhotoSize，base64 编码嵌入 prompt 供 Claude Vision 使用
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1] // 最后一项分辨率最高
		savedPath, err := d.downloadTelegramFile(photo.FileID, msg.Chat.ID, "photo.jpg")
		if err != nil {
			slog.Error("下载图片失败", "err", err, "chat_id", msg.Chat.ID)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ 图片下载失败: %v", err))
			return
		}
		caption := strings.TrimSpace(msg.Caption)

		// 尝试读取并 base64 编码图片；超过 5MB 则退回文件路径模式
		const maxImageBytes = 5 * 1024 * 1024
		var text string
		if imgBytes, readErr := os.ReadFile(savedPath); readErr == nil && len(imgBytes) <= maxImageBytes {
			b64 := base64.StdEncoding.EncodeToString(imgBytes)
			text = fmt.Sprintf("[图片]\n<image>\n<media_type>image/jpeg</media_type>\n<data>%s</data>\n</image>", b64)
		} else {
			// 文件过大或读取失败，退回路径模式
			if readErr != nil {
				slog.Warn("读取图片文件失败，退回路径模式", "err", readErr, "path", savedPath)
			} else {
				slog.Warn("图片超过 5MB，退回路径模式", "size", len(imgBytes), "path", savedPath)
			}
			text = fmt.Sprintf("[用户发送了图片: %s]", savedPath)
		}
		if caption != "" {
			text += "\n" + caption
		}
		d.enqueueWithDebounce(ctx, chatTopicKey{msg.Chat.ID, topicID}, incomingMsg{
			text:       text,
			from:       msg.From.Username,
			chatID:     msg.Chat.ID,
			topicID:    topicID,
			messageID:  msg.MessageID,
			receivedAt: time.Now(),
		})
		return
	}

	// 处理文件消息（PDF、文档等通用文件）
	if msg.Document != nil {
		doc := msg.Document
		filename := doc.FileName
		if filename == "" {
			filename = doc.FileUniqueID // 无原始文件名时用 unique ID 代替
		}
		savedPath, err := d.downloadTelegramFile(doc.FileID, msg.Chat.ID, filename)
		if err != nil {
			slog.Error("下载文件失败", "err", err, "chat_id", msg.Chat.ID, "filename", filename)
			d.reply(msg.Chat.ID, topicID, fmt.Sprintf("❌ 文件下载失败: %v", err))
			return
		}
		caption := strings.TrimSpace(msg.Caption)
		mimeType := doc.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		var text string
		if mimeType == "application/pdf" {
			text = fmt.Sprintf("[用户发送了PDF文件，请使用Read工具查看内容: %s]", savedPath)
		} else {
			text = fmt.Sprintf("[用户发送了文件: %s (%s)]", savedPath, mimeType)
		}
		if caption != "" {
			text += "\n" + caption
		}
		d.enqueueWithDebounce(ctx, chatTopicKey{msg.Chat.ID, topicID}, incomingMsg{
			text:       text,
			from:       msg.From.Username,
			chatID:     msg.Chat.ID,
			topicID:    topicID,
			messageID:  msg.MessageID,
			receivedAt: time.Now(),
		})
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	d.enqueueWithDebounce(ctx, chatTopicKey{msg.Chat.ID, topicID}, incomingMsg{
		text:       text,
		from:       msg.From.Username,
		chatID:     msg.Chat.ID,
		topicID:    topicID,
		messageID:  msg.MessageID,
		receivedAt: time.Now(),
	})
}

// parseCommand 检测消息是否为 bot 命令。
// 返回命令名称（不含斜杠）、参数字符串，以及是否为命令的布尔值。
// telego 不提供 IsCommand/Command 辅助方法，需手动解析 Entities。
func parseCommand(msg *telego.Message) (cmd string, args string, ok bool) {
	if !strings.HasPrefix(msg.Text, "/") {
		return "", "", false
	}
	// 确认第一个 entity 类型为 bot_command
	for _, e := range msg.Entities {
		if e.Type == telego.EntityTypeBotCommand && e.Offset == 0 {
			// 截取命令部分，例如 "/clear@botname" → "clear"
			cmdFull := msg.Text[1:e.Length] // 去掉前缀 "/"
			if at := strings.IndexByte(cmdFull, '@'); at >= 0 {
				cmdFull = cmdFull[:at]
			}
			// 参数为命令后的剩余文本（去除首尾空白）
			rest := strings.TrimSpace(msg.Text[e.Length:])
			return cmdFull, rest, true
		}
	}
	return "", "", false
}

// handleCommand 处理 /start /help /clear /status /bg 等内置命令。
func (d *Dispatcher) handleCommand(ctx context.Context, msg *telego.Message, topicID int, cmd string, args string) {
	chatID := msg.Chat.ID
	switch cmd {
	case "start", "help":
		d.reply(chatID, topicID, "👋 goclaudeclaw 已就绪\n\n"+
			"发送任意消息即可与 Claude 对话。\n"+
			"命令:\n"+
			"  /clear — 清除当前会话\n"+
			"  /status — 查看运行状态\n"+
			"  /bg <任务> — 强制以后台模式运行")
	case "clear":
		if err := d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID); err != nil {
			slog.Error("清除会话失败", "err", err, "chat_id", chatID, "topic_id", topicID)
			d.reply(chatID, topicID, fmt.Sprintf("❌ 清除会话失败: %v", err))
			return
		}
		d.reply(chatID, topicID, "✓ 会话已清除，下次对话将开启新会话。")
	case "status":
		topicInfo := "无（普通聊天）"
		if topicID > 0 {
			topicInfo = fmt.Sprintf("Topic #%d", topicID)
		}
		d.reply(chatID, topicID, fmt.Sprintf(
			"Bot: %s\nWorkspace: %s\nSecurity: %s\nTopic: %s",
			d.botCfg.Name, d.workspace, d.cfg.Security.Level, topicInfo,
		))
	case "bg":
		// 强制后台模式
		if args == "" {
			d.reply(chatID, topicID, "用法: /bg <任务描述>")
			return
		}
		d.dispatchJob(ctx, chatID, topicID, msg.MessageID, args, runner.ModeBackground)
	default:
		d.reply(chatID, topicID, "未知命令，发送 /help 查看帮助。")
	}
}

// enqueueWithDebounce 将消息加入防抖窗口。
// 在 debounce_ms 内连续到达的同一 chat+topic 消息会被合并为一条发给 claude。
func (d *Dispatcher) enqueueWithDebounce(ctx context.Context, key chatTopicKey, msg incomingMsg) {
	debounceMs := d.botCfg.DebounceMs
	if debounceMs <= 0 {
		debounceMs = 1500 // 默认 1.5s
	}
	delay := time.Duration(debounceMs) * time.Millisecond

	d.mu.Lock()
	state, ok := d.debounce[key]
	if !ok {
		state = &debounceState{}
		d.debounce[key] = state
	}
	d.mu.Unlock()

	state.mu.Lock()
	defer state.mu.Unlock()

	state.messages = append(state.messages, msg)

	// 重置计时器：新消息到来时重新计时
	if state.timer != nil {
		state.timer.Stop()
	}
	state.timer = time.AfterFunc(delay, func() {
		state.mu.Lock()
		msgs := state.messages
		state.messages = nil
		state.mu.Unlock()

		if len(msgs) == 0 {
			return
		}

		combined := combineMessages(msgs)
		slog.Info("防抖窗口触发",
			"chat_id", key.chatID,
			"topic_id", key.topicID,
			"message_count", len(msgs),
			"combined_len", len(combined),
			"bot", d.botCfg.Name)

		// 取最后一条消息的 ID 作为回复目标
		lastMsgID := msgs[len(msgs)-1].messageID

		// 异步分类和分发，不阻塞防抖 goroutine
		go func() {
			mode := d.classifier.Classify(ctx, combined)
			d.dispatchJob(ctx, key.chatID, key.topicID, lastMsgID, combined, mode)
		}()
	})
}

// dispatchJob 将任务提交到 runner，并处理 Telegram 回复。
// replyToID 为触发本次任务的最后一条消息 ID，回复时 quote 该消息。
func (d *Dispatcher) dispatchJob(ctx context.Context, chatID int64, topicID int, replyToID int, prompt string, mode runner.TaskMode) {
	// 收到訊息先打 👀
	d.react(chatID, replyToID, "👀")

	// 后台任务：立即回复用户，异步执行
	if mode == runner.ModeBackground {
		d.replyTo(chatID, topicID, replyToID, "⏳ 已在后台处理，完成后通知你。")

		resultCh := make(chan runner.Result, 1)
		d.runnerMgr.Submit(runner.Job{
			Ctx:       ctx,
			Workspace: d.workspace,
			BotName:   d.botCfg.Name,
			ChatID:    chatID,
			TopicID:   topicID,
			Prompt:    prompt,
			Mode:      mode,
			ResultCh:  resultCh,
		})

		go func() {
			result := <-resultCh
			if result.Err != nil {
				d.replyTo(chatID, topicID, replyToID, fmt.Sprintf("❌ 后台任务失败: %v", result.Err))
				return
			}
			d.react(chatID, replyToID, "✅")
			d.sendOutputTo(chatID, topicID, replyToID, result.Output)
			d.maybeUpdateMemory(ctx, chatID, topicID)
			d.maybeSummarizeSession(ctx, chatID, topicID)
		}()
		return
	}

	// 前台任务：持续发送 typing 动作直到完成，再回复结果
	// 立即发送第一次 typing，让用户马上看到反馈
	firstTypingParams := &telego.SendChatActionParams{
		ChatID: telego.ChatID{ID: chatID},
		Action: telego.ChatActionTyping,
	}
	if topicID > 0 {
		firstTypingParams.MessageThreadID = topicID
	}
	if err := d.botAPI.SendChatAction(firstTypingParams); err != nil {
		slog.Warn("SendChatAction 失败", "err", err, "chat_id", chatID, "topic_id", topicID)
	}

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:    prompt,
		Mode:      mode,
		ResultCh:  resultCh,
	})

	// 每 4s 续一次 typing，直到结果就绪
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingDone:
				return
			case <-ticker.C:
				params := &telego.SendChatActionParams{
					ChatID: telego.ChatID{ID: chatID},
					Action: telego.ChatActionTyping,
				}
				if topicID > 0 {
					params.MessageThreadID = topicID
				}
				if err := d.botAPI.SendChatAction(params); err != nil {
					slog.Warn("续 typing 失败", "err", err, "chat_id", chatID, "topic_id", topicID)
				}
			}
		}
	}()

	result := <-resultCh
	close(typingDone)

	if result.Err != nil {
		d.replyTo(chatID, topicID, replyToID, fmt.Sprintf("❌ 执行失败: %v", result.Err))
		return
	}
	d.react(chatID, replyToID, "✅")
	d.sendOutputTo(chatID, topicID, replyToID, result.Output)
	d.maybeUpdateMemory(ctx, chatID, topicID)
	d.maybeSummarizeSession(ctx, chatID, topicID)
}

// maybeUpdateMemory 在每 N 次成功完成后静默触发 memory.md 更新。
// 不向用户发送任何消息，结果只写日志。
func (d *Dispatcher) maybeUpdateMemory(ctx context.Context, chatID int64, topicID int) {
	interval := d.botCfg.MemoryUpdateInterval
	if interval <= 0 {
		return
	}

	key := chatTopicKey{chatID, topicID}
	d.countsMu.Lock()
	d.completionCounts[key]++
	count := d.completionCounts[key]
	d.countsMu.Unlock()

	if count%interval != 0 {
		return
	}

	slog.Info("触发记忆更新", "chat_id", chatID, "topic_id", topicID, "count", count)

	prompt := "請根據以上對話，靜默更新工作目錄下的 .goclaudeclaw/memory.md 文件。\n" +
		"文件格式使用 section 標記，每個 section 包含雙語（中英文）tags，方便相關性匹配：\n\n" +
		"<!-- section: global tags: always -->\n" +
		"## 全局偏好\n" +
		"（用戶偏好、語言設定等，每次都注入）\n\n" +
		"<!-- section: project-name tags: 英文tag,中文tag,別名,... -->\n" +
		"## 項目名稱\n" +
		"（項目相關知識）\n\n" +
		"Tags 規則：同一概念的中英文都要寫，例如 'hn,永旺,lottery,彩票'。\n" +
		"保持每個 section 簡潔，不超過 300 字。不需要回覆任何其他內容。"

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
	})

	// 丢弃结果，只记录日志；成功后检查是否需要压缩 memory.md
	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("记忆更新失败", "err", result.Err, "chat_id", chatID, "topic_id", topicID)
			return
		}
		slog.Info("记忆更新完成", "chat_id", chatID, "topic_id", topicID)
		d.maybeCompressMemory(ctx, chatID, topicID)
	}()
}

// maybeCompressMemory 在每 N 次 memory 更新后静默压缩 memory.md，去重并精简。
func (d *Dispatcher) maybeCompressMemory(ctx context.Context, chatID int64, topicID int) {
	interval := d.botCfg.MemoryCompressInterval
	if interval <= 0 {
		return
	}

	d.countsMu.Lock()
	d.memUpdateCount++
	count := d.memUpdateCount
	d.countsMu.Unlock()

	if count%interval != 0 {
		return
	}

	slog.Info("触发 memory.md 压缩", "count", count)

	prompt := "請閱讀工作目錄下的 .goclaudeclaw/memory.md，" +
		"去除重複內容、合并相似條目、刪除過時資訊，重新整理成簡潔的 Markdown。" +
		"直接覆寫原文件，不需要回覆任何其他內容。"

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
	})

	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("memory.md 压缩失败", "err", result.Err)
		} else {
			slog.Info("memory.md 压缩完成")
		}
	}()
}

// maybeSummarizeSession 在每 N 次成功完成后，摘要对话内容写入 memory.md 并重置 session。
// 下次对话从全新 session 开始，但摘要会通过 memory.md 注入保持上下文连续性。
func (d *Dispatcher) maybeSummarizeSession(ctx context.Context, chatID int64, topicID int) {
	interval := d.botCfg.SessionSummarizeInterval
	if interval <= 0 {
		return
	}

	key := chatTopicKey{chatID, topicID}
	d.countsMu.Lock()
	count := d.completionCounts[key]
	d.countsMu.Unlock()

	if count%interval != 0 {
		return
	}

	slog.Info("触发对话摘要并重置 session", "chat_id", chatID, "topic_id", topicID, "count", count)

	prompt := "請將以上完整對話整理成摘要，" +
		"更新工作目錄下的 .goclaudeclaw/memory.md 文件的 '## 對話摘要' 部分，" +
		"保留重要決策、用戶偏好和關鍵上下文。完成後不需要回覆任何其他內容。"

	resultCh := make(chan runner.Result, 1)
	d.runnerMgr.Submit(runner.Job{
		Ctx:       ctx,
		Workspace: d.workspace,
		BotName:   d.botCfg.Name,
		ChatID:    chatID,
		TopicID:   topicID,
		Prompt:    prompt,
		Mode:      runner.ModeForeground,
		ResultCh:  resultCh,
	})

	go func() {
		result := <-resultCh
		if result.Err != nil {
			slog.Warn("对话摘要失败，保留原 session", "err", result.Err, "chat_id", chatID, "topic_id", topicID)
			return
		}
		// 摘要成功后清除 session，下次对话重新开始（memory.md 已有摘要）
		if err := d.sessionMgr.Clear(d.workspace, d.botCfg.Name, chatID, topicID); err != nil {
			slog.Warn("清除 session 失败", "err", err)
		} else {
			slog.Info("对话已摘要，session 已重置", "chat_id", chatID, "topic_id", topicID)
		}
	}()
}

// sendOutputTo 处理超长输出，首段 quote 触发消息，后续段直接发送。
func (d *Dispatcher) sendOutputTo(chatID int64, topicID int, replyToID int, output string) {
	if output == "" {
		d.replyTo(chatID, topicID, replyToID, "✓ 完成（无输出）")
		return
	}

	const maxLen = 4000
	runes := []rune(output)
	first := true

	for len(runes) > 0 {
		chunk := runes
		if len(chunk) > maxLen {
			chunk = runes[:maxLen]
			runes = runes[maxLen:]
		} else {
			runes = nil
		}
		if first {
			d.replyTo(chatID, topicID, replyToID, string(chunk))
			first = false
		} else {
			d.reply(chatID, topicID, string(chunk))
		}
	}
}

// editOrReply 尝试编辑 thinkingMsgID；若无效或编辑失败则退回 replyTo。
func (d *Dispatcher) editOrReply(chatID int64, topicID int, thinkingMsgID int, replyToID int, text string) {
	if thinkingMsgID > 0 {
		_, err := d.botAPI.EditMessageText(&telego.EditMessageTextParams{
			ChatID:    telego.ChatID{ID: chatID},
			MessageID: thinkingMsgID,
			Text:      text,
			ParseMode: telego.ModeMarkdown,
		})
		if err == nil {
			return
		}
		// Markdown 解析失败时降级为纯文本重试
		_, err = d.botAPI.EditMessageText(&telego.EditMessageTextParams{
			ChatID:    telego.ChatID{ID: chatID},
			MessageID: thinkingMsgID,
			Text:      text,
		})
		if err == nil {
			return
		}
		slog.Warn("编辑消息失败，退回 replyTo", "err", err, "chat_id", chatID)
	}
	d.replyTo(chatID, topicID, replyToID, text)
}

// sendOutputWithThinking 将结果写入思考占位消息：
// 第一段 edit 进 thinkingMsgID，超长部分作为新消息继续发送。
func (d *Dispatcher) sendOutputWithThinking(chatID int64, topicID int, thinkingMsgID int, replyToID int, output string) {
	if output == "" {
		d.editOrReply(chatID, topicID, thinkingMsgID, replyToID, "✓ 完成（无输出）")
		return
	}

	const maxLen = 4000
	runes := []rune(output)
	first := true

	for len(runes) > 0 {
		chunk := runes
		if len(chunk) > maxLen {
			chunk = runes[:maxLen]
			runes = runes[maxLen:]
		} else {
			runes = nil
		}
		if first {
			d.editOrReply(chatID, topicID, thinkingMsgID, replyToID, string(chunk))
			first = false
		} else {
			d.reply(chatID, topicID, string(chunk))
		}
	}
}

// replyTo 回复指定消息（quote），若 replyToID <= 0 则退化为普通发送。
func (d *Dispatcher) replyTo(chatID int64, topicID int, replyToID int, text string) {
	params := &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		Text:      text,
		ParseMode: telego.ModeMarkdown,
	}
	if topicID > 0 {
		params.MessageThreadID = topicID
	}
	if replyToID > 0 {
		params.ReplyParameters = &telego.ReplyParameters{MessageID: replyToID}
	}
	if _, err := d.botAPI.SendMessage(params); err != nil {
		// Markdown 解析失败时降级为纯文本重试
		params.ParseMode = ""
		if _, err2 := d.botAPI.SendMessage(params); err2 != nil {
			slog.Error("发送 Telegram 消息失败",
				"chat_id", chatID, "topic_id", topicID, "err", err2, "bot", d.botCfg.Name)
		}
	}
}

// reply 向指定 chat（可选 topic）发送文本消息，不 quote 任何消息。
func (d *Dispatcher) reply(chatID int64, topicID int, text string) {
	d.replyTo(chatID, topicID, 0, text)
}

// react 给指定消息打表情 reaction，出错只记日志不影响主流程。
func (d *Dispatcher) react(chatID int64, messageID int, emoji string) {
	_ = d.botAPI.SetMessageReaction(&telego.SetMessageReactionParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: messageID,
		Reaction:  []telego.ReactionType{&telego.ReactionTypeEmoji{Type: "emoji", Emoji: emoji}},
	})
}

// isAllowed 检查用户是否在白名单中。
func (d *Dispatcher) isAllowed(userID int64) bool {
	d.mu.Lock()
	allowed := d.botCfg.AllowedUsers
	d.mu.Unlock()

	for _, id := range allowed {
		if id == userID {
			return true
		}
	}
	return false
}

// openAIAPIKey 返回有效的 OpenAI API 密钥：优先使用 BotConfig 字段，其次读环境变量。
func (d *Dispatcher) openAIAPIKey() string {
	if k := d.botCfg.OpenAIAPIKey; k != "" {
		return k
	}
	return os.Getenv("OPENAI_API_KEY")
}

// transcribeVoice 下载 Telegram 语音文件（ogg）并通过 Whisper API 转为文字。
func (d *Dispatcher) transcribeVoice(fileID string, chatID int64) (string, error) {
	apiKey := d.openAIAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("未配置 OpenAI API key（openai_api_key 或 OPENAI_API_KEY）")
	}

	// 下载 ogg 文件到 inbox
	savedPath, err := d.downloadTelegramFile(fileID, chatID, "voice.ogg")
	if err != nil {
		return "", fmt.Errorf("下载语音文件失败: %w", err)
	}

	// 读取文件内容
	audioBytes, err := os.ReadFile(savedPath)
	if err != nil {
		return "", fmt.Errorf("读取语音文件失败: %w", err)
	}

	// 构造 multipart 请求体
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// model 字段
	if err := mw.WriteField("model", "whisper-1"); err != nil {
		return "", fmt.Errorf("写入 multipart model 字段失败: %w", err)
	}

	// file 字段
	fw, err := mw.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return "", fmt.Errorf("创建 multipart file 字段失败: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return "", fmt.Errorf("写入音频数据失败: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("关闭 multipart writer 失败: %w", err)
	}

	// 发送请求到 Whisper API
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 Whisper API 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 Whisper 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Whisper API 返回错误 %d: %s", resp.StatusCode, string(body))
	}

	// 解析响应：{"text": "..."}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析 Whisper 响应失败: %w", err)
	}

	slog.Info("语音转文字完成",
		"chat_id", chatID,
		"chars", len(result.Text),
		"bot", d.botCfg.Name)

	return result.Text, nil
}

// downloadTelegramFile 通过 Telegram Bot API 下载文件，
// 保存到 {workspace}/.goclaudeclaw/inbox/{chatID}/{filename}，
// 返回本地绝对路径。
func (d *Dispatcher) downloadTelegramFile(fileID string, chatID int64, filename string) (string, error) {
	// 第一步：向 Telegram 查询文件路径
	file, err := d.botAPI.GetFile(&telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("获取文件信息失败: %w", err)
	}
	if file.FilePath == "" {
		return "", fmt.Errorf("Telegram 返回空文件路径")
	}

	// 构造下载 URL
	downloadURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", d.botCfg.Token, file.FilePath)

	// 第二步：创建目录 {workspace}/.goclaudeclaw/inbox/{chatID}/
	inboxDir := filepath.Join(d.workspace, ".goclaudeclaw", "inbox", fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 inbox 目录失败: %w", err)
	}

	// 目标路径：若文件名已存在则加时间戳前缀避免覆盖
	destPath := filepath.Join(inboxDir, filename)
	if _, statErr := os.Stat(destPath); statErr == nil {
		// 文件已存在，加时间戳前缀
		ts := time.Now().Format("20060102_150405_")
		destPath = filepath.Join(inboxDir, ts+filename)
	}

	// 第三步：HTTP 下载文件内容
	resp, err := http.Get(downloadURL) //nolint:noctx // 下载为一次性操作，无需 context
	if err != nil {
		return "", fmt.Errorf("HTTP 下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Telegram 返回非 200 状态: %d", resp.StatusCode)
	}

	// 写入本地文件
	f, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("创建本地文件失败: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	slog.Info("文件已下载",
		"chat_id", chatID,
		"filename", filename,
		"dest", destPath,
		"bot", d.botCfg.Name)

	return destPath, nil
}

// combineMessages 将多条消息合并为一条，按时间顺序拼接。
// 多条消息之间用换行分隔，便于 claude 理解上下文。
func combineMessages(msgs []incomingMsg) string {
	if len(msgs) == 1 {
		return msgs[0].text
	}
	var sb strings.Builder
	for i, m := range msgs {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(m.text)
	}
	return sb.String()
}
