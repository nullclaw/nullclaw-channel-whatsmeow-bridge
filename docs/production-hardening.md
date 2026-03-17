# Production Hardening

This bridge is a separate daemon that owns a real WhatsApp linked-device
session. Treat it as sensitive infrastructure.

## Recommended Topology

- bind the bridge to loopback by default
- keep it on the same host as `nullclaw` when possible
- if you expose it remotely, put TLS and auth in front of it

The default recommended shape is:

```text
nullclaw -> local adapter/plugin -> 127.0.0.1:3301 -> whatsmeow bridge
```

## Authentication

Use `NULLCLAW_WHATSMEOW_BRIDGE_TOKEN` unless the bridge is strictly local and
inaccessible outside the service host.

If the token is set, every request must send:

```http
Authorization: Bearer <token>
```

## State

The real WhatsApp session is stored in:

```text
<state dir>/whatsmeow.db
```

Recommendations:

- keep the state directory on persistent storage
- restrict permissions to the bridge service user
- back it up only if you are comfortable backing up live linked-device credentials

Suggested ownership:

```bash
chown -R nullclaw:nullclaw /var/lib/nullclaw-channel-whatsmeow-bridge
chmod 700 /var/lib/nullclaw-channel-whatsmeow-bridge
```

## systemd

Use the provided unit:

- [deploy/systemd/nullclaw-channel-whatsmeow-bridge.service](../deploy/systemd/nullclaw-channel-whatsmeow-bridge.service)

Typical setup:

1. Install the binary into `/usr/local/bin`.
2. Copy the unit to `/etc/systemd/system/`.
3. Optionally create `/etc/default/nullclaw-channel-whatsmeow-bridge` with:

```bash
NULLCLAW_WHATSMEOW_BRIDGE_TOKEN=change-me
```

4. Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now nullclaw-channel-whatsmeow-bridge
```

## Docker Compose

Use:

- [deploy/docker-compose.yml](../deploy/docker-compose.yml)

Recommended adjustments before production:

- replace `change-me` with a real token
- keep port publication on `127.0.0.1`
- mount a persistent named volume or host path

## QR and Pair-Code Operations

For headless production hosts, pairing code is often easier than QR.

Use QR when:

- an operator can access `/qr` easily
- you already have a web UI or internal admin surface for rendering QR codes

Use pairing code when:

- the host is remote and headless
- copy-paste flow is easier than QR rendering

## Logging

Watch:

- bridge startup and reconnect logs
- `logged_out` events
- repeated websocket disconnects
- auth failures or 401 responses from callers

## Upgrade Advice

1. stop the service
2. keep a backup of the binary and state directory
3. deploy the new version
4. start the service
5. confirm `/health` still returns `logged_in=true`

## When To Re-Link

You need a new QR or pairing flow when:

- WhatsApp explicitly logs out the linked device
- the state directory is lost or corrupted
- you intentionally rotate to a new linked device

