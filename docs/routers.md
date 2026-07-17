# Running Tunnelo on a router (OpenWrt / pfSense)

This guide covers exposing a service (Jellyfin by default) through Tunnelo when
you want your **router** involved — either running the agent directly on it, or
having the router carry the WireGuard tunnel while the agent runs elsewhere.

If you don't specifically need the router, the simplest setup is still the
[normal Docker or bare-metal install](../README.md) on the machine that runs
your service. Read on only if you want the router in the picture.

## How the pieces fit

Tunnelo has two independent jobs. Understanding the split is the whole game:

1. **The tunnel** — a WireGuard peer from your network to the gateway. Routers
   are excellent at this.
2. **The agent** — registers your token, then heartbeats: reports whether the
   service is reachable and runs the onboarding upload speed test. This is a
   small Go process that has to run on *some* always-on Linux box, but **not
   necessarily the router** (it probes the service over the LAN).

There is also a hop that's easy to miss. The gateway routes your subdomain to
**your tunnel IP on the service port** (e.g. `10.77.0.42:8096`). Whatever holds
that tunnel IP must make traffic arriving there reach your actual service:

```
viewer ──HTTPS──▶ gateway ──WireGuard──▶ your tunnel IP :8096 ──▶ Jellyfin :8096
                                          ▲                        ▲
                                   (whoever runs WG)        (must be forwarded here)
```

- In **managed mode**, the agent runs *both* the WireGuard interface **and** a
  built-in forwarder that listens on `tunnelIP:8096` and dials your service — so
  the hop is handled for you, but only where the agent runs.
- In **external mode**, the agent only writes a `wg-quick` config and
  heartbeats; it does **not** run the forwarder. So whatever terminates the
  tunnel (your router) must forward `tunnelIP:8096 → your service` itself, with
  a NAT/redirect rule.

The generated config's `[Peer] AllowedIPs` is just the gateway's tunnel IP
(`10.77.0.1/32`), so adding this tunnel to a router **does not** hijack its
default route — only gateway-bound traffic enters the tunnel.

## Which setup should I use?

| You have… | Do this |
|---|---|
| An always-on Linux box (NAS, Pi, the Jellyfin host) and just want it working | Normal install on that box (managed mode). The router isn't involved. |
| An **OpenWrt** router (arm64 or x86_64) you want to run everything on | [Agent on OpenWrt, managed mode](#openwrt-agent-on-the-router-managed-mode) |
| An **OpenWrt** router and you'd rather use its native WireGuard | [OpenWrt native WireGuard + external mode](#openwrt-native-wireguard-external-mode) |
| A **pfSense** router | [pfSense](#pfsense) — external mode only |

## OpenWrt: agent on the router (managed mode)

The agent runs entirely on the router and handles the forwarding hop itself.

**Requirements & caveats**

- **Architecture:** release binaries are published for `linux/amd64` and
  `linux/arm64` only. That covers x86_64 mini-router boxes and arm64 routers,
  but **not** the mips/mipsel/armv7 SoCs in many consumer routers. Check with
  `uname -m` (`x86_64` or `aarch64` → supported). If you're on another arch, use
  the [external-mode path](#openwrt-native-wireguard-external-mode) instead and
  run the agent on a different box.
- **Kernel WireGuard:** install `kmod-wireguard` (`opkg update && opkg install
  kmod-wireguard`). Without it the agent falls back to userspace wireguard-go,
  which also needs `kmod-tun` and noticeably more RAM/CPU — fine on x86, tight
  on small routers.
- **Flash/RAM:** ~3 MB download, ~10 MB on disk once unpacked. Comfortable on
  x86/extroot devices, tight on 16 MB-flash routers.
- OpenWrt uses **procd**, not systemd, so the `install.sh` one-liner does not
  apply — install manually as below.

**1. Install the binary**

```sh
# On the router (as root). Pick the arch that matches `uname -m`.
ARCH=amd64   # or arm64
cd /tmp
wget -O tunnelo.tgz "https://github.com/abiteman/tunnelo-agent/releases/latest/download/tunnelo-agent_linux_${ARCH}.tar.gz"
tar -xzf tunnelo.tgz tunnelo-agent
install -m 0755 tunnelo-agent /usr/bin/tunnelo-agent
```

**2. Write the config**

```sh
mkdir -p /etc/tunnelo-agent
cat > /etc/tunnelo-agent/agent.env <<'EOF'
TUNNELO_TOKEN=<your token from the dashboard>
TUNNELO_GATEWAY_URL=https://api.<your-domain>
# Point the forwarder at your actual service (not 127.0.0.1 unless Jellyfin
# runs on the router itself):
TUNNELO_SERVICE_URL=http://<jellyfin-lan-ip>:8096
EOF
chmod 600 /etc/tunnelo-agent/agent.env
```

**3. procd init script**

```sh
cat > /etc/init.d/tunnelo-agent <<'EOF'
#!/bin/sh /etc/rc.common
USE_PROCD=1
START=95
STOP=10

start_service() {
    procd_open_instance
    procd_set_param command /usr/bin/tunnelo-agent
    procd_set_param env $(cat /etc/tunnelo-agent/agent.env | grep -v '^#' | xargs)
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}
EOF
chmod +x /etc/init.d/tunnelo-agent
/etc/init.d/tunnelo-agent enable
/etc/init.d/tunnelo-agent start
logread -f -e tunnelo-agent   # watch it register + connect
```

**4. Allow the tunnel port through the firewall**

The agent's forwarder listens on the tunnel IP (on its own `tunnelo0`
interface). OpenWrt's firewall drops input from interfaces not in a zone, so
add a traffic rule permitting the service port on the tunnel interface:

```sh
uci add firewall rule
uci set firewall.@rule[-1].name='Allow-Tunnelo'
uci set firewall.@rule[-1].src='*'
uci set firewall.@rule[-1].proto='tcp'
uci set firewall.@rule[-1].dest_port='8096'
uci set firewall.@rule[-1].target='ACCEPT'
uci commit firewall
/etc/init.d/firewall restart
```

That's it — no manual NAT, because the agent's forwarder makes the
`tunnelIP → Jellyfin` hop for you.

**Uninstalling.** This is a procd (not systemd) setup, so the installer's
`--uninstall` flag doesn't apply — reverse it by hand:

```sh
/etc/init.d/tunnelo-agent disable
rm /etc/init.d/tunnelo-agent /usr/bin/tunnelo-agent
rm -rf /etc/tunnelo-agent            # optional: removes token + credentials
# and delete the 'Allow-Tunnelo' firewall rule you added above
```

## OpenWrt: native WireGuard + external mode

Let OpenWrt's own WireGuard carry the tunnel (works on any arch, including
mips/armv7 since no agent binary runs on the router). The agent runs on any
LAN Linux box just to register and heartbeat, and the router forwards the hop.

**1. Get the peer config.** On any always-on Linux box (the Jellyfin host is
ideal), run the agent once in external mode:

```sh
docker run --rm -v tunnelo-agent:/var/lib/tunnelo-agent \
  -e TUNNELO_TOKEN=<your token> \
  -e TUNNELO_TUNNEL_MODE=external \
  -e TUNNELO_GATEWAY_URL=https://api.<your-domain> \
  -e TUNNELO_SERVICE_URL=http://<jellyfin-lan-ip>:8096 \
  ghcr.io/abiteman/tunnelo-agent:latest
```

It prints the path to a `wg-quick` config (`tunnelo-wg.conf`) and keeps
heartbeating. Leave it running (or as a persistent `docker run -d …`) so the
dashboard shows live status — the tunnel itself will be on the router.

The config looks like:

```ini
[Interface]
PrivateKey = <private key>
Address = 10.77.0.42/32
MTU = 1280

[Peer]
PublicKey = <gateway key>
Endpoint = wg.<your-domain>:51820
AllowedIPs = 10.77.0.1/32
PersistentKeepalive = 25
```

**2. Add it to OpenWrt.** In LuCI: *Network → Interfaces → Add new interface*,
protocol **WireGuard VPN**, and fill in the `[Interface]` private key, the
`Address`, then add the `[Peer]`. Or via `/etc/config/network`:

```
config interface 'tunnelo'
	option proto 'wireguard'
	option private_key '<private key>'
	list addresses '10.77.0.42/32'

config wireguard_tunnelo
	option public_key '<gateway key>'
	option endpoint_host 'wg.<your-domain>'
	option endpoint_port '51820'
	option persistent_keepalive '25'
	list allowed_ips '10.77.0.1/32'
```

`ifup tunnelo` to bring it up; `wg show` should show a recent handshake.

**3. Forward the hop.** The router now holds `10.77.0.42`. Redirect traffic
arriving on the tunnel to Jellyfin:

```sh
uci add firewall redirect
uci set firewall.@redirect[-1].name='Tunnelo-Jellyfin'
uci set firewall.@redirect[-1].src='*'
uci set firewall.@redirect[-1].proto='tcp'
uci set firewall.@redirect[-1].src_dport='8096'
uci set firewall.@redirect[-1].dest_ip='<jellyfin-lan-ip>'
uci set firewall.@redirect[-1].dest_port='8096'
uci set firewall.@redirect[-1].target='DNAT'
uci commit firewall
/etc/init.d/firewall restart
```

(Jellyfin must accept the router as a source, and its LAN gateway must be this
router so return traffic comes back — usually already the case.)

## pfSense

pfSense is **FreeBSD**, and the agent's tunnel management is Linux-only, so you
**cannot run the agent on pfSense itself** — and there is no FreeBSD binary. Use
pfSense's built-in WireGuard for the tunnel and run the agent on a LAN Linux
box for health reporting.

**1. Get the peer config** exactly as in [OpenWrt step 1](#openwrt-native-wireguard-external-mode)
— run the agent in external mode on any LAN Linux box (the Jellyfin host works
well) and leave it running for dashboard status.

**2. Create the tunnel in pfSense.** *VPN → WireGuard → Tunnels → Add*:

- **Interface Keys:** paste the `[Interface] PrivateKey`. (pfSense derives the
  public key; that's fine — the gateway only needs the private key's side, which
  this config already carries.)
- **Address:** the `[Interface] Address` (`10.77.0.42/32`).
- Add a **Peer**: `PublicKey`, `Endpoint` host + port (`wg.<your-domain>:51820`),
  `AllowedIPs` = `10.77.0.1/32`, keepalive `25`.

Assign the tunnel under *Interfaces → Assignments*, enable it, and check
*Status → WireGuard* for a handshake.

**3. Forward the hop.** *Firewall → NAT → Port Forward*, on the WireGuard
interface: destination = the tunnel address, port `8096`, redirect target =
your Jellyfin LAN IP port `8096`. Add the matching pass rule on the WireGuard
interface for that traffic.

## Limitations & notes

- **Published arches** are `linux/amd64` and `linux/arm64`. mips/mipsel/armv7
  routers can still use the external-mode paths (tunnel on the router, agent on
  another box); they just can't run the agent binary on the router. FreeBSD
  (pfSense) can't run the agent at all yet.
- **The agent must keep running somewhere** for the dashboard to show live
  status and to service dashboard-triggered speed-test re-runs. In external
  setups the tunnel stays up even if the agent stops — you just lose live health
  reporting until it's back.
- **The forwarding hop is required** whenever the tunnel terminates somewhere
  other than the service host. Managed mode handles it automatically; external
  mode needs the NAT/redirect rule shown above.
- **Multiple services:** set `TUNNELO_SERVICES=<ip>:8096,7878,8989` instead of
  `TUNNELO_SERVICE_URL`. In external setups, add one forward rule per service
  port on the router.
