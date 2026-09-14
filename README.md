# canon-rp-sync

Wireless photo sync for the Canon EOS RP, built on Canon's [CCAPI](https://developercommunity.usa.canon.com/s/article/CCAPI).

`rpsync` watches your network for the camera, pulls every new JPEG/CR3 into a
dated folder tree, and serves a small HTTP API and web UI so phones and tablets
can browse the library and hand over shots they took in the field.

It runs the same way everywhere:

- **Docker** on a Synology NAS or any Linux box (`linux/amd64` and `linux/arm64`)
- **A background service** on Windows, macOS and Linux — `rpsync service install`
- **In the foreground** — `rpsync daemon`

Import-only by design: the card is never modified unless you explicitly enable
`delete_after_import`.

## What it does

```
        ┌──────────────┐  CCAPI over home Wi-Fi   ┌────────────────┐
        │   EOS RP     │─────────────────────────►│  rpsync daemon │──► /photos/2024/2024-05-04/
        └──────────────┘                          │                │
               │                                  │  HTTP API      │
               │ CCAPI over the camera's hotspot  │  + web UI      │
               ▼                                  └────────────────┘
        ┌──────────────┐    hand over later, over LAN       ▲
        │ iPad/Android │    or Tailscale / Cloudflare ──────┘
        └──────────────┘
```

- Finds the camera by SSDP, falling back to a subnet probe when multicast is
  blocked (Docker bridge networks, VLANs).
- Catches up on the card, then blocks on CCAPI's event stream and imports each
  frame as it is shot.
- Files photos by **capture date**, read from EXIF (JPEG) or the CR3 metadata,
  not by when the import happened.
- Tracks everything in a manifest, so restarts and re-runs only move new files.
- Deduplicates by content hash: a shot your iPad already handed over is not
  downloaded again from the card.

## Quick start

### Docker (Synology, or any Linux host)

```bash
cd deploy
docker compose up -d
docker compose logs -f rpsync     # the first pairing code is printed here
```

Then open `http://<host>:8787` and enter the code. See
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for the Synology specifics
(shared folders, PUID/GUID, host networking).

### Windows, macOS or Linux desktop

```bash
rpsync service install       # systemd / launchd / Windows Service Manager
rpsync service start
rpsync pair                  # prints a code for your phone or browser
```

### Foreground, no install

```bash
rpsync discover              # find the camera and print what it says
rpsync pull --dest ~/Photos/Canon     # import once and exit
rpsync daemon --dest ~/Photos/Canon   # watch continuously, serve the UI
```

## Before the first run

1. **Activate CCAPI on the camera.** One-time, over USB, with Canon's *CCAPI
   Activation Tool* (free, from the Canon Developer Community). Until you do,
   the camera has no HTTP API at all.
2. **Put the camera on your Wi-Fi.** Menu → Wireless settings → Wi-Fi/Bluetooth
   connection. *Connect to smartphone* or *Remote control (EOS Utility)* both
   work; the API is served regardless of which app the camera thinks it is
   talking to.
3. Leave the camera awake long enough to be found — auto power-off ends the
   Wi-Fi session, and rpsync will simply wait and reconnect.

## Configuration

Every setting has a working default. To change one, use whichever layer suits:

| Layer | Example | Notes |
|---|---|---|
| `rpsync.yaml` | `camera: {host: 192.168.1.42}` | `rpsync config --path` lists the search locations |
| Environment | `RPSYNC_CAMERA_HOST=192.168.1.42` | The usual choice for Docker and Synology |
| Flags | `rpsync daemon --host 192.168.1.42` | Highest precedence |

`rpsync config` prints the effective result. See
[`rpsync.example.yaml`](rpsync.example.yaml) for every key.

## Pairing and remote access

Everything except `/api/v1/health` and the pairing endpoint needs a bearer
token, because this daemon is meant to be reachable from outside the LAN.

```bash
rpsync pair            # prints an 8-character code, valid once, for 10 minutes
rpsync devices         # list paired devices
rpsync devices revoke <id>
```

For access away from home, rpsync can advertise its address on a **tailnet**, or
supervise a **Cloudflare Tunnel** — neither needs a port forwarded on your
router. See [`docs/REMOTE_ACCESS.md`](docs/REMOTE_ACCESS.md).

## HTTP API

The companion apps are built on it, and it is stable enough to script against:
status, photo listings, downloads, thumbnails, upload handover, and a
server-sent event stream. See [`docs/API.md`](docs/API.md).

## Status

The daemon is the working part of the project today.

- [x] Discover the camera on the LAN (SSDP, with a subnet-probe fallback)
- [x] List and import new files via CCAPI `contents`
- [x] Capture-date folders from EXIF / CR3 metadata
- [x] Manifest-tracked delta imports, deduplicated by content hash
- [x] Daemon mode driven by CCAPI `event/polling`
- [x] HTTP API, web UI, device pairing
- [x] Photo handover endpoint for the companion apps
- [x] Tailscale / Cloudflare Tunnel remote access
- [x] Docker image (amd64 + arm64) and native services
- [ ] iPad app
- [ ] Android app

## Repository layout

```
cmd/rpsync/        CLI, daemon and service entrypoint
internal/ccapi/    Canon CCAPI client (device info, contents, download, events)
internal/discovery SSDP search and subnet probing
internal/exif/     Capture-date extraction (JPEG, CR3, MP4)
internal/manifest/ The import ledger
internal/sync/     Delta computation, staging, placement, handover ingest
internal/watcher/  Camera connection lifecycle
internal/api/      HTTP API, pairing, embedded web UI
internal/remote/   Tailscale / Cloudflare Tunnel integration
internal/service/  systemd, launchd and Windows Service Manager installers
internal/cameratest A fake CCAPI camera used throughout the tests
apps/ios/          iPad companion app (planned)
apps/android/      Android companion app (planned)
deploy/            Dockerfile, compose, systemd unit, launchd plist
docs/              Architecture, deployment, API and CCAPI notes
```

## Building

```bash
make build      # ./bin/rpsync
make test       # go test -race ./...
make lint       # go vet + gofmt check
make docker     # container image for the host architecture
```

Go 1.22+ is required. The only dependencies are `gopkg.in/yaml.v3` (config) and
`golang.org/x/sys` (Windows service); the sync core itself is dependency-free so
it can be bound into the mobile apps.

## License

MIT — see [LICENSE](LICENSE).
