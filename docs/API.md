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

Codes the agent handles specially:

- `invalid_token` (401) — user token is wrong or revoked; the agent exits
  with an actionable error instead of retrying.
- `invalid_agent` (401) — agent secret revoked (e.g. key regenerated from the
  dashboard); the agent discards its persisted state and requires
  re-registration.
- Any 5xx or network error — retried with exponential backoff.

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
  "jellyfin": {
    "detected": true,
    "version": "10.10.3",
    "server_name": "Living Room",
    "server_id": "f3a1…"
  }
}
```

`jellyfin` is best-effort detection at startup and may be `null`.

Response `200`:

```json
{
  "agent_id": "agt_01hxyz…",
  "agent_secret": "as_…",
  "subdomain": "brave-otter.tunnelo.io",
  "wireguard": {
    "address": "10.66.0.7/32",
    "mtu": 1280,
    "peer": {
      "public_key": "base64 gateway public key",
      "endpoint": "gw1.tunnelo.io:51820",
      "allowed_ips": ["10.66.0.1/32"],
      "persistent_keepalive_seconds": 25
    }
  },
  "service_port": 8096,
  "heartbeat_interval_seconds": 60,
  "speedtest": {
    "upload_url": "https://gw1.tunnelo.io/v1/speedtest/sink",
    "size_bytes": 33554432,
    "max_seconds": 15
  }
}
```

- `wireguard.address` is the tunnel IP the gateway routes the user's
  subdomain to, on `service_port`.
- `allowed_ips` is deliberately narrow (the gateway's tunnel IP only): the
  tunnel carries proxy traffic, nothing else.
- `heartbeat_interval_seconds` sets the agent's reporting cadence.

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
  "jellyfin": {
    "reachable": true,
    "version": "10.10.3"
  }
}
```

`tunnel.up` means a WireGuard handshake completed within the last 3 minutes.
The gateway uses `jellyfin.reachable` to serve a friendly "server offline"
page instead of a proxy timeout.

Response `200`:

```json
{ "heartbeat_interval_seconds": 60 }
```

## Upload speed test

Run once after a fresh registration (and on demand via `--speedtest`). Two
steps:

### 1. `POST <speedtest.upload_url>`

Auth: **agent secret**. Body: `application/octet-stream`, random bytes,
streamed until `size_bytes` are sent or `max_seconds` elapse. The gateway
discards the body. Response `200` with empty body. The agent measures
throughput client-side.

### 2. `POST /v1/agents/{agent_id}/speedtest`

Auth: **agent secret**.

Request:

```json
{
  "upload_mbps": 23.4,
  "bytes_sent": 33554432,
  "duration_ms": 11467,
  "measured_at": "2026-07-02T12:00:00Z"
}
```

Response `204`.

The result is surfaced in the dashboard so users understand their ISP's
upload bandwidth — not the gateway — is the streaming ceiling.
