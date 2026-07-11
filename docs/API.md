# Gateway API contract (agent side), v1

This document describes the gateway HTTP API **as implemented by this agent**.
It is the client-side mirror of `API.md` in the `gateway` repository, which is
the source of truth. If the two ever disagree, the gateway wins and this file
(and `internal/register`, `internal/heartbeat`, `internal/speedtest`) must be
updated to match.

All endpoints are served by the gateway over HTTPS. All request and response
bodies are JSON (`Content-Type: application/json`) unless noted otherwise.

## Authentication

Two credentials exist:

| Credential | Obtained | Used for |
|---|---|---|
| **User token** | Issued to the user at signup (dashboard) | `POST /v1/agents/register` only |
| **Agent secret** | Returned by registration | Every other endpoint |

Both are sent as `Authorization: Bearer <value>`.

## Errors

Non-2xx responses carry:

```json
{ "error": { "code": "invalid_token", "message": "human-readable detail" } }
```

| HTTP | `code` | Meaning |
|------|--------|---------|
| 400 | `bad_request` | malformed body / missing or invalid field |
| 401 | `invalid_token` | user token unknown or revoked (registration) |
| 401 | `invalid_agent` | agent credentials revoked |
| 413 | `too_large` | speed-test upload exceeded the advertised size |
| 503 | `not_ready` | gateway cannot verify agent state right now; retry with backoff |
| 500 | `internal` | gateway-side failure; safe to retry with backoff |

How the agent reacts:

- `invalid_token` — the agent exits with an actionable error instead of
  retrying.
- `invalid_agent` — the agent discards its persisted state and requires
  re-registration. The gateway sends this **only** for truly revoked
  credentials (key regenerated, seat re-registered), never for transient
  conditions — those are `503 not_ready`.
- Any other 4xx (e.g. `bad_request`) — the request itself is wrong;
  registration fails fast instead of retrying. `429` is the exception and
  is treated as retryable.
- 5xx and network errors — retried with exponential backoff (registration)
  or on the next tick (heartbeat).

## `POST /v1/agents/register`

Exchanges a user token for a WireGuard peer configuration. Idempotent per
token: re-registering with the same token replaces the previous peer for that
server seat.

Auth: **user token**.

Request:

```json
{
  "public_key": "base64 WireGuard public key (generated locally; the private key never leaves the host)",
  "hostname": "media-box",
  "agent_version": "v0.1.0",
  "tunnel_mode": "managed",
  "service": {
    "type": "jellyfin",
    "detected": true,
    "version": "10.10.3",
    "name": "Living Room"
  },
  "jellyfin": {
    "detected": true,
    "version": "10.10.3",
    "server_name": "Living Room",
    "server_id": "f3a1…"
  }
}
```

`service` describes whatever HTTP service the tunnel exposes: `type` is
`"jellyfin"` when the Jellyfin probe succeeded, the operator-declared
`TUNNELO_SERVICE_TYPE`, or `"http"` for anything else that answered.
`jellyfin` is the legacy mirror of the same detection (fully populated only
when the service really is Jellyfin) — kept so old gateways keep working;
both blocks are best-effort and may be `null`.

`tunnel_mode` is `"managed"` (the agent runs WireGuard itself, default) or
`"external"` (the user carries the peer on WireGuard they already run; the
agent renders a wg-quick config and only reports service health). The
gateway can use this to tailor dashboard setup hints.

Response `200`:

```json
{
  "agent_id": "agt_01hxyz…",
  "agent_secret": "as_…",
  "subdomain": "quiet-falcon",
  "wireguard": {
    "address": "10.77.0.42/32",
    "mtu": 1280,
    "peer": {
      "public_key": "base64 gateway public key",
      "endpoint": "wg.<ourdomain>:51820",
      "allowed_ips": ["10.77.0.1/32"],
      "persistent_keepalive_seconds": 25
    }
  },
  "service_port": 8096,
  "heartbeat_interval_seconds": 30,
  "speedtest": {
    "upload_url": "https://api.<ourdomain>/v1/agents/agt_01hxyz…/speedtest/sink",
    "size_bytes": 50000000,
    "max_seconds": 20
  }
}
```

- `subdomain` is the bare label (the dashboard/gateway own the full
  hostname); the agent treats it as opaque.
- `wireguard.address` is the tunnel IP the gateway routes the user's
  subdomain to, on `service_port` — the **only** port the gateway routes.
- `allowed_ips` is deliberately narrow (the gateway's tunnel IP only): the
  tunnel carries proxy traffic, nothing else.
- `heartbeat_interval_seconds` sets the agent's reporting cadence.
- Re-registering with the same user token replaces the previous peer for
  the seat and **revokes the old `agent_secret`** — one seat, one active
  peer.

## `POST /v1/agents/{agent_id}/heartbeat`

Periodic status report; interval comes from the registration response (and
may be adjusted by each heartbeat response).

Auth: **agent secret**.

Request:

```json
{
  "agent_version": "v0.1.0",
  "tunnel": {
    "up": true,
    "last_handshake_unix": 1751414400,
    "rx_bytes": 1048576,
    "tx_bytes": 4194304
  },
  "service": {
    "reachable": true,
    "type": "jellyfin",
    "version": "10.10.3"
  },
  "jellyfin": {
    "reachable": true,
    "version": "10.10.3"
  }
}
```

`tunnel.up` means a WireGuard handshake completed within the last 3 minutes.
The gateway prefers `service.reachable` (falling back to the legacy
`jellyfin.reachable` mirror from older agents) to serve a friendly "server
offline" page instead of a proxy timeout. Reachability counts ANY HTTP
answer on the health path — a 401 from a service with auth is up.

`tunnel` is **omitted** in external tunnel mode: the gateway terminates the
tunnel, so its own peer table (last handshake, byte counters) is the
authoritative status source there. Agent-side tunnel status is advisory
even in managed mode.

Response `200`:

```json
{ "heartbeat_interval_seconds": 30 }
```

## Upload speed test

Run once after a fresh registration (and on demand via `--speedtest`). Two
steps:

### 1. `POST <speedtest.upload_url>`

The per-agent sink advertised at registration
(`/v1/agents/{agent_id}/speedtest/sink`). Auth: **agent secret**. Body:
`application/octet-stream`, random bytes, streamed until `size_bytes` are
sent or `max_seconds` elapse. The gateway discards the body and enforces a
max size (`size_bytes` + 1 MiB slack → `413 too_large` beyond that).

Response `200`:

```json
{ "received_bytes": 50000000 }
```

The agent measures throughput client-side and ignores the response body.

### 2. `POST /v1/agents/{agent_id}/speedtest`

Auth: **agent secret**.

Request (`measured_at` is RFC 3339 UTC):

```json
{
  "upload_mbps": 23.4,
  "bytes_sent": 50000000,
  "duration_ms": 17100,
  "measured_at": "2026-07-02T10:15:00Z"
}
```

Response `204`.

The result is surfaced in the dashboard so users understand their ISP's
upload bandwidth — not the gateway — is the streaming ceiling.
