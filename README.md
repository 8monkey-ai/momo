# momo

momo connects your messaging channels to AI agents. Every contact who writes to you gets their own private, persistent agent: when they send a message on WhatsApp, Messenger, Telegram, or any other connected channel, momo passes it to that contact's agent and delivers the agent's reply back over the same channel. Each conversation is a one-on-one with its own agent that remembers the contact across messages.

## Quick start

You need [Go](https://go.dev/dl/) installed and a host reachable from the internet (channels deliver messages via webhooks).

1. **Create your config file:**

   ```sh
   cp momo.example.conf momo.conf
   ```

2. **Fill in `momo.conf`** with your port and channel credentials (see [Configuration](#configuration) below).

3. **Start the server:**

   ```sh
   go run .
   ```

   You should see:

   ```
   🐒 momo listening on :8080
   ```

To use a config file from another location, pass `-config`:

```sh
go run . -config /etc/momo/momo.conf
```

## Configuration

momo reads a single config file (default `./momo.conf`):

```ini
port = 8080         # HTTP listen port

[channels.respondio]
incoming_signing_key = ...
outgoing_signing_key = ...
```

| Setting | What it does |
|---|---|
| `port` | Port momo listens on for incoming webhooks. |
| `[channels.<name>]` | Enables a channel. A channel is active if and only if its section is present. |

Where the signing keys come from is covered in the channel setup below.

## Connecting a channel

A channel is where your contacts message from. Enable one by adding its `[channels.<name>]` section to the config.

### respond.io

[respond.io](https://respond.io) aggregates WhatsApp, Facebook Messenger, Telegram, and more into one workspace, and is the first channel momo supports.

1. In respond.io, go to **Settings → Integrations → Webhook**.

2. Register two webhooks, both pointing at your momo server:

   ```
   https://<your-host>/webhook/respondio
   ```

   - **New Incoming Message** — fires when a contact messages your workspace.
   - **New Outgoing Message** — fires when a reply goes out (from an operator or the agent itself).

3. respond.io shows a **signing key** for each webhook you register. Copy them into your config:

   ```ini
   [channels.respondio]
   incoming_signing_key = <key from the New Incoming Message webhook>
   outgoing_signing_key = <key from the New Outgoing Message webhook>
   ```

4. Restart momo. It now verifies the signature of every event and rejects anything that doesn't match — so keep the keys in your config in sync with respond.io if you ever regenerate them.

## Connecting an agent

The agent harness — running an [ACP](https://agentclientprotocol.com) agent per contact — lands in follow-up releases. Until then, momo receives, verifies, and logs channel events.

