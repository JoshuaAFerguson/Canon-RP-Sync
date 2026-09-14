# iPad companion app (planned)

Not started. Intended stack: Swift/SwiftUI, joining the camera hotspot via
`NEHotspotConfiguration`.

The daemon side it talks to already exists — see [`../../docs/API.md`](../../docs/API.md).
The shape of the app is:

1. **Pair once.** Ask the user for the daemon's URL and an 8-character pairing
   code, `POST /api/v1/pair`, and keep the returned token in the keychain.
2. **In the field.** Join the camera's hotspot, talk CCAPI directly (the same
   endpoints `internal/ccapi` uses), and pull shots to the device, previewing
   CR3s with their embedded JPEG rather than decoding RAW.
3. **Back home, or over Tailscale/Cloudflare.** Hand everything over with
   `POST /api/v1/ingest`, passing `card` and `dir` so the daemon knows not to
   download the same frames from the card later. Re-uploading is safe: the
   daemon deduplicates by content hash and answers `"imported": false`.
4. **Browse.** `GET /api/v1/photos?since=` for incremental listings,
   `/thumb` for previews, `/file` for the full image.

Whether the sync core is bound from Go via `gomobile` or reimplemented natively
is still open; the API is stable either way.
