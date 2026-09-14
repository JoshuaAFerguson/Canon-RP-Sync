# Architecture

## Goals

- Import photos from an EOS RP without cables, at home and in the field.
- Never modify the card unless explicitly asked.
- One sync core shared by the daemon and both mobile apps.

## Components

```
┌──────────────┐  CCAPI/HTTP   ┌───────────────┐
│  EOS RP      │◄─────────────►│  rpsync daemon │──► NAS storage
│  (home Wi-Fi)│               │  (homelab)     │
└──────────────┘               └───────────────┘
        ▲
        │ CCAPI/HTTP over camera hotspot
        ▼
┌──────────────┐   later, over LAN or S3-compatible bucket
│ iPad/Android │────────────────────────────────────────► rpsync daemon
│ companion    │
└──────────────┘
```

### Sync core (`internal/`)

- `ccapi` — HTTP client. Device info, recursive content listing, download, delete, event polling.
- `discovery` — SSDP M-SEARCH; filters responders containing "Canon".
- `manifest` — JSON set of imported files keyed by `card/dir/name`.
- `sync` — delta = camera files − manifest; downloads and records.

The core is deliberately dependency-free so it can later be exposed to mobile
via `gomobile bind` (or ported to Rust + UniFFI if that proves cleaner).

### Daemon (`cmd/rpsync`)

Loop: discover → catch up → block on `event/polling` → import on `addedcontents`.
Falls back to periodic retry when the camera is off or out of range.

### Companion apps (`apps/`)

Not started. Planned shape:

- Join the camera's hotspot programmatically (iOS `NEHotspotConfiguration`,
  Android `WifiNetworkSpecifier`).
- Same delta logic against a device-local manifest.
- Show CR3s using the embedded JPEG preview rather than decoding RAW.
- Hand off to the daemon when home (direct LAN) or via an S3-compatible bucket.

## Open decisions

- Capture-date folders: parse EXIF `DateTimeOriginal` (JPEG) / CR3 `CMT1` box vs. import date. Currently import date.
- Mobile → home handoff transport: LAN push vs. S3-compatible bucket.
- Whether to support two-way sync (probably not).
