# Standalone image build. Release images are built by goreleaser from
# Dockerfile.goreleaser using the same distroless base.
#
# The container needs two things the default Docker profile doesn't grant:
#
#   --cap-add=NET_ADMIN       create/configure the WireGuard interface
#   --device /dev/net/tun     userspace fallback when the host lacks the
#                             kernel WireGuard module
#
# Example:
#   docker run -d --name tunnelo-agent \
#     --cap-add=NET_ADMIN --device /dev/net/tun \
#     -e TUNNELO_TOKEN=<your token> \
#     -e TUNNELO_JELLYFIN_URL=http://<jellyfin-host>:8096 \
#     -v tunnelo-agent:/var/lib/tunnelo-agent \
#     ghcr.io/abiteman/tunnelo-agent:latest

FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /tunnelo-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /tunnelo-agent /tunnelo-agent
# Persisted registration state (agent credentials + WireGuard private key).
VOLUME /var/lib/tunnelo-agent
ENTRYPOINT ["/tunnelo-agent"]
