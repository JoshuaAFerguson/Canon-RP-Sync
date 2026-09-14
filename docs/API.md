# rpsync HTTP API

Base path: `/api/v1`. All responses are JSON unless noted.

The companion apps are built on this API. It is also perfectly reasonable to
script against with `curl`.

## Authentication

Every endpoint except `GET /health` and `POST /pair` requires a device token.

Present it in whichever form suits the client:

| Form | Use |
|---|---|
| `Authorization: Bearer <token>` | Native apps and scripts |
| `X-RPSync-Token: <token>` | Clients that cannot set `Authorization` |
| `rpsync_token` cookie | The web UI; set automatically when pairing in a browser |
| `?token=<token>` | `EventSource` and `<img>` tags, which cannot set headers |

Tokens are issued by pairing and stored only as a SHA-256 hash, so a leaked
`devices.json` does not reveal working credentials. A revoked token stops
working immediately, including in a daemon that is already running.

When `server.allow_local_unauthenticated` is true, requests from loopback are
accepted without a token and act as the device "This computer".

## Pairing

A code is eight characters from an unambiguous alphabet, shown as `XXXX-XXXX`.
It is valid once, expires after ten minutes by default, and is destroyed after
five wrong guesses — the pairing endpoint is the one route an attacker can reach
without credentials.

Get a code with `rpsync pair` on the host, from the daemon's startup log on a
fresh install, or from the web UI's **Pair a device** button.

### `POST /pair`

No authentication. Redeems a code for a token.

```jsonc
// request
{"code": "53K3-LUFZ", "name": "Josh's iPad", "platform": "ios"}

// 200
{
  "token": "rps_1Zc…",          // shown once, never retrievable again
  "device": {"id": "9f2c…", "name": "Josh's iPad", "platform": "ios",
             "created_at": "2024-05-04T10:30:00Z"},
  "base_url": "https://rp.example.com"   // when advertise_url is configured
}
```

| Status | Meaning |
|---|---|
| 200 | Paired |
| 400 | Malformed body |
| 403 | Wrong code, expired, or too many attempts |
| 409 | No code is currently active |

### `POST /pair/code`

Authenticated. Mints a new code (invalidating any previous one) so an existing
device can enrol another.

```json
{"code": "AB3D-7KMP", "expires_at": "2024-05-04T10:40:00Z"}
```

### `DELETE /pair/code`

Authenticated. Cancels the active code.

## Status

### `GET /health`

No authentication, and deliberately reveals nothing about the camera or the
library — it exists for container healthchecks and tunnel probes.

```json
{"service": "rpsync", "status": "ok", "version": "1.0.0"}
```

### `GET /status`

Everything the web UI renders.

```jsonc
{
  "version": "1.0.0",
  "camera": {
    "state": "connected",          // searching | connected | importing | stopped
    "since": "2024-05-04T10:29:00Z",
    "camera": {"host": "192.168.1.42", "port": 8080, "model": "Canon EOS RP",
               "serial_number": "0123456789", "firmware_version": "1.6.0",
               "source": "ssdp"},  // config | ssdp | scan
    "last_sync": "2024-05-04T10:30:02Z",
    "last_imported": 2,
    "total_imported": 148,
    "progress": {"file": "IMG_0042.CR3", "done": 3, "total": 11}
  },
  "library": {"files": 148, "bytes": 3221225472, "last_import": "…"},
  "storage": {"dest": "/photos", "layout": "2006/2006-01-02", "manifest": "…"},
  "remote": {"mode": "tailscale", "state": "ready", "url": "http://nas.tail1234.ts.net:8787"},
  "pairing": {"active": true, "expires_at": "…"},
  "devices": 2,
  "recent": [{"kind": "import", "time": "…", "message": "imported IMG_0042.CR3"}],
  "photos": [ /* the 24 most recent entries */ ]
}
```

## Photos

### `GET /photos`

| Parameter | Default | Meaning |
|---|---|---|
| `limit` | 100 | Maximum entries |
| `offset` | 0 | Skip, for paging |
| `ext` | — | Filter to one extension, e.g. `cr3` |
| `since` | — | RFC3339; only files imported after this time |

Ordered newest first by capture time, falling back to import time.

```jsonc
{
  "photos": [{
    "id": "f022b2e6c78c0175",     // stable; use it to address the photo
    "card": "sd", "dir": "100CANON", "name": "IMG_0042.CR3",
    "dest": "/photos/2024/2024-05-04/IMG_0042.CR3",
    "ext": "cr3", "size": 27311104,
    "sha256": "3767e0d7…",
    "source": "camera",           // or "device:Josh's iPad"
    "captured_at": "2024-05-04T10:31:00Z",
    "imported_at": "2024-05-04T10:31:04Z"
  }],
  "total": 148
}
```

`since` is how a companion app syncs incrementally: remember the timestamp of
the last entry you saw and ask for everything after it.

### `GET /photos/{id}`

One entry, in the shape above.

### `GET /photos/{id}/file`

The imported file itself, served with `Content-Disposition: inline`. Supports
range requests, so a client can resume a large CR3.

### `GET /photos/{id}/thumb?size=480`

A JPEG preview. rpsync downscales and caches JPEGs itself; for a CR3 it uses the
JPEG shot alongside it, and failing that the camera's own embedded thumbnail
while the card is still reachable. `404` when no preview can be produced —
clients should fall back to a placeholder rather than treating it as an error.

## Handover

### `POST /ingest`

Hands a photo to the daemon. This is the path the companion apps use after
pulling shots over the camera's own hotspot in the field.

Two body forms are accepted:

```bash
# raw body
curl -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE/api/v1/ingest?name=IMG_0042.CR3&card=sd&dir=100CANON&captured_at=2024-05-04T10:31:00Z" \
  --data-binary @IMG_0042.CR3

# multipart
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -F file=@IMG_0042.CR3 -F card=sd -F dir=100CANON \
  "$BASE/api/v1/ingest"
```

| Field | Where | Meaning |
|---|---|---|
| `name` | query, form, multipart filename, or `X-RPSync-Filename` | Required |
| `card`, `dir` | query or form | The file's identity on the card. Supply them and the daemon will not download the same shot again over Wi-Fi |
| `captured_at` | query or form | RFC3339. EXIF inside the uploaded bytes wins if it disagrees |

```json
{"imported": true, "entry": { /* manifest entry */ }}
```

`"imported": false` means the daemon already had this file — by camera identity
or by content hash — and nothing was written. It is a success, not an error:
clients can safely re-upload after an interrupted transfer.

| Status | Meaning |
|---|---|
| 200 | Accepted (check `imported`) |
| 400 | No file name |
| 413 | Larger than `server.max_upload_bytes` |

### `POST /sync`

Asks the watcher to catch up with the card now, rather than waiting for the next
camera event. Returns `202` immediately; watch `/events` for the result.

## Devices

### `GET /devices`

```json
{"devices": [{"id": "9f2c…", "name": "Josh's iPad", "platform": "ios",
              "created_at": "…", "last_seen": "…"}]}
```

### `DELETE /devices/{id}`

Revokes a device. Any paired device may revoke another, including itself — this
is a personal tool, not a multi-tenant one.

## Events

### `GET /events`

A `text/event-stream` of daemon activity. The last 25 events are replayed on
connect so a freshly loaded page is not blank, and a comment heartbeat is sent
every 25 seconds to stop proxies and tunnels closing an idle stream.

```
event: import
data: {"kind":"import","time":"2024-05-04T10:31:04Z","message":"imported IMG_0042.CR3","data":{…}}
```

Kinds: `camera`, `import`, `remote`, `pairing`, `error`.

Because `EventSource` cannot set headers, browsers authenticate with the cookie
set at pairing time; other clients can pass `?token=`.

## Errors

Every error has the same shape:

```json
{"error": "pairing code is incorrect"}
```
