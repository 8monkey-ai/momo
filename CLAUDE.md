# CLAUDE.md

Go service bridging respond.io (messaging platform) webhooks to ACP (Agent Client Protocol) harnesses — one harness subprocess per contact, replies delivered back through the respond.io API paced like human typing. See README.md for architecture. Single flat `main` package; work happens on the `feat/agent-server` branch (PR #1).

## Commands

```sh
go build ./... && go vet ./... && go test -race ./...   # always run all three
set -a && source .env && set +a && go run .              # run with real secrets
```

`.env` is gitignored and holds real secrets (respond.io API token, per-webhook signing keys, HEBO_API_KEY) plus all runtime configuration (`PORT`, `DATA_DIR`, `CONTACT_TEMPLATE`, …). Always read the local `.env` for the actual values instead of assuming paths or ports. Never commit its values or paste them into the repo, README, or PR.

## Production harness: pi-acp → pi → gato (non-obvious constraints)

The deployed harness is `pi-acp` (npm, global) driving `pi` with the gato sales agent config (a local checkout of the gato repo — find it via the `CONTACT_TEMPLATE` symlink target in `.env`). Things that will bite you:

- **pi only loads a project's `.pi/` config if the session cwd contains it AND the cwd is under a path in `~/.pi/agent/trust.json`.** That's why `DATA_DIR` lives under `$HOME`, not `/tmp`, and why `CONTACT_TEMPLATE` symlinks the gato checkout into each contact dir. Both paths come from `.env`.
- **pi-acp never resolves `session/prompt` for extension slash commands** (e.g. `/add-assistant-message` from @8monkey/pi-context-history): pi runs the handler without an agent loop, and pi-acp only resolves on `agent_end`. The command does emit an `agent_message_chunk` ack once the message is persisted; sessions.go ends record-only turns at that ack. This is a special case of the general per-turn lifecycle: the harness is terminated after **every** completed turn (steered/cancelled turns keep it alive for the queued prompt) and the next message respawns it, because recorded messages and restored history only apply on the next session rebuild (`session/load`). If the upstream bug is fixed (https://github.com/svkozak/pi-acp/issues/84), only the ack wait can go.
- **All harness termination is a plain SIGTERM to pi-acp**: pi-acp exits, the broken stdio pipe makes pi shut down gracefully via its stdin-EOF handler — persisting the session and firing `session_shutdown{reason:"quit"}` hooks (verified empirically; pi-acp never signals its child itself, but the EOF path covers every real shutdown). Session gzip-at-rest is currently **de-prioritized**: the pi-session-gzip extension is removed from the gato config until its restore story is settled (see its PR #2 — stub `.jsonl` + a `pi-gz` shim via `PI_ACP_PI_COMMAND`, which pi-acp honors when spawning pi). If it comes back, that shim setup is what makes `session/load` see full history; the server needs no changes either way.
- **pi-acp splits a slash command from its args at the first literal space** (every splitter on the path uses `indexOf(" ")`, verified in the 0.0.31 bundle), so `command + " " + text` always parses correctly and newlines inside the text reach the extension handler verbatim (`/add-user-message` / `/add-assistant-message`, hardcoded in server.go).
- pi model config: `~/.pi/agent/models.json` defines the `hebo` provider (gateway.hebo.ai) resolving `$HEBO_API_KEY` from the environment the server inherits. pi-acp session map: `~/.pi/pi-acp/session-map.json`. pi session files: `~/.pi/agent/sessions/<cwd-slug>/*.jsonl`.

## respond.io specifics

- Both webhooks (New Incoming Message `message.received`, New Outgoing Message `message.sent`) point at the **same** `/webhook` endpoint; the server dispatches on `event_type`. Each registered webhook has its **own** signing key (`X-Webhook-Signature` = base64 HMAC-SHA256 of the raw body).
- A channel-less test contact (created via the API, e.g. named "E2E Test") is useful for pipeline tests: sends to it fail with "no last interacted channel" (expected; the pipeline up to the send still exercises fully). List contacts via `POST {RESPOND_API_URL}/contact/list` with the `RESPOND_API_TOKEN` from `.env` to find its id, or create one with `POST /contact/create`. Real Telegram contacts get created per user messaging the workspace's Telegram bot — discover the contact id for the current user from the `contactId` in incoming webhook bodies (ngrok inspector) or the per-contact dirs under `DATA_DIR`. Conversations are assigned to the respond.io user in `RESPOND_AI_ASSIGNEE_ID` (`.env`).
- The docs at developers.respond.io are JS-rendered; real payload examples come from the Stoplight backend API (`stoplight.io/api/v1/projects/cHJqOjE3NzMxNA/...`).

## Full e2e loop: sending real Telegram messages via tdl

`tdl` (installed via `brew install telegram-downloader`) holds a logged-in Telegram **user** session, so tests can drive the untouched Telegram→respond.io leg — not just synthetic webhooks. One-time setup per user: `tdl login -T code` (the default `tdl login` tries to import a Telegram Desktop session and fails; use `-T code` for phone+code login — it must be run interactively by the user) and `tdl extension install ruinmi/tdl-send`. The target is the workspace's Telegram bot (`<bot>` below) — its username appears in respond.io's Telegram channel settings and in the channel `meta` of any incoming webhook body; ask the user if no webhook has been captured yet. Before relying on it, verify a session exists (e.g. `tdl chat ls` succeeds) and send a first probe message to the bot to discover this user's respond.io contact id from the resulting webhook.

- Send as the user: `tdl send --to @<bot> --message "..."`.
- Read the bot's replies back: `tdl chat export -c <bot> -T last -i 6 --all --with-content -o /tmp/chat.json` (`--all --with-content` are required — without them text messages export empty).
- Confirm webhook delivery via the ngrok inspector (see below), then watch for the agent's reply both in the chat export and in server logs.
- Alternative when Telegram ingestion isn't under test: forge a `message.received` payload, sign it (base64 HMAC-SHA256 of raw body with `RESPOND_INCOMING_SIGNING_KEY`), and POST to `/webhook` directly.

## e2e debugging tricks

- To capture ACP wire traffic, shadow `pi-acp` earlier on the `PATH` with a wrapper script: `tee -a /tmp/wire-in.jsonl | pi-acp | tee -a /tmp/wire-out.jsonl`.
- The ngrok inspector API may not be on the default port 4040 — probe 4040/4041/4042 with `curl http://127.0.0.1:<port>/api/tunnels` to find the one serving the active tunnel. `GET /api/requests/http?limit=N` lists recent webhook deliveries; request bodies are base64 in `.requests[].request.raw`.
- Real turns take 20–40s (thought chunks stream first) — a "hung" webhook usually isn't.

## Known open issues

- Ordering of multiple messages queued behind an active turn relies on mutex wake-up order, which is not FIFO.
- Per-turn harness termination is server-driven only because pi-acp can't survive its pi child dying (silent empty `end_turn` on the next prompt — https://github.com/svkozak/pi-acp/issues/82). Once fixed upstream, revisit harness-side session end (see the FUTURE note in sessions.go).
