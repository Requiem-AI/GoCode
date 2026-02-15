# GoCode Agent Guide

This file documents repository-specific context for coding agents working in `GoCode`.

## What this project is

`GoCode` is a Go Telegram bot that runs Codex CLI workflows per chat/topic:

- Telegram messages are received in `services/telegram.go`.
- Topic-specific repo workspaces are managed in `services/git.go`.
- LLM calls are delegated to Codex CLI in `llm/codex.go`.
- Optional web preview tunnels are managed in `services/preview.go`.

## Repository layout

- `runtime/gocode.go`: entrypoint, env loading, logger config, service registration.
- `context/`: lightweight service container and lifecycle orchestration.
- `services/setup.go`: interactive first-run setup (Codex login, Telegram token, USER_ID, SSH setup).
- `services/telegram.go`: bot handlers (`/new`, `/clear`, `/delete`, `/github`, `/preview`, `/restart`, text handling).
- `services/git.go`: per-topic repo bootstrap/clone/delete and GitHub auth mode.
- `services/preview.go`: `yarn dev` + `ngrok`/`tailscale` tunnel lifecycle.
- `llm/`: LLM client abstraction (`codex` currently used).
- `data/telegram_topics.json`: persisted mapping of Telegram topic -> repo context.

## Build, run, test

- Build: `go build -o gocode ./runtime`
- Run: `./gocode`
- Alternate run: `go run ./runtime`
- Tests: `go test ./...`

Note: `run.sh` loops forever and restarts `./gocode` every 2 seconds after exit.

## Runtime behavior you should know

- `.env` is required at startup (`godotenv.Load()` is fatal on missing file).
- Service start order is fixed by registration in `runtime/gocode.go`:
  1. `setup_svc`
  2. `git_svc`
  3. `Agent_svc`
  4. `preview_svc`
  5. `telegram_svc`
- `setup_svc` is interactive and can block startup waiting for user input.
- Topic repos are keyed by `chatID:threadID`.
- `TelegramService.guardHandler` logs every inbound update with `chat_id`, `chat_type`, and `thread_id`.

## Environment variables (actual code behavior)

- Required:
  - `TELEGRAM_SECRET`: bot token used by Telegram services.
- Common:
  - `LOG_LEVEL`: `trace|debug|info` (defaults to info behavior).
  - `CODEX_BIN`: Codex CLI path (default `codex`).
  - `USER_ID`: restrict bot handling to this Telegram user ID.
- Repo/Git:
  - `GIT_REPO_ROOT`: root for managed repos (default `repos`).
  - `GITHUB_TOKEN`: HTTPS clone auth token.
  - `GITHUB_USE_SSH`: SSH mode toggle (`true/1/yes/...`).
  - `GITHUB_SSH_KEY_PATH`: SSH private key path (default `~/.ssh/id_ed25519_gocode`).
- Telegram:
  - `TELEGRAM_TOPIC_CONTEXTS_PATH`: persisted topic mapping file (default `data/telegram_topics.json`).
  - `TELEGRAM_MAIN_CHAT_ID`: chat for startup online message.
  - `TELEGRAM_ONLINE_MESSAGE`: startup message text (default `"Bot is online."`).
- Preview:
  - `PREVIEW_TUNNEL`: `ngrok` or `tailscale`.
  - `NGROK_BIN`: ngrok binary path override.
  - `TAILSCALE_BIN`: tailscale binary path override.

## Telegram command behavior

- `/new <name> [repo-url|repo-path]`: create topic and bind repo context.
- `/clear`: clear topic LLM session memory.
- `/delete`: delete topic repo and Telegram topic (with confirm/cancel buttons).
- `/github ssh|status|logout|<token>`: auth setup/status/token storage.
- `/preview [start|status|stop] [ngrok|tailscale]`: topic-local web preview.
- `/restart`: runs `go mod tidy`, `go mod vendor`, `go build ./runtime/gocode.go`, then respawns process and terminates current PID.

## Conventions and guardrails

- Preserve existing architecture: logic should stay inside services, not package-level globals.
- Never use global functions or variables for app logic; keep behavior scoped to services.
- Use `ctx` alias for imports of the standard `context` package (existing project pattern).
- Keep changes scoped; avoid broad refactors unless requested.
- Validate changes with `go test ./...` when possible.
- Do not commit or expose local secrets (`.env`, `gh_ssh.key`, `gh_ssh.key.pub` are present in this workspace).

## Known implementation details worth remembering

- Setup service registers Telegram command menu entries, but handler coverage is defined in `services/telegram.go`.
- Preview assumes target repo has `package.json` and a working `yarn dev`.
- Topic context persistence writes atomically via temp file + rename.
