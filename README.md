# goclaudeclaw ⚡

**Claude Code on your phone. Tap a message, get work done.**

goclaudeclaw is a Go rewrite of [ClaudeClaw](https://github.com/lustan3216/claudeclaw) — a daemon that keeps Claude Code running 24/7 and lets you talk to it over Telegram. Ask it to write code, run reports, check your project status, or kick off a long task while you're away from your desk.

One binary. No Node, no Bun, no runtime to babysit.

---

## Why goclaudeclaw over original ClaudeClaw?

| | goclaudeclaw | claudeclaw (original) |
|---|---|---|
| Installation | Single binary, no runtime | Node + Bun required |
| Multi-bot | Multiple bots, separate permissions | One bot |
| Telegram Topics | Each topic = parallel independent session | Single thread |
| Reactions | 👀 on receipt, ✅ on done | None |
| Images / PDFs | Auto-download + vision / Read tool | None |
| Voice messages | Whisper transcription → Claude | None |
| Local memory | Auto-inject `memory.md` on new sessions | Manual |
| Auto subagent | Long tasks auto-detected, run in background | `/bg` only |
| Config format | JSON, hot-reload without restart | YAML, restart required |
| Memory footprint | ~15MB idle | Heavier JS runtime |

---

## Install

**One-line script:**
```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/goclaudeclaw/main/install.sh | sh
```

**With Go:**
```sh
go install github.com/lustan3216/goclaudeclaw/cmd/goclaudeclaw@latest
```

**Via Claude Code plugin:**
```sh
claude plugin install lustan3216/goclaudeclaw
```

---

## Quick Start

**1. Create `config.json`**

```json
{
  "workspace": "/path/to/your/project",
  "bots": [
    {
      "name": "main",
      "token": "YOUR_TELEGRAM_BOT_TOKEN",
      "allowed_users": [123456789],
      "debounce_ms": 1500,
      "openai_api_key": ""
    }
  ],
  "security": {
    "level": "moderate"
  }
}
```

Get your Telegram user ID from [@userinfobot](https://t.me/userinfobot).

**2. Run it**

```bash
goclaudeclaw --config config.json
```

That's it — Claude Code is now reachable over Telegram from anywhere.

---

## Key Features

### Telegram Topics → Parallel Conversations

Create a Telegram group, enable Topics, and each topic becomes its own independent Claude session. Run a code review in one thread while a deploy runs in another — no waiting in line. When a topic is created, Claude automatically acknowledges with `✓ 已就緒`.

> **Tip:** Make the bot a group **admin** so it can read all messages without disabling privacy mode.

### Message Reactions

Bot reacts to every message it receives:
- **👀** — received, processing
- **✅** — done

### Media Support

Forward images, screenshots, PDFs, or documents directly to the bot:
- **Images** — base64-encoded and sent to Claude's vision
- **PDFs** — downloaded and processed via Claude's Read tool
- **Voice messages** — transcribed via OpenAI Whisper, then sent to Claude

Set `openai_api_key` in config (or `OPENAI_API_KEY` env var) to enable voice.

### Local Memory Injection

Place notes in `{workspace}/.goclaudeclaw/memory.md`. On every new session, this file is automatically injected as context — giving Claude persistent knowledge about your project, preferences, and decisions without needing an external service.

**Smart injection:** memory.md is split into tagged sections. On each new session, only sections relevant to the current prompt are injected — saving tokens and keeping context focused.

Section format:
```markdown
<!-- section: global tags: always -->
## Global preferences
Always injected (keep short).

<!-- section: hn tags: hn,永旺,lottery,彩票,nestjs -->
## HN Project
Injected only when the prompt mentions hn/永旺/lottery/etc.
```

Tags should include both Chinese and English synonyms — e.g. `hn,永旺,lottery,彩票`. Claude auto-generates tags when updating memory.

Three config knobs control automatic memory lifecycle:

| Field | Default | What it does |
|---|---|---|
| `memory_update_interval` | 0 (off) | Every N completions, Claude silently writes new knowledge to `memory.md` |
| `session_summarize_interval` | 0 (off) | Every N completions, Claude summarizes the conversation into `memory.md`, then resets the session — keeping context without bloating the history |
| `memory_compress_interval` | 0 (off) | Every N memory updates, Claude deduplicates and trims `memory.md` to keep it lean |

### Typing Indicator

While Claude processes a message, the bot sends Telegram's native `••• typing` indicator and refreshes it every 4 seconds until the response is ready. No placeholder messages — just the standard in-chat typing status.

### Auto Subagent Detection

goclaudeclaw classifies each message as foreground (quick reply) or background (long task). Long tasks reply immediately and ping you when done. Use `/bg <task>` to force background mode manually.

### Multi-Bot Support

Run multiple bots from a single config — different people, different projects, separate permissions. All bots share the same workspace and memory.

### Heartbeat

Optional periodic check-ins. Claude surfaces anything worth your attention on a timer. Configure quiet windows so it stays silent at night.

### Cron Jobs

Schedule prompts with standard cron syntax. Daily reports, weekly summaries, whatever you want on a clock.

---

## Config Reference

```json
{
  "workspace": "/path/to/project",

  "bots": [
    {
      "name": "main",
      "token": "BOT_TOKEN",
      "allowed_users": [123456789],
      "debounce_ms": 1500,
      "openai_api_key": "",
      "memory_update_interval": 5,
      "session_summarize_interval": 20,
      "memory_compress_interval": 10
    }
  ],

  "memory": {
    "provider": "claude-mem",
    "endpoint": "http://localhost:8080"
  },

  "heartbeat": {
    "enabled": true,
    "interval_minutes": 30,
    "prompt": "Check pending tasks and surface anything important.",
    "timezone": "Asia/Shanghai",
    "quiet_windows": [
      { "start": "23:00", "end": "08:00" }
    ]
  },

  "cron_jobs": [
    {
      "name": "daily-standup",
      "schedule": "0 9 * * 1-5",
      "prompt": "What's on today's agenda?"
    }
  ],

  "security": {
    "level": "moderate"
  }
}
```

**Security levels:**

| Level | What it means |
|-------|---------------|
| `locked` | Read-only, system prompt constrained |
| `strict` | Confirm every tool call |
| `moderate` | Most operations auto-approved |
| `unrestricted` | `--dangerously-skip-permissions` |

---

## Bot Commands

| Command | What it does |
|---------|-------------|
| `/start`, `/help` | Show help |
| `/clear` | Start a fresh session |
| `/status` | Show workspace, security level, session info |
| `/bg <task>` | Force background mode |

---

## FAQ

**Does this break Anthropic ToS?**
No. It wraps Claude Code directly — same as running `claude` from your terminal, just with a Telegram UI on top.

**Does it work on a VPS?**
Yes, that's the main use case. Install on any Linux server, point it at your project, and your Telegram becomes a remote terminal with Claude inside.

**Can multiple people share one bot?**
Add multiple user IDs to `allowed_users`. Each user gets their own conversation context per topic.

**What happens if Claude is mid-task and I restart?**
Session IDs persist to disk. `--resume` is passed automatically so context survives restarts.

**Do I need claude-mem?**
No. Local memory via `memory.md` works out of the box. claude-mem adds cross-bot persistent memory if you want it.

---

## Related

- [claudeclaw](https://github.com/lustan3216/claudeclaw) — original TypeScript/Bun implementation
- [claude-mem](https://github.com/lustan3216/claude-mem) — shared memory server (MCP-compatible)
- [Claude Code](https://docs.anthropic.com/claude-code) — the engine underneath
