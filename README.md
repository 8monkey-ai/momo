# agent-server

Drives one AI agent per contact across messaging channels: incoming messages prompt the contact's agent, and its replies are sent back through the channel the contact wrote on.

Channel implementations live under `channel/`; each translates its transport into the channel-neutral messages the core pipeline consumes. [respond.io](https://respond.io) is the first channel.

## respond.io channel

Register two webhooks in respond.io (Settings → Integrations → Webhook), pointing both at the same URL, `https://<host>/webhook/respondio` (the server dispatches on `event_type`). Each registered webhook has its own signing key; set them via `RESPOND_INCOMING_SIGNING_KEY` / `RESPOND_OUTGOING_SIGNING_KEY` and the server verifies the `X-Webhook-Signature` header (base64 HMAC-SHA256 of the raw body) per event:

- **New Incoming Message** (`message.received`) — a contact messaged the workspace.
- **New Outgoing Message** (`message.sent`) — an operator (or the agent itself) replied.

Events are acked immediately and processed asynchronously.

## Configuration

| Env var | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port |
| `RESPOND_INCOMING_SIGNING_KEY` | — (off) | Signing key of the New Incoming Message webhook; verifies `X-Webhook-Signature` |
| `RESPOND_OUTGOING_SIGNING_KEY` | — (off) | Signing key of the New Outgoing Message webhook |

## Run

```sh
go run .
```

## Test

```sh
go test ./...
```
