# nullclaw-channel-whatsmeow-bridge

Standalone HTTP bridge for `nullclaw` WhatsApp integrations built on top of
[`whatsmeow`](https://github.com/tulir/whatsmeow).

This repository is intentionally separate from `nullclaw` core. It owns the
actual WhatsApp login session, QR and pairing-code UX, websocket lifecycle, and
simple HTTP endpoints that an external `nullclaw` channel plugin can talk to.

## Why This Exists

The clean split is:

- `nullclaw`
  generic ExternalChannel host
- `nullclaw-channel-whatsmeow-bridge`
  WhatsApp session and bridge API
- optional adapter plugin
  a tiny stdio JSON-RPC layer if you want to keep the bridge HTTP-based

That keeps Go code out of the main `nullclaw` repository while still allowing
WhatsApp to be integrated cleanly.

## Endpoints

- `GET /health`
- `GET /qr`
- `POST /pair-code`
- `POST /poll`
- `POST /send`
- `POST /edit`
- `POST /delete`
- `POST /reaction`
- `POST /read`

## Configuration

Environment variables:

- `NULLCLAW_WHATSMEOW_BRIDGE_LISTEN`
  default `127.0.0.1:3301`
- `NULLCLAW_WHATSMEOW_BRIDGE_STATE_DIR`
  default `./state`
- `NULLCLAW_WHATSMEOW_BRIDGE_TOKEN`
  optional Bearer token required on every request when set
- `NULLCLAW_WHATSMEOW_BRIDGE_DISPLAY_NAME`
  default `Chrome (Linux)`

## Operator Flow

### QR flow

1. Start the bridge.
2. Open `GET /qr` until it returns `event=code`.
3. Render or copy the QR code value into your preferred UI.
4. Link the device from the WhatsApp phone app.
5. `GET /health` begins returning `logged_in=true`.

### Pair-code flow

1. Start the bridge.
2. Call `POST /pair-code` with `phone_number`.
3. Show the returned pairing code to the operator.
4. Complete linked-device flow on the phone.
5. `GET /health` begins returning `logged_in=true`.

The bridge persists its real WhatsApp session in `state/whatsmeow.db`.

## nullclaw Integration

This bridge is designed to be used behind a small external-channel plugin.
The existing example contract from `nullclaw` expects:

- `GET /health`
- `POST /poll`
- `POST /send`

This bridge implements those directly and also exposes edit/delete/reaction/read
endpoints for future richer adapters.

## Payload Shapes

### `POST /poll`

Request:

```json
{
  "account_id": "wa-main",
  "cursor": "0"
}
```

Response:

```json
{
  "next_cursor": "12",
  "messages": [
    {
      "id": "base64url-message-ref",
      "from": "551199999999@s.whatsapp.net",
      "text": "hello",
      "chat_id": "551199999999@s.whatsapp.net",
      "is_group": false
    }
  ]
}
```

### `POST /send`

Request:

```json
{
  "account_id": "wa-main",
  "to": "551199999999@s.whatsapp.net",
  "text": "hello"
}
```

### `POST /pair-code`

Request:

```json
{
  "phone_number": "551199999999",
  "display_name": "Chrome (Linux)"
}
```

Response:

```json
{
  "pairing_code": "ABCD-EFGH"
}
```

## Message ID Format

Inbound `messages[].id` is a base64url-encoded JSON envelope containing:

- WhatsApp message ID
- chat JID
- sender JID
- whether the message was sent by the current account
- timestamp

That allows later edit/delete/reaction/read actions to target the right
message without maintaining a separate lookup table in the plugin.

## Build

```bash
go build ./...
```

## References

- [whatsmeow README](https://github.com/tulir/whatsmeow)
- [whatsmeow pkg docs](https://pkg.go.dev/go.mau.fi/whatsmeow)

