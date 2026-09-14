# syntax=docker/dockerfile:1

# Build stage. TARGETOS/TARGETARCH come from buildx, so one Dockerfile produces
# both the amd64 image and the arm64 one a Synology DS with an ARM CPU needs.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Dependencies first, so edits to the source do not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rpsync ./cmd/rpsync

# Runtime stage.
FROM alpine:3.20

# ca-certificates is needed to reach Cloudflare when a tunnel is configured;
# tzdata makes capture-date folders match the local day rather than UTC.
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -g 1000 rpsync \
 && adduser -D -u 1000 -G rpsync rpsync \
 && mkdir -p /photos /config \
 && chown -R rpsync:rpsync /photos /config

COPY --from=build /out/rpsync /usr/local/bin/rpsync

# /photos is the import destination; /config holds device tokens and rpsync.yaml.
VOLUME ["/photos", "/config"]
EXPOSE 8787

ENV RPSYNC_CONTAINER=1 \
    RPSYNC_DEST=/photos \
    RPSYNC_STATE_DIR=/config \
    RPSYNC_SERVER_ADDR=:8787

# Runs unprivileged. On a Synology, set user: "PUID:PGID" in compose to match
# the account that owns the photo share.
USER rpsync:rpsync

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8787/api/v1/health || exit 1

ENTRYPOINT ["rpsync"]
CMD ["daemon"]
