# agent-server

A bridge between [respond.io](https://respond.io) and any agent harness that speaks the
[Agent Client Protocol (ACP)](https://agentclientprotocol.com). It receives respond.io
webhooks, drives one harness subprocess per contact over ACP, and sends the agent's
replies back through the respond.io API — paced like human typing.

```
respond.io ──webhook POST──▶  agent-server  ──JSON-RPC/stdio──▶ ACP harness (per contact)
respond.io ◀──REST API ─────  (this binary) ◀── session/update ─┘
```

## How it works

- **One harness subprocess and one ACP session per contact**, spawned on first message.
  The respond.io contact ID is the routing key; the harness issues the session ID.
- **Per-contact working directory** (`DATA_DIR/<contactId>`), passed as the session
  `cwd`. This is how the harness identifies the user and where it persists their chat
  history — returning users continue their previous conversation.
- **Session resume**: the contact→session mapping is persisted in
  `DATA_DIR/sessions.json`; if the harness supports `session/load`, conversations
  survive agent-server restarts.
- **Steering**: a message arriving while a turn is streaming cancels the active turn
  (`session/cancel`) and prompts with the new message.
- **Human-paced delivery**: streamed output is split on paragraph boundaries (`\n\n`)
  and each paragraph is sent as a separate respond.io message, delayed proportionally
  to its length.
- **Attachments**: images and audio (voice messages) are downloaded and sent to the
  agent as inline content blocks; other files are saved into the contact's working
  directory and referenced with a resource link.
- **Permissions**: tool-call permission requests from the harness are auto-approved
  (the harness uses its own fs/terminal tools inside the contact's cwd).

## Webhooks

Register these in respond.io (Settings → Integrations → Webhook), pointing at
`https://<host>/webhook`:

- **New Incoming Message** (`message.received`) — required. Each text message becomes
  a prompt turn.
- **New Outgoing Message** (`message.sent`) — optional. Messages sent by others
  (human operators, workflows) are recorded into the harness context by prompting with
  `OUTGOING_COMMAND` followed by the message text; nothing is delivered back. Messages
  sent by agent-server itself are recognized by messageId and skipped, preventing echo
  loops.

## Configuration

| Env var | Default | Description |
| --- | --- | --- |
| `RESPOND_API_TOKEN` | — (required) | respond.io API bearer token |
| `AGENT_CMD` | — (required) | Harness command, e.g. `claude-code-acp` or `gemini --experimental-acp` |
| `PORT` | `8080` | HTTP listen port |
| `DATA_DIR` | `./data` | Per-contact working dirs and session store |
| `TYPING_DELAY_MS_PER_CHAR` | `30` | Typing simulation; delay = chars × this (capped at 10s) |
| `OUTGOING_COMMAND` | — (off) | Slash command prefix for recording operator messages |
| `RESPOND_API_URL` | `https://api.respond.io/v2` | Override for testing |

## Run

```sh
RESPOND_API_TOKEN=... AGENT_CMD="gemini --experimental-acp" go run .
```

## Test

```sh
go test ./...
```

The end-to-end test builds a stub ACP agent (`./testagent`) and exercises the full
webhook → prompt → streamed reply → chunked delivery flow against a fake respond.io API.
