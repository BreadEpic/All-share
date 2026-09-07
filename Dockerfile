# ALL SHARE rendezvous server.
#
# This is the small "introduce my two devices to each other" service. It never
# sees your screen, keystrokes or clipboard — those go directly between your
# devices — so it needs almost nothing: no database, no disk worth keeping, and
# a few kilobytes of traffic per connection. That is what lets it run on a free
# hosting tier.
#
# Build:  docker build -t allshare-server .
# Run:    docker run -p 8443:8443 allshare-server
#
# On a hosting platform, no arguments are needed: the platform sets $PORT and
# the server binds it. TLS is terminated by the platform, which is why the
# image speaks plain HTTP.

# --- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
# CGO off gives a single static binary with no shared libraries, which is what
# makes the final image able to be built FROM scratch.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.Version=${VERSION}" \
        -o /out/allshare-server ./server/cmd/allshare-server

# --- run --------------------------------------------------------------------
# scratch, not alpine: the server opens no shell, runs no subprocess and reads
# no system files, so an image containing nothing but the binary and CA roots
# removes the entire question of what else is in there.
FROM scratch

# CA roots, so the server can verify TLS if it is ever pointed at an external
# relay. Copied from the build stage rather than installed.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/allshare-server /allshare-server

# An unprivileged, non-existent-by-design uid. Nothing in the image is owned by
# it, which is the point: the process can read its own binary and write only to
# the data directory a platform mounts for it.
USER 65532:65532

# The data directory holds the server's own key and its device list. Both are
# rebuilt automatically if lost — the agent re-sends its registration on every
# reconnect — so a platform with disposable storage is fine here.
ENV ALLSHARE_DATA=/data
VOLUME ["/data"]

EXPOSE 8443
ENTRYPOINT ["/allshare-server"]
