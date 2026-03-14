---
name: claudeclaw:install
description: Interactive setup wizard — download binary, configure bot, and start claudeclaw
---

Guide the user through installing claudeclaw step by step.

## Step 1 — Download binary

Run the install script:

```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/claudeclaw/main/install.sh | sh
```

Or with Go:
```sh
go install github.com/lustan3216/claudeclaw/cmd/claudeclaw@latest
```

Verify:
```sh
claudeclaw --version
```

## Step 2 — Run it

```sh
claudeclaw
```

claudeclaw detects there is no config and launches the interactive setup wizard automatically.
The wizard covers:
1. **Telegram Bot Token** — from @BotFather (`/newbot`). Token is validated live.
2. **Telegram User ID** — find yours with @userinfobot.
3. **Workspace path** — the project directory (defaults to current dir).
4. **Security level** — `moderate` is recommended for solo use.
5. **Optional tokens** — GitHub, Notion, OpenAI (all skippable).

Config is saved to `config.json` and the daemon starts immediately.

## Step 3 — Test

Send a message to the bot on Telegram. It should react with 👀 while processing and reply when done.

Send `/help` to see all available commands.

## Re-run setup

To change settings at any time:

```sh
claudeclaw --setup
```
