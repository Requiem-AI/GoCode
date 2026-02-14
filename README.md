# GoCode

GoCode is a Telegram bot that runs Codex-based agent workflows in chat topics.

## Prerequisites

- Go 1.24.10 (see `go.mod`).
- A Telegram bot token from BotFather.
- Codex CLI installed and available on your `PATH` (or set `CODEX_BIN`).
- Git (for repo operations).

## Telegram setup

1. Create a bot with BotFather and copy the token.
2. Create a **Telegram group** for GoCode.
3. Enable **Topics** (also called “Forums”) for the group.
4. Add the bot to the group.
5. Promote the bot to **Admin** with permission to **Create Topics** (and allow it to manage topics if you want it to create topics via `/new`).

The bot relies on topics for per-repo context. If topics are disabled or the bot cannot create them, it will only respond in the main chat.

## Configuration

GoCode loads configuration from `.env` at startup.

Create a `.env` in the repo root:

```env
# Required
TELEGRAM_SECRET=your-telegram-bot-token

# Optional
LOG_LEVEL=info
CODEX_BIN=codex
GIT_REPO_ROOT=./data/repos
GITHUB_USE_SSH=true
GITHUB_SSH_KEY_PATH=~/.ssh/id_ed25519
TELEGRAM_TOPIC_CONTEXTS_PATH=./data/telegram_topics.json
USER_ID=1234567890
PREVIEW_TUNNEL=ngrok
NGROK_BIN=ngrok
TAILSCALE_BIN=tailscale
```

Set `USER_ID` to your Telegram numeric user ID to restrict the bot to only your messages.

On first run, GoCode will prompt for Codex login if needed and can set up the Telegram token in `.env`.

## Build

```bash
go build -o gocode ./runtime
```

## Run

```bash
./gocode
```

Or run directly with Go:

```bash
go run ./runtime
```

## Usage

- `/new <name> [repo-url|repo-path]` creates a topic with a repo context.
- `/clear` clears the current topic context.
- `/delete` deletes the current topic and its repo.
- `/github` toggles GitHub auth mode (see bot replies for details).
- `/preview [start|status|stop] [ngrok|tailscale]` starts a web preview using `yarn dev`.

### Web preview requirements

- The repo must include a `package.json` with a `dev` script (`yarn dev` is used).
- Install either `ngrok` (recommended for quick ad-hoc sharing) or `tailscale` (recommended for stable, authenticated URLs via Funnel).
