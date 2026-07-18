# Tunnelo Agent

Give your Jellyfin server a public address like `https://your-name.tunnelo.io`
— no port forwarding, no dynamic DNS, no reverse proxy, no certificate
wrangling.

The agent runs next to your Jellyfin server, keeps an **outbound** WireGuard
tunnel to the Tunnelo gateway, and relays your Jellyfin port over it. Because
the connection is outbound, it works behind CGNAT, double NAT, and ISPs that
block inbound ports.

## What it does

1. **Registers** your server: you paste a one-time token from your Tunnelo
   dashboard; the agent generates a WireGuard keypair locally and exchanges
   the public key for a tunnel configuration and your subdomain.
2. **Maintains the tunnel**: uses the kernel WireGuard module when available
   and falls back to userspace `wireguard-go` when it isn't (no kernel
   module needed — handy for Unraid, older kernels, and restricted VMs).
   Reconnects automatically with backoff if your network drops or the
   gateway's address changes.
3. **Detects Jellyfin**: probes your Jellyfin server's public info endpoint
   (`/System/Info/Public`, no credentials involved) so the dashboard can show
   its version and reachability.
4. **Measures your upload speed** once at setup, against the gateway. Most
   residential connections upload at 10–35 Mbps — that, not Tunnelo, is the
   ceiling for remote streaming quality, and it's better to know up front.
5. **Sends heartbeats** (tunnel state + Jellyfin reachability) so your
   subdomain shows a clear "server offline" page instead of a timeout when
   your box is down.

## What it can access — and what it can't

Worth being explicit, since this thing runs on your media server:

- The tunnel is **restricted to your Jellyfin port**. The gateway routes
  `your-name.tunnelo.io` to exactly one port on the tunnel; the peer's
  `allowed_ips` covers only the gateway itself. It is not a general-purpose
  VPN into your network.
- Traffic to your subdomain is **end-to-end between viewers and your
  Jellyfin** over TLS at the gateway + WireGuard to your box. Your Jellyfin
  credentials and media never touch anything else of ours.
- The agent sends the gateway: your WireGuard **public** key, hostname,
  Jellyfin version/name (from the public, unauthenticated info endpoint),
  tunnel status, byte counters, and one upload-speed measurement. That's the
  complete list — grep for `json:` in `internal/` to audit it.
- The WireGuard **private key never leaves your machine**. It's stored in
  the agent's state directory with owner-only permissions.

This repo is open source (Apache-2.0) precisely so you don't have to take
our word for any of the above.

## Install

You need a token from your [Tunnelo dashboard](https://api.tunnelo.io) first.
It's used once, at registration.

### Docker

```sh
docker run -d --name tunnelo-agent \
  --cap-add=NET_ADMIN --device /dev/net/tun \
  --restart unless-stopped \
  -e TUNNELO_TOKEN=<your token> \
  -e TUNNELO_SERVICE_URL=http://<service-host>:8096 \
  -v tunnelo-agent:/var/lib/tunnelo-agent \
  ghcr.io/abiteman/tunnelo-agent:latest
```

- `NET_ADMIN` + `/dev/net/tun` are required to create the WireGuard
  interface — that's the only elevated access the container asks for. It is
  **not** a privileged container.
- Set `TUNNELO_SERVICE_URL` to wherever the service actually is — your host's
  LAN IP if it runs in another container (`http://192.168.1.50:8096`), or a
  compose service name if they share a network. Jellyfin is the default, but
  any HTTP service works (Navidrome, Audiobookshelf, …); reachability counts
  any HTTP answer, and Jellyfin additionally gets version detection.
- **Multiple services on one box?** Set `TUNNELO_SERVICES` instead — a
  comma-separated list of `IP:PORT`, or the shorthand `IP:PORT,PORT,PORT`
  where bare ports inherit the first host
  (e.g. `192.168.1.50:8096,7878,8989` for Jellyfin + Radarr + Sonarr). The
  agent detects each service's type and the gateway gives each its own
  subdomain (the first entry is your primary address; the rest become
  `<primary>-radarr` etc., renamable from the dashboard). One tunnel, one
  plan throughput ceiling shared across them. Changing the list restarts
  registration automatically without resetting your tunnel. Make sure each
  service has its own authentication enabled — the URLs are public.
- The volume keeps your registration (credentials + private key) across
  container updates.

### Bare metal (systemd)

```sh
curl -fsSL https://raw.githubusercontent.com/abiteman/tunnelo-agent/main/install.sh | sudo TUNNELO_TOKEN=<your token> sh
```

Installs `/usr/local/bin/tunnelo-agent` and a hardened systemd unit
(capability bounding set reduced to `CAP_NET_ADMIN`). Re-run it any time to
upgrade — an existing registration is never overwritten. Prefer to read
before you pipe to shell? Good instinct: [`install.sh`](install.sh).

By default this exposes Jellyfin on this host. To expose several services on
one tunnel, pass `TUNNELO_SERVICES` (a `host:port` comma list; bare ports
reuse the first host) — the installer persists it to the env file:

```sh
curl -fsSL https://raw.githubusercontent.com/abiteman/tunnelo-agent/main/install.sh \
  | sudo TUNNELO_TOKEN=<your token> TUNNELO_SERVICES=127.0.0.1:8096,7878,8989 sh
```

Since the agent runs on the host (not a container), `127.0.0.1` reaches
services on this machine; use a LAN IP for services on another box.

**Uninstall.** Removes the service and binary but keeps your token and agent
credentials, so a reinstall resumes the same registration:

```sh
curl -fsSL https://raw.githubusercontent.com/abiteman/tunnelo-agent/main/install.sh | sudo TUNNELO_UNINSTALL=1 sh
# or, with the script downloaded:  sudo ./install.sh --uninstall
```

Add `--purge` (or `TUNNELO_PURGE=1`) to also delete `/etc/tunnelo-agent` and
`/var/lib/tunnelo-agent`. Uninstall is idempotent — safe to run when nothing
is installed.

### Unraid

Search for **tunnelo-agent** in Community Applications (template in
[`unraid/`](unraid/)), paste your token, and set the Jellyfin URL to your
server's LAN IP, e.g. `http://192.168.1.100:8096`. The template presets the
`NET_ADMIN` capability and `/dev/net/tun` device.

### Already running WireGuard? (advanced)

If you'd rather carry the Tunnelo peer on WireGuard you already operate — a
router, wg-quick on the host, an existing WireGuard container — run the
agent in **external tunnel mode**:

```sh
docker run -d --name tunnelo-agent \
  --restart unless-stopped \
  -e TUNNELO_TOKEN=<your token> \
  -e TUNNELO_TUNNEL_MODE=external \
  -e TUNNELO_SERVICE_URL=http://<service-host>:8096 \
  -v tunnelo-agent:/var/lib/tunnelo-agent \
  ghcr.io/abiteman/tunnelo-agent:latest
```

Note what's missing: **no `NET_ADMIN`, no `/dev/net/tun`** — in this mode
the agent is a completely unprivileged process. It registers, writes a
standard wg-quick config to `/var/lib/tunnelo-agent/tunnelo-wg.conf` (or
`--wg-config-out`), and keeps reporting Jellyfin health. You add that
config to your own WireGuard; the gateway reads tunnel status from its own
end, so the dashboard stays accurate.

What becomes your responsibility:

- Traffic the gateway sends to your tunnel IP on the Jellyfin port must
  reach Jellyfin. If your WireGuard terminates on the Jellyfin host and
  Jellyfin listens on `0.0.0.0`, that's automatic; if it terminates on a
  router or another box, you need the DNAT/route.
- Don't let another peer's `AllowedIPs` (e.g. a commercial VPN's
  `0.0.0.0/0`) swallow the Tunnelo gateway's address.

The managed mode exists because this is exactly the plumbing most people
don't want to own — but if you already own it, external mode stays out of
your way.

Running this on an **OpenWrt or pfSense router** — either the agent directly
on the router or the router carrying the tunnel while the agent runs
elsewhere — is covered step by step in [`docs/routers.md`](docs/routers.md),
including the firewall/NAT rules for the forwarding hop.

## Configuration

Every flag has an environment variable; flags win.

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--token` | `TUNNELO_TOKEN` | — | One-time setup token (only until first registration) |
| `--service-url` | `TUNNELO_SERVICE_URL` | `http://127.0.0.1:8096` | Where to reach the exposed service |
| `--services` | `TUNNELO_SERVICES` | — | Expose several services: `IP:PORT,IP:PORT` or `IP:PORT,PORT,PORT` (bare ports inherit the first host). Overrides `--service-url` |
| `--health-path` | `TUNNELO_HEALTH_PATH` | `/` | Path probed for reachability; any HTTP answer counts as up |
| `--service-type` | `TUNNELO_SERVICE_TYPE` | autodetect | Display name for the dashboard (`jellyfin`, `navidrome`, …) |
| `--gateway-url` | `TUNNELO_GATEWAY_URL` | `https://api.tunnelo.io` | Gateway API |
| `--state-dir` | `TUNNELO_STATE_DIR` | `/var/lib/tunnelo-agent` | Credentials + private key |
| `--interface` | `TUNNELO_INTERFACE` | `tunnelo0` | WireGuard interface name |
| `--mtu` | `TUNNELO_MTU` | gateway-provided (1280) | Tunnel MTU override; lower it if pages/streams stall on a connected tunnel (MTU black hole) |
| `--tunnel-mode` | `TUNNELO_TUNNEL_MODE` | `managed` | `external` = bring your own WireGuard |
| `--wg-config-out` | `TUNNELO_WG_CONFIG_OUT` | `<state-dir>/tunnelo-wg.conf` | Where external mode writes the wg-quick config |
| `--userspace` | `TUNNELO_USERSPACE` | `false` | Force userspace wireguard-go |
| `--speedtest` | — | `false` | Re-run the upload speed test |
| `--log-level` | `TUNNELO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Troubleshooting

- **"kernel WireGuard unavailable, using userspace wireguard-go"** — normal
  on hosts without the kernel module; userspace mode is slightly slower but
  fine for streaming.
- **Remote playback buffers** — check the `upload speed test` line in the
  agent logs. A 4K remux needs more upload bandwidth than most ISPs sell;
  set Jellyfin's transcoding bitrate below your measured upload.
- **"gateway revoked this agent"** — you regenerated the key from the
  dashboard. Restart the agent with a fresh `TUNNELO_TOKEN`.
- **Service shows unreachable** — the agent probes `TUNNELO_SERVICE_URL`
  from *its own* network namespace; from inside Docker, `127.0.0.1` is the
  agent container, not your host. Use the host's LAN IP.
- **Changed the service's port?** Set `TUNNELO_SERVICE_URL` to match (e.g.
  `http://127.0.0.1:9096`). Your public subdomain is unaffected — the agent
  bridges the gateway's port to wherever Jellyfin actually listens.

## Building from source

```sh
go build ./cmd/agent
```

Releases are built by [goreleaser](.goreleaser.yaml) via GitHub Actions:
tagged versions publish static linux amd64/arm64 binaries and a multi-arch
distroless image at `ghcr.io/abiteman/tunnelo-agent`.

The gateway API this agent speaks is documented in
[`docs/API.md`](docs/API.md).

## License

[Apache-2.0](LICENSE)
