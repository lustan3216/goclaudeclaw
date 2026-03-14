---
name: goclaudeclaw:install
description: Interactive setup wizard — download binary, configure bot, and start goclaudeclaw
---

Guide the user through installing and configuring goclaudeclaw step by step.

## Step 1 — Download binary

Run the install script to download the pre-built binary:

```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/goclaudeclaw/main/install.sh | sh
```

If the user has Go installed, they can alternatively use:
```sh
go install github.com/lustan3216/goclaudeclaw/cmd/goclaudeclaw@latest
```

Verify installation:
```sh
goclaudeclaw --version
```

## Step 2 — Create a Telegram bot

Tell the user to:
1. Open Telegram and message **@BotFather**
2. Send `/newbot` and follow the prompts
3. Copy the bot token (looks like `123456789:ABCdef...`)
4. Get their Telegram user ID from **@userinfobot**

## Step 3 — Create config.json

Ask the user for:
- **workspace** — the project directory Claude Code should work in (e.g. `/home/user/myproject`)
- **bot token** — from BotFather
- **allowed_users** — their Telegram user ID (from @userinfobot)

Then create `config.json` in the workspace:

```json
{
  "workspace": "<WORKSPACE_PATH>",
  "bots": [
    {
      "name": "main",
      "token": "<BOT_TOKEN>",
      "allowed_users": [<USER_ID>],
      "debounce_ms": 1500
    }
  ],
  "security": {
    "level": "moderate"
  }
}
```

## Step 4 — Start

Option A — run directly:
```sh
goclaudeclaw
```

Option B — with watchdog (auto-restart + auto-update):
```sh
curl -fsSL https://raw.githubusercontent.com/lustan3216/goclaudeclaw/main/run.sh -o run.sh
chmod +x run.sh
bash run.sh
```

## Step 5 — Test

Send a message to the bot on Telegram. It should respond with 👀 while processing and reply when done.

Send `/help` to see all available commands.
