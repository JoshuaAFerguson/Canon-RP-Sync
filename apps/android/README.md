# Android companion app (planned)

Not started. Intended stack: Kotlin/Compose, joining the camera hotspot via
`WifiNetworkSpecifier`.

The daemon side it talks to already exists — see [`../../docs/API.md`](../../docs/API.md),
and [`../ios/README.md`](../ios/README.md) for the flow, which is identical:
pair once for a bearer token, pull over the camera's hotspot while out, then
hand over with `POST /api/v1/ingest` when back on the home network or over a
tunnel.

Note that on Android a `WifiNetworkSpecifier` connection is bound to the
requesting app and has no internet route, so the handover step has to happen
after leaving the camera's hotspot — queue uploads rather than streaming them
live.
