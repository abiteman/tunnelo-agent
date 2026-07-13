# Multi-service support — agent plan (Phase B, agent slice)

Lets one agent expose several local services (Jellyfin **and** Radarr **and**
Sonarr…) through one tunnel. This is the **agent** slice; the gateway and
dashboard slices live in `tunnelo-gateway/docs/multi-service-plan.md` and
`tunnelo-dashboard/docs/multi-service-plan.md`. Ships in lockstep with the
gateway (Phase B).

## Why this is small

The two things that would be hard are already self-contained, per-URL structs:
`internal/tunnel/forward.go` (`Forwarder`) and `internal/detect/detect.go`
(`Prober`). Multi-service is "instantiate N of them," not a rewrite. **One
WireGuard interface / one tunnel IP** stays; the forwarders just bind different
ports on it.

## Locked decisions (agent-relevant)

- **Env is `host:port` list with shorthand.** `TUNNELO_SERVICES` accepts
  `IP:PORT,IP:PORT,IP:PORT` or the shorthand `IP:PORT,PORT,PORT` where the first
  token's host is inherited by bare ports. Scheme defaults to `http`, health
  path `/`. **No service names in the env** — the agent detects the type per
  port; naming/subdomain assignment happens in the dashboard from what the agent
  reports ("detect-then-name").
- **Tunnel-side port = local port.** The forwarder binds `tunnelIP:<localPort>`
  and dials `<host>:<localPort>` — the gateway records the reported port as the
  route target, so no port is allocated anywhere.
- **No back-compat.** Agent and gateway upgrade together; drop the legacy
  singular `service`/`jellyfin` blocks from the request/heartbeat bodies.

## Changes

### `internal/config/config.go`
- Add `Services []ServiceSpec{Host, Port, HealthPath, Type}` parsed from
  `TUNNELO_SERVICES` (the `IP:PORT[,PORT…]` grammar above; validate host:port,
  inherit the leading host for bare ports).
- Keep `TUNNELO_SERVICE_URL` / `TUNNELO_JELLYFIN_URL` as sugar for a one-entry
  list, so single-service installs are unchanged.
- `ServiceHostPort()` becomes per-spec.

### `internal/detect/detect.go`
- No change to `Prober` — instantiate **one per service**. Type is per port:
  Jellyfin probe succeeds → `jellyfin` (+ version/name), else any HTTP answer →
  `http`; operator-declared type wins when set.

### `internal/tunnel/forward.go`
- No change to `Forwarder`. Run **one per service**: `ListenAddr =
  tunnelIP:localPort`, `TargetAddr = host:localPort`. The `EADDRINUSE`
  step-aside logic is already per-forwarder and port-aware — keep it.

### `internal/register/register.go`
- `Request` sends `services:[{name,host,port,type,detected,version}]`
  (name empty — the gateway/dashboard assigns it). Drop singular `service`/
  `jellyfin`.
- `Response` parses `services:[{name,subdomain,service_port}]`; `service_port`
  is the local port echoed back, telling the agent which forwarder maps to which
  subdomain.

### `internal/register/state.go`
- `State` gains `Services []ServiceState{Name, Subdomain, Port}` (replacing the
  singular `Subdomain`/`ServicePort`).

### `internal/heartbeat/heartbeat.go`
- `Report` sends a per-service health array
  `services:[{name,reachable,type,version}]`, one probe per service each beat.
  Drop the singular blocks.

### `cmd/agent/main.go`
- `run()` loops `state.Services`: one `Prober` and (managed mode) one
  `Forwarder` per service, all in the errgroup. External mode still writes one
  wg-quick config (one tunnel IP); the onboarding note lists all service ports.

## The detect-then-name flow

1. User installs with `TUNNELO_SERVICES=192.168.1.5:8096,7878,8989`.
2. Agent probes each port → detects `jellyfin` / `http` → registers, reporting
   the discovered list.
3. Gateway auto-assigns a subdomain per service (primary = account subdomain,
   extras `<primary>-<type>`/`<primary>-<port>`) and routes them live.
4. Dashboard auto-populates the detected services for naming/rename.

## Testing

- Parse `IP:PORT,PORT,PORT` (host inheritance) and full `IP:PORT,IP:PORT`.
- Single `TUNNELO_SERVICE_URL` still yields a one-entry list.
- One forwarder per service binds the right `tunnelIP:port`; `EADDRINUSE`
  step-aside still fires per service when a local port is already served.
- Heartbeat carries one health entry per service.
