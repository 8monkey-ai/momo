# momo

momo connects messaging channels to AI agents. Every contact gets their own agent: when a contact writes on a channel, momo prompts that contact's agent and sends the agent's replies back over the same channel — each conversation is a private, persistent one-on-one with its own agent.

## Setup

### 1. Run the server

momo is configured with a YAML file (default `./config.yaml`, override with `-config <path>`):

```sh
cp config.example.yaml config.yaml
go run .            # or: go run . -config /etc/momo/config.yaml
```

```yaml
port: 8080          # HTTP listen port
channels:
  respondio:
    incoming_signing_key: "..."
    outgoing_signing_key: "..."
```

### 2. Connect a channel

A channel is where contacts message from. Each channel is enabled by the presence of its section under `channels:` in the config; [respond.io](https://respond.io) (WhatsApp, Messenger, Telegram, …) is the first supported channel.

In respond.io, go to Settings → Integrations → Webhook and register two webhooks, both pointing at `https://<host>/webhook/respondio`:

- **New Incoming Message** — fires when a contact messages the workspace.
- **New Outgoing Message** — fires when an operator (or the agent itself) replies.

Each registered webhook has its own signing key; copy them into `incoming_signing_key` and `outgoing_signing_key`. momo verifies every event's `X-Webhook-Signature` against the matching key and rejects mismatches.

### 3. Connect an agent

Not available yet: this build receives, verifies, and logs channel events. The agent harness — running an [ACP](https://agentclientprotocol.com) agent such as pi per contact — lands in follow-up PRs.

## Development

Channel implementations live under `channel/`; each translates its transport into the channel-neutral messages the core pipeline consumes.

```sh
go test ./...
```
