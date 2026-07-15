# CLAUDE.md

Go service bridging respond.io (messaging platform) webhooks to ACP (Agent Client Protocol) harnesses — one harness subprocess per contact, replies delivered back through the respond.io API paced like human typing. See README.md for architecture. Single flat `main` package; work happens on the `feat/agent-server` branch (PR #1).

## Commands

```sh
go build ./... && go vet ./... && go test -race ./...   # always run all three
set -a && source .env && set +a && go run .              # run with real secrets
```

`.env` is gitignored and holds real secrets (respond.io API token, per-webhook signing keys, HEBO_API_KEY). Never commit its values or paste them into the repo, README, or PR.

## Production harness: pi-acp → pi → gato (non-obvious constraints)

The deployed harness is `pi-acp` (npm, global) driving `pi` with the gato sales agent config (local checkout: `~/Projects/hebo-gato`). Things that will bite you:

- **pi only loads a project's `.pi/` config if the session cwd contains it AND the cwd is under a path in `~/.pi/agent/trust.json`.** That's why `DATA_DIR` lives under `$HOME` (e.g. `~/.gato-e2e/data`), not `/tmp`, and why `CONTACT_TEMPLATE` symlinks the gato checkout into each contact dir.
- **pi-acp never resolves `session/prompt` for extension slash commands** (e.g. `/add-assistant-message` from @8monkey/pi-context-history): pi runs the handler without an agent loop, and pi-acp only resolves on `agent_end`. The command does emit an `agent_message_chunk` ack once the message is persisted; sessions.go ends record-only turns at that ack (bounded by the generic turn timeout) and then recycles the harness — the recycle is required regardless, since recorded messages only apply on the next session rebuild (`session/load`). If the upstream bug in github.com/svkozak/pi-acp is fixed, only the ack-or-timeout wait can go.
- **Harness shutdown must SIGKILL the whole process group** (already implemented): a gracefully-quitting pi child runs gato's pi-session-gzip hook, which compresses the session file that `session/load` needs — silently emptying restored history.
- **pi-acp splits a slash command from its args at the first space**, so recorded messages are flattened to one line before prepending the slash command (`/add-user-message` / `/add-assistant-message`, hardcoded in server.go).
- pi model config: `~/.pi/agent/models.json` defines the `hebo` provider (gateway.hebo.ai) resolving `$HEBO_API_KEY` from the environment the server inherits. pi-acp session map: `~/.pi/pi-acp/session-map.json`. pi session files: `~/.pi/agent/sessions/<cwd-slug>/*.jsonl`.

## respond.io specifics

- Both webhooks (New Incoming Message `message.received`, New Outgoing Message `message.sent`) point at the **same** `/webhook` endpoint; the server dispatches on `event_type`. Each registered webhook has its **own** signing key (`X-Webhook-Signature` = base64 HMAC-SHA256 of the raw body).
- Test contact created via API: id 488295717 ("E2E Test") — has no messaging channel, so sends to it fail with "no last interacted channel" (expected; the pipeline up to the send still exercises fully). Real Telegram contact via @pi_gato_bot: id 488396680. Conversations are assigned to respond.io user 471663 ("Gato AI").
- The docs at developers.respond.io are JS-rendered; real payload examples come from the Stoplight backend API (`stoplight.io/api/v1/projects/cHJqOjE3NzMxNA/...`).

## e2e debugging tricks

- To capture ACP wire traffic, shadow `pi-acp` earlier on the `PATH` with a wrapper script: `tee -a /tmp/wire-in.jsonl | pi-acp | tee -a /tmp/wire-out.jsonl`.
- The ngrok inspector (`http://127.0.0.1:4040/api/tunnels`, then port 4041, 4042… for additional agents) shows which local port each tunnel maps to and lets you replay/inspect webhook bodies.
- Real turns take 20–40s (thought chunks stream first) — a "hung" webhook usually isn't.

## Known open issues

- Ordering of multiple messages queued behind an active turn relies on mutex wake-up order, which is not FIFO.
- No idle reaper: harness subprocesses live forever once spawned.
