# Build image for every nit binary.
#
# One image serves all four. They share most of their code, the whole set is a
# few tens of megabytes, and an operator debugging a worker will want `nitctl`
# on the same host. Splitting them would mean four images to build, tag, scan
# and keep in step for no gain anyone could point at.

# --- build ------------------------------------------------------------------

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off: nit shells out to git and talks to PostgreSQL over the wire, so it
# needs nothing from libc that a static binary cannot do. A static binary is
# what makes the runtime image this small.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/ ./cmd/...

# --- runtime ----------------------------------------------------------------

FROM alpine:3.21

# git is not optional: the worker's entire job is clone, apply, rebase, push.
# ca-certificates is what lets it reach an HTTPS forge at all.
# openssh-client covers SSH remotes, where the credential is a key rather than a
# token.
# curl serves both the bootstrap scripts and container healthchecks.
RUN apk add --no-cache git ca-certificates openssh-client curl tini \
    && adduser -D -u 10001 -h /var/lib/nit nit \
    && mkdir -p /var/lib/nit/blobs /var/lib/nit/work \
    && chown -R nit:nit /var/lib/nit

COPY --from=build /out/nit /out/nitd /out/nit-worker /out/nitctl /usr/local/bin/

# The example bundle, so a container started with no policy at all still has
# something to compile rather than failing on a missing directory.
COPY configs/policy/example /usr/share/nit/policy.example

USER nit
WORKDIR /var/lib/nit

ENV NIT_BLOB_DIR=/var/lib/nit/blobs \
    NIT_WORK_DIR=/var/lib/nit/work \
    NIT_POLICY_DIR=/etc/nit/policy

EXPOSE 8080

# tini reaps the processes git leaves behind. Without an init, a container
# running thousands of clones accumulates zombies until it cannot fork.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["nitd"]
