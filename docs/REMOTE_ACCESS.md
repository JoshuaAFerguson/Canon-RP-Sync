# Remote access

To hand photos over from an iPad while away from home, the daemon has to be
reachable. rpsync supports two ways of doing that without forwarding a port on
your router — which is worth avoiding, since it puts your NAS on the public
internet.

| | Tailscale | Cloudflare Tunnel |
|---|---|---|
| Who can reach it | Only your devices | Anyone with the URL, unless you add Cloudflare Access |
| Needs an account | Tailscale | Cloudflare (free tier is enough) |
| Needs a domain | No | Yes, for a stable hostname |
| Client setup | Tailscale app on each device | Nothing |
| Best for | Personal use across your own devices | Sharing, or devices you cannot install a VPN on |

Either way, rpsync's own bearer tokens still apply: the tunnel carries the
traffic, pairing decides who may use it.

## Tailscale

rpsync does **not** run `tailscaled` itself. It reads the node's address from a
Tailscale daemon that is already running — the host's, or a sidecar container
sharing the network namespace — and reports the resulting URL in `/status` and
in the pairing instructions.

### On a host that already has Tailscale

```yaml
remote:
  mode: tailscale
```

That is the whole configuration. `rpsync` runs `tailscale status --json`, takes
the MagicDNS name, and advertises `http://<node>.<tailnet>.ts.net:8787`.

If the CLI is not on `PATH` (common inside containers), either point at it:

```yaml
remote:
  mode: tailscale
  tailscale:
    cli: /usr/local/bin/tailscale
```

or skip detection and state the name yourself:

```yaml
remote:
  mode: tailscale
  tailscale:
    hostname: nas.tail1234.ts.net
```

### With the Docker sidecar

`deploy/docker-compose.yml` ships a `tailscale` profile that joins the tailnet
and shares the host network with rpsync:

```bash
echo "TS_AUTHKEY=tskey-auth-…" >> .env
docker compose --profile tailscale up -d
```

Then set `RPSYNC_REMOTE_MODE=tailscale` and
`RPSYNC_TAILSCALE_HOSTNAME=rpsync.tail1234.ts.net` on the rpsync service.

### On the phone or tablet

Install the Tailscale app, sign in, and pair against the tailnet URL. It works
identically at home and away, which means the companion app needs only one
address configured.

## Cloudflare Tunnel

A tunnel gives the daemon a public HTTPS hostname with no inbound firewall rule.
rpsync can supervise the `cloudflared` connector for you, or leave it to a
sidecar.

### Named tunnel (stable hostname)

1. In the Cloudflare dashboard: **Zero Trust → Networks → Tunnels → Create**.
2. Add a public hostname, e.g. `rp.example.com`, routed to
   `http://localhost:8787`.
3. Copy the tunnel token.

```yaml
remote:
  mode: cloudflare
  cloudflare:
    token_file: /config/cloudflared-token   # or RPSYNC_CLOUDFLARE_TOKEN
    hostname: rp.example.com
    managed: true
server:
  advertise_url: https://rp.example.com
```

rpsync starts `cloudflared tunnel run` as a child process, restarts it with
backoff if it exits, and reports its state in `/status`. Put the token in a file
or an environment variable rather than in `rpsync.yaml`, which is
world-readable by default.

**Anyone who learns the hostname can reach the login page.** rpsync's pairing
endpoint is rate-limited and burns its code after five wrong guesses, but for a
publicly routable deployment add a
[Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/)
policy in front of it as well.

### Quick tunnel (no account, temporary)

Leave the token empty and rpsync starts a quick tunnel, printing the
`*.trycloudflare.com` URL it was given:

```yaml
remote:
  mode: cloudflare
  cloudflare:
    managed: true
```

The URL changes every restart, so this is for trying things out, not for a
device you want to keep paired.

### Sidecar instead of a child process

If a container or system service already runs the connector, tell rpsync not to:

```yaml
remote:
  mode: cloudflare
  cloudflare:
    managed: false
    hostname: rp.example.com
```

`deploy/docker-compose.yml` has a `cloudflare` profile that does exactly this:

```bash
echo "TUNNEL_TOKEN=…" >> .env
docker compose --profile cloudflare up -d
```

## Behind a reverse proxy

If you terminate TLS yourself (Caddy, nginx, Traefik), set:

```yaml
server:
  advertise_url: https://rp.example.com
```

so pairing hands devices the external URL. Forward `X-Forwarded-Proto: https` as
well — rpsync uses it to mark the session cookie `Secure`. The event stream is
server-sent events, so disable response buffering for `/api/v1/events`
(`proxy_buffering off` in nginx).

## What is actually exposed

| Route | Auth | Exposed to a tunnel? |
|---|---|---|
| `GET /api/v1/health` | none | Yes — it reports only service name, status and version |
| `POST /api/v1/pair` | none | Yes — rate-limited, single-use codes |
| everything else | bearer token | Yes, but useless without a token |

Tokens are stored hashed. Revoking a device (`rpsync devices revoke <id>` or the
web UI) takes effect immediately, including in a daemon that is already running.
