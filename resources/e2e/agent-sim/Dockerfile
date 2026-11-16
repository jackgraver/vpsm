# The stand-in agent: curl and a shell, nothing else.
#
# Real mini-bot lives in ../../mini-bot and has its own image. This exists
# only because the agent cannot report yet (see ../README.md), and should be
# deleted once it can.

FROM alpine:3.22

# coreutils for GNU date: the generator needs `date -u -d @<epoch>` to stamp a
# backfill batch, and busybox date's support for that is not something to bet
# the harness on.
RUN apk add --no-cache curl coreutils

WORKDIR /sim
COPY gen-report.sh loop.sh ./
RUN chmod +x gen-report.sh loop.sh

ENTRYPOINT ["./loop.sh"]
