# Contributing

Issues and PRs welcome. Please:

- Run `make lint test` before opening a PR.
- Keep the sync core (`internal/ccapi`, `discovery`, `exif`, `manifest`, `sync`,
  `thumb`) free of third-party dependencies — it is intended to be bound into
  the mobile apps. Dependencies are acceptable in the layers above it; today
  that means `gopkg.in/yaml.v3` for configuration and `golang.org/x/sys` for the
  Windows service.
- Never add a code path that modifies the camera card without an explicit opt-in
  flag.
- Test against `internal/cameratest` rather than real hardware where you can;
  it fakes CCAPI content listing, downloads, deletion and event polling.
- New API endpoints belong in `docs/API.md` — the companion apps are built from
  that document.
