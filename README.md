# claudeclaw ⚡

**Make your Claude Code like openclaw.**

One binary. Drop it on your server. Talk to Claude from Telegram — anywhere, anytime.

claudeclaw keeps Claude Code running 24/7, routes your messages to it, and pings you back when it's done. Write code, run reports, kick off deploys — all from your phone while you're away from the desk.

> **Multiple Claude Code sessions. Simultaneously. Shared memory.**
> Create a Telegram group with Topics enabled — each topic is an independent Claude session running in parallel. Review code in one thread, run a deploy in another, debug a bug in a third. All on the same machine, all sharing the same project memory.

---

## Prerequisites

- **Claude Code CLI** (`claude`) — claudeclaw is a bridge to it, not a replacement
- **Node.js + npx** — only required if you use the built-in MCP servers (GitHub, Notion, Brave, Browser)

**Why Node for MCPs?**
The MCP ecosystem runs on Node. Every MCP server is an npm package launched via `npx`. claudeclaw auto-generates the `.mcp.json` config file for you — but it can't bypass the fact that `npx` needs to be on your system to actually run the server processes. The first time Claude calls a tool from an MCP server, `npx -y` downloads and starts the package automatically. After that it's cached.

If you don't use any MCP servers, Node is not needed at all.

---

## Install

**One-line script:**
```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/claudeclaw/main/install.sh | sh
```

**With Go:**
```sh
go install github.com/lustan3216/claudeclaw/cmd/claudeclaw@latest
```

**Via Claude Code plugin:**
```sh
claude plugin install lustan3216/claudeclaw
```

---

## Quick Start

```bash
claudeclaw
```

No config file needed. On first launch, claudeclaw asks for one thing — your bot token — then guides you through the rest inside Telegram:

```
⚡ Welcome to claudeclaw — one step to get started.
   Don't have a bot? Message @BotFather on Telegram → /newbot

  Bot token: xxxxxxx:xxxxxxxxx
  Verifying token... ✓ Connected as @my_claude_bot

✓ Config saved to config.json
Starting @my_claude_bot — message it on Telegram to finish setup.
```

Then send the bot any message. The first person to message it becomes the owner, and the bot walks you through the rest:

```
⚡ Welcome! You're the owner of this bot.

Let's finish setup — everything can be configured right here in Telegram.

📁 Workspace (currently: /home/user/myproject)
🔒 Security (currently: moderate)
   /set security_level strict

🔑 Integrations (all optional)
   /set github_token  ghp_xxx
   /set notion_token  secret_xxx
   /set brave_key     BSA_xxx
   /set browser       true
   /set gemini        true

👥 Add more users
   /adduser <telegram_id>
```

Config is saved to `config.json`. To re-run setup anytime: `claudeclaw --setup`

---

## Key Features

### Telegram Topics → Multiple Claude Code Sessions in Parallel

Each Telegram topic is a fully independent Claude Code session — its own context, its own task queue, running concurrently with all the others. No waiting in line.

```
Telegram Group (Topics enabled)
├── 📌 #code-review     → Claude session A  (reviewing PR #42)
├── 📌 #deploy          → Claude session B  (running deploy script)
├── 📌 #bug-fix         → Claude session C  (debugging auth issue)
└── 📌 #research        → Claude session D  (idle, ready)
```

All sessions share the same `memory.md` — so context you build in one thread is available in all others. One codebase, one memory, many parallel workers.

**Setup:**
1. Create a Telegram group
2. Enable Topics (Group Info → Edit → Topics)
3. Add your bot as **admin** (required to read messages in topics)
4. Create a topic — Claude auto-replies `✓ Ready`

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

Place notes in `{workspace}/.claudeclaw/memory.md`. On every new session, this file is automatically injected as context — giving Claude persistent knowledge about your project, preferences, and decisions without needing an external service.

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

### Auto Subagent Detection

claudeclaw classifies each message as foreground (quick reply) or background (long task). Long tasks reply immediately and ping you when done. Use `/bg <task>` to force background mode manually.

### Multi-Bot Support

Run multiple bots from a single config — different people, different projects, separate permissions. All bots share the same workspace and memory.

**Creating multiple Telegram bots:**

1. Open [@BotFather](https://t.me/BotFather) on Telegram
2. Send `/newbot` for each bot you need
3. Follow the prompts to set name and username
4. Copy the token for each bot into `config.json`

**Example scenarios:**

```
┌─────────────────────────────────────────────────────┐
│                    config.json                      │
│                                                     │
│  bot: "personal"          bot: "work"               │
│  ┌──────────────┐         ┌──────────────┐          │
│  │ @my_claude   │         │ @team_claude │          │
│  │              │         │              │          │
│  │ allowed: you │         │ allowed: you │          │
│  │              │         │         + 3  │          │
│  │ debounce 1.5s│         │ teammates    │          │
│  └──────────────┘         └──────────────┘          │
│         │                        │                  │
│    Personal use             Shared with team        │
│    Full trust               Shared workspace        │
└─────────────────────────────────────────────────────┘
```

**Scenario 1 — Solo developer:**
One bot, just for you. Full trust, `security: unrestricted`, no friction.

**Scenario 2 — Team access:**
A second bot with teammates in `allowed_users`. Set `security: moderate` so sensitive operations still require confirmation. Both bots share the same codebase and memory — teammates see the same project context you do.

**Scenario 3 — Dedicated task bot:**
A third bot wired to a specific sub-project. Same machine, different `workspace` path per bot. Run code reviews in one thread, infrastructure ops in another — fully parallel, no queue.

```json
{
  "bots": [
    {
      "name": "personal",
      "token": "BOT_TOKEN_1",
      "allowed_users": [111111111],
      "debounce_ms": 1500
    },
    {
      "name": "team",
      "token": "BOT_TOKEN_2",
      "allowed_users": [111111111, 222222222, 333333333],
      "debounce_ms": 500
    }
  ]
}
```

### Heartbeat

Optional periodic check-ins. Claude surfaces anything worth your attention on a timer. Configure quiet windows so it stays silent at night.

### Built-in MCP Servers

Fill in a token and claudeclaw generates `.mcp.json` in your workspace automatically — no manual setup. Claude picks it up on the next run.

| Server | Token field | What it unlocks |
|--------|-------------|-----------------|
| GitHub | `mcps.github.token` | Read/write repos, issues, PRs |
| Notion | `mcps.notion.token` | Read/write Notion pages and databases |
| Brave Search | `mcps.brave.api_key` | Web search |
| Browser (Puppeteer) | `mcps.browser.enabled: true` | Headless browser automation |

Leave any field empty / false to disable that server. Config changes hot-reload — no restart needed.

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
    "endpoint": "http://localhost:47432"
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

  "mcps": {
    "github":  { "token": "" },
    "notion":  { "token": "" },
    "browser": { "enabled": false },
    "brave":   { "api_key": "" }
  },

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

- [claude-mem](https://github.com/lustan3216/claude-mem) — shared memory server (MCP-compatible)
- [Claude Code](https://docs.anthropic.com/claude-code) — the engine underneath
