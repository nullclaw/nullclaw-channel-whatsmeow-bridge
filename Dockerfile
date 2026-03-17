FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /out/nullclaw-channel-whatsmeow-bridge .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV NULLCLAW_WHATSMEOW_BRIDGE_LISTEN=0.0.0.0:3301
ENV NULLCLAW_WHATSMEOW_BRIDGE_STATE_DIR=/var/lib/nullclaw-channel-whatsmeow-bridge

WORKDIR /app
VOLUME ["/var/lib/nullclaw-channel-whatsmeow-bridge"]

COPY --from=build /out/nullclaw-channel-whatsmeow-bridge /usr/local/bin/nullclaw-channel-whatsmeow-bridge

EXPOSE 3301

ENTRYPOINT ["/usr/local/bin/nullclaw-channel-whatsmeow-bridge"]

