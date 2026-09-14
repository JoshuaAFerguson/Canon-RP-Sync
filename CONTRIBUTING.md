# Contributing

Issues and PRs welcome. Please:

- Run `make lint test` before opening a PR.
- Keep `internal/` free of third-party dependencies where practical (it is
  intended to be bound into the mobile apps).
- Never add code paths that modify the camera card without an explicit opt-in flag.
