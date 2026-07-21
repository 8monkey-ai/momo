# agent-server

A bridge between [respond.io](https://respond.io) and [pi-acp](https://github.com/svkozak/pi-acp), the [Agent Client Protocol (ACP)](https://agentclientprotocol.com) harness for pi. It receives respond.io webhooks, drives one harness subprocess per contact over ACP, and sends the agent's replies back through the respond.io API — paced like human typing.

```
respond.io ──webhook POST──▶  agent-server  ──JSON-RPC/stdio──▶ pi-acp (per contact)
respond.io ◀──REST API ─────  (this binary) ◀── session/update ─┘
```

## How it works

- **One harness subprocess per contact turn**, spawned on demand and gracefully terminated after each turn completes. The respond.io contact ID is the routing key; the harness issues the session ID. The next message respawns the harness and resumes the prior conversation, so history restore and recorded messages (which only apply on a session rebuild) always take effect.
- **Per-contact working directory** (`DATA_DIR/<contactId>`), passed as the session `cwd`. This is how the harness identifies the user and where it persists their chat history — returning users continue their previous conversation. With `CONTACT_TEMPLATE` set, each contact dir is seeded with symlinks to a project template so the harness picks up its config (system prompt, packages, skills).
- **Stateless session discovery**: the server keeps no persistent state. On spawn it asks the harness for existing sessions via `session/list` (filtered by the contact's `cwd`), and if the harness supports `session/load` it resumes the most recently updated one; otherwise it starts fresh with `session/new`.
- **Steering**: each contact's messages are handled by a per-contact actor that drains them in FIFO order, one turn at a time. A message arriving while a turn is streaming cancels the active turn (`session/cancel`) and prompts with the new message; a queued message already superseded by a newer one is still prompted (so it enters the session history) but cancelled as soon as it starts streaming.
- **Human-paced delivery**: streamed output is split on paragraph boundaries (`\n\n`) and each paragraph is sent as a separate respond.io message, delayed proportionally to its length.
- **Attachments**: images and audio (voice messages) are downloaded and sent to the agent as inline content blocks; other files are saved into the contact's working directory and referenced with a resource link.
- **Permissions**: tool-call permission requests from the harness are auto-approved (the harness uses its own fs/terminal tools inside the contact's cwd).

## Webhooks

Register these in respond.io (Settings → Integrations → Webhook), pointing both at the same URL, `https://<host>/webhook` (the server dispatches on `event_type`). Each registered webhook has its own signing key; set them via `RESPOND_INCOMING_SIGNING_KEY` / `RESPOND_OUTGOING_SIGNING_KEY` and the server verifies the `X-Webhook-Signature` header (base64 HMAC-SHA256 of the raw body) per event:

- **New Incoming Message** (`message.received`) — required. Each text message becomes a prompt turn. If `RESPOND_AI_ASSIGNEE_ID` is set, only conversations assigned to that respond.io user (or unassigned) get a reply; messages in conversations assigned to anyone else are recorded into the chat history with `/add-user-message` instead.
- **New Outgoing Message** (`message.sent`) — optional. Messages sent by human operators are recorded into the chat history by prompting with `/add-assistant-message` followed by the message text; nothing is delivered back. Only conversations assigned to a human (an assignee other than `RESPOND_AI_ASSIGNEE_ID`) are recorded — everywhere else the agent is the one replying, so the outgoing message is its own reply and recording it would echo it back into its context.

## Configuration

| Env var | Default | Description |
| --- | --- | --- |
| `RESPOND_API_TOKEN` | — (required) | respond.io API bearer token |
| `RESPOND_INCOMING_SIGNING_KEY` | — (off) | Signing key of the New Incoming Message webhook; verifies `X-Webhook-Signature` |
| `RESPOND_OUTGOING_SIGNING_KEY` | — (off) | Signing key of the New Outgoing Message webhook |
| `PORT` | `8080` | HTTP listen port |
| `DATA_DIR` | `./data` | Per-contact working dirs |
| `CONTACT_TEMPLATE` | — (off) | Directory whose entries are symlinked into each contact's cwd (e.g. a [gato](https://github.com/8monkey-ai/gato) checkout, so the harness finds its `.pi/` project config) |
| `TYPING_DELAY_MS_PER_WORD` | `1000` | Typing simulation; delay = words × this |
| `RESPOND_AI_ASSIGNEE_ID` | — (off) | respond.io user id the AI replies for; conversations assigned to anyone else are record-only |
| `RESPOND_API_URL` | `https://api.respond.io/v2` | Override for testing |

## Run

```sh
RESPOND_API_TOKEN=... go run .
```

`pi-acp` must be on the `PATH` (`npm install -g pi-acp`).

## Test

```sh
go test ./...
```

The end-to-end test builds a stub ACP agent (`./testagent`) and exercises the full webhook → prompt → streamed reply → chunked delivery flow against a fake respond.io API.
