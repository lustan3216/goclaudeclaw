# goclaudeclaw ⚡

**Claude Code on your phone. Tap a message, get work done.**

goclaudeclaw is a Go rewrite of [ClaudeClaw](https://github.com/lustan3216/claudeclaw) — a daemon that keeps Claude Code running 24/7 and lets you talk to it over Telegram. Ask it to write code, run reports, check your project status, or kick off a long task while you're away from your desk.

One binary. No Node, no Bun, no runtime to babysit.

---

## Why goclaudeclaw over original ClaudeClaw?

| | goclaudeclaw | claudeclaw (original) |
|---|---|---|
| Installation | `go install` — single binary, done | Node + Bun runtime required |
| Multi-bot | Run multiple bots, each with different permissions | One bot |
| Telegram Topics | Each topic = separate parallel conversation | Single thread |
| Auto subagent | Long tasks run in background automatically | Manual `/bg` only |
| Memory footprint | ~15MB idle | Heavier JS runtime |
| Config reload | Hot-reload without restart | Restart required |
| Crash recovery | Goroutine-per-bot, isolated failures | Process-level failure |

---

## Install

**One line:**
```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/goclaudeclaw/main/install.sh | sh
```

**Or with Go:**
```sh
go install github.com/lustan3216/goclaudeclaw/cmd/goclaudeclaw@latest
```

**Or as Claude Code plugin (coming soon):**
```sh
claude plugin install lustan3216/goclaudeclaw
```

---

## Quick Start

**1. Create a config file**

```yaml
# config.yaml
workspace: /path/to/your/project

bots:
  - name: "main"
    token: "YOUR_TELEGRAM_BOT_TOKEN"
    allowed_users: [123456789]   # your Telegram user ID — keep this private

security:
  level: moderate   # locked | strict | moderate | unrestricted
```

**2. Run it**

```bash
goclaudeclaw --config config.yaml
```

That's your Claude Code daemon, now reachable from anywhere.

---

## Key Features

### Telegram Topics → Parallel Conversations

Create a Telegram group, enable Topics, and each topic becomes its own independent conversation with Claude. Run a code review in one thread while a deploy task runs in another — no waiting in line.

### Multi-Bot Support

Run multiple bots from a single config. Give different people (or different projects) their own bot with separate permissions and workspaces. All bots share the same claude-mem memory.

### Auto Subagent Detection

When you send a message, goclaudeclaw quickly figures out if it's a quick question (answer in the chat) or a long task (fire it in the background). You don't have to think about it. Long tasks reply immediately with "on it" and ping you when done.

### Shared Memory via claude-mem

All bots pull from the same memory store. Things Claude learns in one conversation are available in others. Your project context, preferences, and past decisions persist across restarts.

### Heartbeat

Optional periodic check-ins — Claude pings you on a timer to surface anything worth your attention. Configure quiet windows so it doesn't bother you at 3am.

### Cron Jobs

Schedule prompts with standard cron syntax. Daily reports, weekly summaries, whatever you want on a clock.

### Hot Config Reload

Edit `config.yaml` and changes apply immediately — no restart, no dropped connections.

---

## Config Reference

```yaml
workspace: /path/to/project

bots:
  - name: "main"
    token: "BOT_TOKEN"
    allowed_users: [123456789]
    debounce_ms: 1500            # merge rapid messages before sending

security:
  level: moderate                # moderate = most ops auto-approved

heartbeat:
  enabled: true
  interval_minutes: 30
  quiet_windows:
    - start: "23:00"
      end: "08:00"
  timezone: "Asia/Shanghai"

memory:
  enabled: true
  url: "http://localhost:3001"   # claude-mem server

crons:
  - name: "daily-standup"
    schedule: "0 9 * * 1-5"
    prompt: "What's on today's agenda?"
```

**Security levels:**

| Level | What it means |
|-------|---------------|
| `locked` | Read-only, system prompt constrained |
| `strict` | Confirm every tool call (Claude default) |
| `moderate` | Most operations auto-approved |
| `unrestricted` | `--dangerously-skip-permissions` |

---

## Bot Commands

| Command | What it does |
|---------|-------------|
| `/start`, `/help` | Show help |
| `/clear` | Start a fresh session |
| `/status` | Show workspace, security level, session info |
| `/bg <task>` | Force a task to run in background mode |

---

## FAQ

**Does this break Anthropic ToS?**
No. It wraps Claude Code directly — same as running `claude` from your terminal, just with a Telegram UI on top.

**Does it work on a VPS?**
Yes, that's the main use case. Install it on any Linux server, point it at your project, and your Telegram becomes a remote terminal with Claude inside.

**Can multiple people use the same bot?**
Set multiple user IDs in `allowed_users`. Each user gets their own conversation context.

**What happens if Claude is mid-task and I restart?**
Session IDs are persisted to disk. `--resume` is passed automatically on the next run so context survives restarts.

---

## Related

- [claudeclaw](https://github.com/lustan3216/claudeclaw) — original TypeScript/Bun implementation
- [claude-mem](https://github.com/lustan3216/claude-mem) — shared memory server (MCP-compatible)
- [Claude Code](https://docs.anthropic.com/claude-code) — the engine underneath
