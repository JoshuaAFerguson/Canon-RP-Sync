# Deployment

rpsync is a single static binary with an embedded web UI. Pick whichever of
these fits the machine you want it running on.

- [Synology NAS (Docker)](#synology-nas-docker)
- [Any Linux host (Docker)](#any-linux-host-docker)
- [Linux (systemd)](#linux-systemd)
- [macOS (launchd)](#macos-launchd)
- [Windows (Service Manager)](#windows-service-manager)
- [Storage layout](#storage-layout)
- [Troubleshooting](#troubleshooting)

## Synology NAS (Docker)

Synology is the deployment this project was built for, and it has two quirks
worth knowing before you start: **UIDs** and **multicast**.

### 1. Prepare a shared folder

Create (or pick) a shared folder for the imports, e.g. `/volume1/photo/Canon`.
Note the UID and GID of the account that should own the files:

```bash
ssh admin@nas
id your-username        # -> uid=1026(you) gid=100(users)
```

### 2. Deploy

Copy `deploy/docker-compose.yml` to the NAS and edit three things: the volume
path, the `user:` line, and `TZ`.

```yaml
services:
  rpsync:
    image: ghcr.io/joshuaaferguson/canon-rp-sync:latest
    container_name: rpsync
    restart: unless-stopped
    network_mode: host          # needed for SSDP discovery
    user: "1026:100"            # the ids from step 1
    environment:
      - TZ=America/Los_Angeles
      - RPSYNC_INCLUDE=jpg,cr3
    volumes:
      - /volume1/photo/Canon:/photos
      - /volume1/docker/rpsync:/config
```

```bash
sudo docker compose up -d
sudo docker compose logs -f rpsync
```

The log prints a pairing code on the first run. Open `http://<nas>:8787`, enter
it, and you are in.

> **Container Manager (the DSM GUI)** works too: import the compose file as a
> project. If you use the older *Docker* package instead, enable **Use the same
> network as Docker Host** and add the volume and environment entries by hand.

### 3. Why `network_mode: host`

The camera is found by SSDP, which is multicast, and multicast does not cross
Docker's bridge network. Without host networking, rpsync falls back to probing
the local subnet — which only helps if the container shares that subnet.

If you would rather not use host networking, pin the camera instead:

```yaml
    network_mode: bridge
    ports: ["8787:8787"]
    environment:
      - RPSYNC_CAMERA_HOST=192.168.1.42
      - RPSYNC_CAMERA_DISCOVERY=false
```

Give the camera a DHCP reservation on your router first, so the address does not
move.

### 4. Timezone

Set `TZ`. Capture-date folders come from the camera's own clock, and without a
timezone the container runs in UTC — which puts evening shots in tomorrow's
folder.

## Any Linux host (Docker)

Identical to the above, minus the Synology specifics:

```bash
docker run -d --name rpsync --restart unless-stopped \
  --network host \
  -e TZ=Europe/London \
  -v /srv/photos/canon:/photos \
  -v /srv/rpsync:/config \
  ghcr.io/joshuaaferguson/canon-rp-sync:latest
```

The image runs as UID 1000 and is published for `linux/amd64` and `linux/arm64`.

## Linux (systemd)

```bash
sudo install -m 0755 rpsync /usr/local/bin/rpsync
rpsync service install          # user service; add --system for all users
rpsync service start
rpsync service status
```

A user service only runs while you are logged in. To keep it running after you
log out:

```bash
sudo loginctl enable-linger "$USER"
```

For a system-wide install, run as root and give it an account to run as:

```bash
sudo rpsync service install --system --user rpsync --dest /srv/photos/canon
```

`rpsync service install` writes the unit, reloads systemd and enables it; it
prints the file path so you can inspect or edit it. `deploy/rpsync.service` is a
reference copy for configuration-managed hosts.

## macOS (launchd)

```bash
sudo install -m 0755 rpsync /usr/local/bin/rpsync
rpsync service install          # LaunchAgent, runs while you are logged in
rpsync service start
```

Logs go to `~/Library/Logs/rpsync/`. Running as root installs a LaunchDaemon in
`/Library/LaunchDaemons` instead, which starts at boot.

macOS will ask for permission the first time rpsync touches the local network —
allow it, or discovery will find nothing.

## Windows (Service Manager)

From an **Administrator** prompt:

```powershell
rpsync.exe service install --dest "D:\Photos\Canon"
rpsync.exe service start
rpsync.exe service status
```

This registers a real Windows service that starts automatically at boot and
restarts itself if it crashes. `rpsync service uninstall` removes it.

Allow rpsync through Windows Defender Firewall on **private** networks when
prompted — it needs to receive SSDP replies and serve the API.

## Storage layout

```
/photos
├── 2024
│   └── 2024-05-04
│       ├── IMG_0041.JPG
│       └── IMG_0042.CR3
├── .rpsync-manifest.json      # the import ledger
└── .rpsync-incoming/          # transient staging, emptied at startup
```

The folder layout is configurable (`storage.layout`) as a Go time format —
`2006/01` for year/month, `2006-01-02` for flat dated folders.

`/config` holds `devices.json` (paired device tokens), `pairing.json` (the
active pairing code, if any) and cached thumbnails. Back up `/photos` for the
photos and `/config` if you would rather not re-pair after a rebuild.

### Moving or backing up

The manifest keys on the camera-side identity of each file, so it moves with the
photo directory. Copy `/photos` somewhere else, point `storage.dest` at it, and
rpsync picks up exactly where it left off. If the manifest is ever lost, rpsync
will re-download — but files with identical content are recognised at their
destination and not duplicated.

## Troubleshooting

**The camera is never found.** Confirm CCAPI is activated (the one-time USB step
with Canon's tool) and that the camera is awake — auto power-off ends the Wi-Fi
session. Then check from the daemon's host:

```bash
rpsync discover
curl http://<camera-ip>:8080/ccapi
```

If `curl` works but `discover` does not, multicast is being dropped: set
`camera.host`.

**It finds the camera but imports nothing.** Check `import.include` — the
default is `[jpg, cr3]`, so movies are skipped. `rpsync config` prints what the
daemon is actually using.

**Permission denied writing to /photos.** The container's `user:` does not match
the share's owner. Re-check `id` on the NAS and fix the compose file.

**Imports stop when several things talk to the camera at once.** The body
accepts only a couple of simultaneous HTTP connections. rpsync serialises its
own requests and never leaves an event poll open during a download, but Canon's
own apps connected at the same time will contend with it.

**The web UI is empty and asks for a code.** That is the login screen. Run
`rpsync pair` on the host, or read the code from the startup log.
