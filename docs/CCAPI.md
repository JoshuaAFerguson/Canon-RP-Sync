# CCAPI subset used by rpsync

Base URL: `http://<camera-ip>:8080/ccapi/ver100`

| Endpoint | Method | Purpose |
|---|---|---|
| `/deviceinformation` | GET | Model, serial, firmware — used to confirm we found a camera |
| `/contents` | GET | List storage devices (`sd`) |
| `/contents/sd` | GET | List directories (`100CANON`, …) |
| `/contents/sd/100CANON?type=all&kind=number` | GET | Page count for a directory |
| `/contents/sd/100CANON?type=all&kind=list&page=N` | GET | File URLs for page N |
| `/contents/sd/100CANON/IMG_0001.JPG` | GET | Download the file (`?kind=thumbnail` for a thumbnail) |
| `/contents/sd/100CANON/IMG_0001.JPG` | DELETE | Delete from card (opt-in only) |
| `/event/polling?continue=on` | GET | Long-poll; `addedcontents` lists new files |

## Activation

CCAPI is off by default. Run Canon's *CCAPI Activation Tool* once against the
camera over USB. The camera then serves the API whenever Wi-Fi is enabled,
regardless of which connection mode is selected.

## Notes

- The camera only allows a small number of concurrent HTTP connections; keep
  downloads sequential.
- File URLs returned by `contents` are absolute and can be fetched directly.
- Official reference: Canon Developer Community → CCAPI (registration required).
