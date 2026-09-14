# CCAPI subset used by rpsync

Base URL: `http://<camera-ip>:8080/ccapi`

## Version negotiation

`GET /ccapi` returns the endpoints the body supports, grouped by API version:

```json
{
  "ver100": [{"path": "/ccapi/ver100/deviceinformation", "get": true}, …],
  "ver110": [{"path": "/ccapi/ver110/contents", "get": true}, …]
}
```

rpsync reads this once per connection and uses the newest version that offers
each endpoint, rather than hard-coding `ver100`. If the index cannot be read it
falls back to `ver100` paths, which every CCAPI body supports.

## Endpoints

| Endpoint | Method | Purpose |
|---|---|---|
| `/deviceinformation` | GET | Model, serial, firmware. Doubles as the reachability check |
| `/contents` | GET | List storage devices (`sd`) |
| `/contents/sd` | GET | List directories (`100CANON`, …) |
| `/contents/sd/100CANON?type=all&kind=number` | GET | Page count for a directory |
| `/contents/sd/100CANON?type=all&kind=list&page=N` | GET | File URLs for page N |
| `/contents/sd/100CANON/IMG_0001.JPG` | GET | Download the file |
| `…?kind=thumbnail` | GET | The embedded preview — how CR3s are shown without decoding RAW |
| `…?kind=info` | GET | File size and the camera's timestamp |
| `/contents/sd/100CANON/IMG_0001.JPG` | DELETE | Delete from the card (opt-in only) |
| `/event/polling?continue=on` | GET | Long poll; `addedcontents` lists new files |

File URLs returned by `contents` are absolute and can be fetched directly.

## Activation

CCAPI is off by default. Run Canon's *CCAPI Activation Tool* once against the
camera over USB. The camera then serves the API whenever Wi-Fi is enabled,
regardless of which connection mode is selected.

## Behaviour worth knowing

- **Connections are scarce.** The body accepts only a couple of simultaneous
  HTTP connections. rpsync serialises its own requests and always cancels an
  event poll before downloading, but Canon's own apps connected at the same time
  will contend for the same budget.
- **`event/polling` blocks.** With `continue=on` the camera holds the connection
  open until something happens. A client-side timeout on that request is wrong:
  bound it with a context you can cancel instead.
- **Auto power-off ends everything.** The Wi-Fi session dies with the camera, so
  every request can fail at any moment. Treat a failed request as "reconnect
  later", not as an error worth stopping for.
- **Timestamps vary.** `kind=info` reports `datetime` in RFC1123 form with a
  numeric zone, but not every firmware populates it — which is why rpsync reads
  EXIF from the downloaded file first and treats the camera's answer as a
  fallback.

## Reference

Official documentation: Canon Developer Community → CCAPI (registration
required).
