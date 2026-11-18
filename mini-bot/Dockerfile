# The agent as a container image, used by the e2e harness in ../resources/e2e.
#
# Build context is this directory (mini-bot/). Only the manifest and src are
# copied, which keeps the 1 GB local `target/` out of the build context.
#
# Read ../resources/e2e/README.md before trusting the numbers this produces: a container
# sees the host's /proc/stat and /proc/meminfo but only its own PID namespace,
# so host gauges are the host's and the process list is the container's.

FROM rust:1-alpine AS build

# musl-dev provides the libc the statvfs(3) FFI links against — the one
# syscall with no /proc file behind it, and the agent's only dependency.
RUN apk add --no-cache musl-dev

WORKDIR /src

COPY Cargo.toml Cargo.lock ./
COPY src ./src

# The release profile is the one worth testing: it is what gets deployed, and
# the binary-size and RSS figures in CLAUDE.md are measured against it.
RUN cargo build --release --locked

# Just the static binary, for deploying it outside a container:
#   docker build --target artifact --output type=local,dest=dist .
# leaves dist/mini-bot (this is what .cicd.yaml's build step runs).
FROM scratch AS artifact
COPY --from=build /src/target/release/mini-bot /mini-bot

FROM alpine:3.22

COPY --from=build /src/target/release/mini-bot /usr/local/bin/mini-bot

# The agent runs as a long-lived foreground process; the sample loop is its
# own scheduler, so there is nothing for an init system to do here.
ENTRYPOINT ["mini-bot"]
