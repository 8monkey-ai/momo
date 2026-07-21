# agent-server

A bridge between [respond.io](https://respond.io) and an AI agent: it receives respond.io webhooks and will drive one agent per contact, sending replies back through the respond.io API.

## Webhooks

Register these in respond.io (Settings → Integrations → Webhook), pointing both at the same URL, `https://<host>/webhook` (the server dispatches on `event_type`). Each registered webhook has its own signing key; set them via `RESPOND_INCOMING_SIGNING_KEY` / `RESPOND_OUTGOING_SIGNING_KEY` and the server verifies the `X-Webhook-Signature` header (base64 HMAC-SHA256 of the raw body) per event:

- **New Incoming Message** (`message.received`) — a contact messaged the workspace.
- **New Outgoing Message** (`message.sent`) — an operator (or the agent itself) replied.

Events are acked immediately and processed asynchronously: an agent turn will far outlive respond.io's webhook timeout.

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
