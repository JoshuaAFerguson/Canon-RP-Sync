# Architecture

## Goals

- Import photos from an EOS RP without cables, at home and in the field.
- Never modify the card unless explicitly asked.
- One sync core shared by the daemon and both mobile apps.
- The same binary on a NAS, a laptop and a desktop.

## Shape

```
┌──────────────┐  CCAPI/HTTP    ┌──────────────────────────────────────┐
│  EOS RP      │◄──────────────►│ rpsync daemon                        │
│ (home Wi-Fi) │                │                                      │
└──────────────┘                │  watcher ──► sync ──► manifest       │
        ▲                       │     │         │                      │
        │                       │     └─► events bus ──► api ──► web UI│──► NAS storage
        │ CCAPI over the        │                         ▲            │
        │ camera's hotspot      └─────────────────────────┼────────────┘
        ▼                                                 │
┌──────────────┐   handover: POST /api/v1/ingest          │
│ iPad/Android │──────────────────────────────────────────┘
│ companion    │   over LAN, Tailscale or Cloudflare Tunnel
└──────────────┘
```

## Components

### Sync core (`internal/`)

Dependency-free, so it can later be exposed to mobile via `gomobile bind`:

- **`ccapi`** — HTTP client. Negotiates the API version from `GET /ccapi` rather
  than hard-coding `ver100`, serialises ordinary requests (the body accepts very
  few simultaneous connections), and gives long polls their own transport with
  no client-side timeout.
- **`discovery`** — SSDP M-SEARCH on every multicast-capable interface, with a
  bounded TCP probe of the local /24s as a fallback. The fallback exists because
  Docker bridge networks and VLANs routinely swallow multicast.
- **`exif`** — capture-date extraction: JPEG APP1, the CR3 `CMT1`/`CMT2` TIFF
  blocks inside the Canon `uuid` box, and `mvhd` for MP4.
- **`manifest`** — the import ledger, keyed by `card/dir/name`, indexed by public
  id and by content hash.
- **`sync`** — the importer: stage, inspect, place.
- **`thumb`** — JPEG previews, with a disk cache.

### Import pipeline

Every import takes the same three steps, whether the bytes came from the camera
or from a phone:

1. **Stage** — write to `dest/.rpsync-incoming/`, inside the destination volume.
2. **Inspect** — hash the bytes (SHA-256) and read the capture date.
3. **Place** — move into `dest/<layout>/` and record it.

Staging first is what makes capture-date folders possible at all: the date is
only knowable once the file is on disk. It also means a half-transferred file
never looks like a finished import, and the final step is a rename rather than a
copy.

Name collisions are resolved by comparing content: an identical file already at
the destination is adopted (so a lost manifest does not duplicate a library),
and a different one gets a `_1` suffix.

### Daemon (`cmd/rpsync`, `internal/watcher`)

The watcher owns the camera connection. Its loop is: resolve the address →
catch up on the card → block on `event/polling` → import what the event names →
repeat, reconnecting with a delay whenever the camera goes away.

Nothing else talks to the camera directly. That is a hard rule rather than
tidiness: the body accepts only a couple of simultaneous HTTP connections, so
the watcher cancels its long poll before any download starts, and the API asks
the watcher for a sync instead of driving one itself.

### API and web UI (`internal/api`)

A `net/http` server with Go 1.22 routing patterns and an embedded single-page
UI. Pairing issues bearer tokens; tokens are stored hashed. See
[`API.md`](API.md).

Pairing state lives on disk rather than in memory, which is what lets
`rpsync pair` mint a code in one process and the running daemon redeem it in
another. The device store re-reads its file when the mtime changes, so
`rpsync devices revoke` takes effect in a daemon that is already running.

### Remote access (`internal/remote`)

Reports the daemon's tailnet address, or supervises a `cloudflared` connector
with restart backoff. A tunnel failure never touches the import loop — remote
access is a component that can be unavailable while everything else works.

### Services (`internal/service`)

systemd units, launchd plists and Windows Service Manager registration, behind
one interface. The Windows path implements a real service control handler, so
stop and shutdown requests cancel the daemon's context and an in-flight import
finishes rather than being killed.

## Testing

`internal/cameratest` is a fake CCAPI camera: content listing with paging,
downloads, thumbnails, deletion, and a long-polling event endpoint that releases
when a test "takes a shot". The client, the sync core, the watcher and the API
are all tested against it, so the import path is exercised end to end without
hardware.

## Decisions

**Capture date over import date.** The original scaffold filed everything under
the import date, which is wrong the moment you import a card a week later. EXIF
is now the default (`storage.date_source`), with the camera's own timestamp and
then the import time as fallbacks.

**Content hashing.** Cheap relative to the transfer, and it buys deduplication
across the two import paths: an iPad can hand over a shot the daemon has not
seen, and the daemon will not fetch it again from the card.

**Bearer tokens, not a password.** The daemon is designed to be exposed through
a tunnel. Per-device tokens can be revoked individually, and a pairing code that
is single-use and short-lived is easier to use correctly than a shared secret.

**No RAW decoding.** Previews come from the JPEG in a RAW+JPEG pair, or from the
camera's own embedded thumbnail. Decoding CR3 would mean a heavy dependency for
a thumbnail.

## Companion apps (`apps/`)

Not started. Planned shape:

- Join the camera's hotspot programmatically (iOS `NEHotspotConfiguration`,
  Android `WifiNetworkSpecifier`).
- Use the same delta logic against a device-local manifest.
- Show CR3s using the embedded JPEG preview rather than decoding RAW.
- Hand off to the daemon with `POST /api/v1/ingest`, passing `card` and `dir` so
  the daemon knows not to pull the same frame over Wi-Fi later.

## Open questions

- Should the daemon push to the apps (APNs/FCM) when new photos land, or is
  polling `GET /photos?since=` enough?
- Two-way sync — writing photos back to the card — is almost certainly not worth
  it.
- Whether the apps get the sync core via `gomobile bind` or a native reimplementation.
