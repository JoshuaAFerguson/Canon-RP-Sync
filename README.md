# canon-rp-sync

Wireless photo sync for the Canon EOS RP, built on Canon's [CCAPI](https://developercommunity.usa.canon.com/s/article/CCAPI).

- **`rpsync` daemon** — runs on a NAS / homelab box, waits for the camera to join your Wi-Fi, and pulls new JPEG/CR3 files into a dated folder structure.
- **Companion apps** (iPad + Android, planned) — connect to the camera's own hotspot while you're out, pull shots to the device, and hand them off to the daemon when you're home.

Import-only by design: the camera's card is never modified unless you explicitly enable `delete_after_import`.

## Status

Early scaffold. The daemon CLI is the first milestone:

- [ ] Discover the camera on the LAN (SSDP/UPnP)
- [ ] List new files since last run via CCAPI `contents`
- [ ] Download deltas into `YYYY/YYYY-MM-DD/`
- [ ] Track imported files in a local manifest
- [ ] Daemon mode using CCAPI `event/polling`
- [ ] iPad app
- [ ] Android app

## Prerequisites

1. **Enable CCAPI on the camera.** This is a one-time step using Canon's *CCAPI Activation Tool* (free download from the Canon Developer Community). After activation, the RP exposes an HTTP API on port 8080 whenever Wi-Fi is on.
2. Put the camera on your home Wi-Fi (Menu → Wireless settings → Wi-Fi/Bluetooth connection → *Connect to smartphone* or *Remote control (EOS Utility)* mode works; the API is available regardless of which app the camera thinks it's talking to).
3. Go 1.22+ to build the daemon.

## Quick start

```bash
go build -o bin/rpsync ./cmd/rpsync

# Discover the camera and print device info
./bin/rpsync discover

# One-shot import of anything not yet in the manifest
./bin/rpsync pull --dest ~/Photos/Canon

# Run continuously, importing as the camera appears / shoots
./bin/rpsync daemon --dest ~/Photos/Canon
```

Copy `rpsync.example.yaml` to `rpsync.yaml` to pin a camera IP, filter by file type, etc.

## Repository layout

```
cmd/rpsync/        CLI + daemon entrypoint
internal/ccapi/    Canon CCAPI HTTP client (device info, contents, download, events)
internal/discovery SSDP discovery of CCAPI cameras on the LAN
internal/manifest/ Local record of what has already been imported
internal/sync/     Delta computation and download orchestration
apps/ios/          iPad companion app (planned)
apps/android/      Android companion app (planned)
deploy/            Dockerfile, systemd unit, docker-compose example
docs/              Design notes and CCAPI reference
```

## Design notes

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the overall design and [`docs/CCAPI.md`](docs/CCAPI.md) for the subset of CCAPI this project uses.

## License

MIT — see [LICENSE](LICENSE).
