# momo

momo connects your channels to AI agents. A channel is anywhere a contact reaches you — WhatsApp, Messenger, or Telegram through [respond.io](https://respond.io) today, with email and other kinds of channels to follow. Every contact gets their own private, persistent agent: momo passes each message to that contact's agent and delivers the reply back over the same channel. Each conversation is a one-on-one with an agent that remembers the contact across messages.

## Install

momo ships as a single static binary. Pick one:

- **Prebuilt binary** *(WIP)*: download the binary for your platform from [GitHub Releases](https://github.com/8monkey-ai/momo/releases) and place it in your PATH.

- **Docker** *(WIP)*:

  ```sh
  docker pull 8monkey/momo
  ```

- **Build from source** (needs [Docker](https://docs.docker.com/get-docker/) or [Go](https://go.dev/dl/)):

  ```sh
  git clone https://github.com/8monkey-ai/momo && cd momo
  docker build -t momo .        # or: go build -o momo .
  ```

## Quick start

You need a host reachable from the internet (channels deliver messages via webhooks).

1. **Create your config file** and fill in your channel credentials (see [Configuration](#configuration) below):

   ```sh
   cp momo.example.conf momo.conf
   ```

2. **Start momo:**

   ```sh
   docker run -d -p 8080:8080 -v ./momo.conf:/etc/momo/momo.conf momo
   ```

   Or with the binary: `momo -config momo.conf`.

   The logs should show:

   ```
   🐒 momo listening on :8080
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

## HTTP routes

Each enabled channel registers its own routes at startup, so the paths momo serves depend on which channels you enable. momo itself serves only `GET /healthz`.

| Route | Registered by | Purpose |
|---|---|---|
| `GET /healthz` | momo | Health check; returns 200 once the server is up. |
| `POST /respondio` | `respondio` channel | Receives respond.io webhook events. |

## Connecting a channel

A channel is where your contacts reach you. Enable one by adding its `[channels.<name>]` section to the config.

### respond.io

[respond.io](https://respond.io) aggregates WhatsApp, Facebook Messenger, Telegram, and more into one workspace, and is the first channel momo supports.

1. In respond.io, go to **Settings → Integrations → Webhook**.

2. Register two webhooks, both pointing at your momo server:

   ```
   https://<your-host>/respondio
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

*(WIP)* The agent harness — running an [ACP](https://agentclientprotocol.com) agent per contact — lands in follow-up releases. Until then, momo receives, verifies, and logs channel events.

