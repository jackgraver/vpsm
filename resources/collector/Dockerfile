# The collector as a container image, used by the e2e harness in ../e2e and
# suitable as a starting point for a real deployment.
#
# Build context is this directory (collector/), not the repo root: the
# collector is independently deployable and its image should not need the
# agent's source to build.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so editing source doesn't re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off is not an optimisation here, it is the point: storage uses the pure-Go
# modernc.org/sqlite driver precisely so the binary links statically with no C
# toolchain, which is what lets the runtime stage be this small.
RUN CGO_ENABLED=0 go build -trimpath -o /out/vpsm-collector .

FROM alpine:3.22

# curl is for the healthcheck below, and for poking the API by hand from
# inside the network during manual testing — which the protocol was chosen to
# make possible, so the image should not take it away.
RUN apk add --no-cache ca-certificates curl

COPY --from=build /out/vpsm-collector /usr/local/bin/vpsm-collector

# The database lives outside the working tree so a bind-mounted source
# directory can never collide with it.
RUN mkdir -p /var/lib/vpsm
ENV VPSM_DB=/var/lib/vpsm/vpsm.db

# Containers get their own loopback, so the collector's deliberate
# bind-to-loopback default would make it unreachable from a sibling container.
# Binding all interfaces is safe here *because* the container network is the
# boundary — publish the port deliberately, as the compose file does.
ENV VPSM_ADDR=0.0.0.0:8777
EXPOSE 8777

# /healthz is unauthenticated by design, so the check needs no token.
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=5 \
    CMD curl -fsS http://127.0.0.1:8777/healthz > /dev/null

ENTRYPOINT ["vpsm-collector"]
CMD ["serve"]
