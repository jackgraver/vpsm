#!/bin/sh
#
# Report to the collector on an interval, the way mini-bot will once its
# reporting half exists. A stand-in, not a model answer — see ../README.md.
#
# Two things here are deliberate rather than incidental, because they are the
# agent behaviours worth having a live example of:
#
#   * It opens with a backfill batch, then settles into one sample per
#     interval. That is what an agent does after an outage, and it means the
#     fleet view has sparklines immediately instead of after several minutes
#     of watching.
#   * It retries connection failures with backoff and keeps going. The
#     collector being down is the normal case this is supposed to survive, so
#     a sim that exits on a refused connection would hide exactly the
#     behaviour under test.
#
# Environment:
#   VPSM_COLLECTOR_URL  Base URL of the collector (required).
#   VPSM_COLLECTOR_TOKEN  Bearer token (required unless the collector is anonymous).
#   VPSM_HOST_ID        Host identity (required — the collector 400s without it).
#   VPSM_INTERVAL_SECS  Seconds between reports (default 5).
#   SIM_BACKFILL        Samples in the opening batch (default 30).
#   PROFILE             healthy or hot; passed through to gen-report.sh.

set -eu

: "${VPSM_COLLECTOR_URL:?set VPSM_COLLECTOR_URL}"
: "${VPSM_HOST_ID:?set VPSM_HOST_ID}"

interval=${VPSM_INTERVAL_SECS:-5}
backfill=${SIM_BACKFILL:-30}
token=${VPSM_COLLECTOR_TOKEN:-}
endpoint="${VPSM_COLLECTOR_URL%/}/v1/reports"

export INTERVAL_SECS="$interval"

auth=""
[ -n "$token" ] && auth="Authorization: Bearer $token"

# post <samples> <tick> — returns 0 when the batch landed, 1 when it did not.
#
# Branching on the response body's `code` rather than on its prose is the
# contract: the message wording is explicitly not stable, and `clock_skew`
# (an operator fixes the monitored box's clock) has to be tellable from
# `malformed` (the agent author fixes the agent) when both arrive as 400s
# that must not be retried.
post() {
    body=$(./gen-report.sh "$VPSM_HOST_ID" "$1" "$2")
    response=$(printf '%s' "$body" | curl -sS \
        -o /tmp/sim-response -w '%{http_code}' \
        -X POST "$endpoint" \
        -H 'Content-Type: application/json' \
        ${auth:+-H "$auth"} \
        --data-binary @- 2>/tmp/sim-curl-error) || {
        echo "agent-sim: cannot reach $endpoint: $(cat /tmp/sim-curl-error)" >&2
        return 1
    }

    case "$response" in
    2*)
        echo "agent-sim: sent $1 sample(s) at tick $2 -> $(cat /tmp/sim-response)"
        return 0
        ;;
    413)
        echo "agent-sim: 413 too large; a real agent halves the batch and retries" >&2
        return 1
        ;;
    4*)
        # Never retried: a rejected payload stays rejected, and an agent that
        # retries one forever is how the agent becomes the outage.
        echo "agent-sim: $response rejected, discarding batch: $(cat /tmp/sim-response)" >&2
        return 0
        ;;
    *)
        echo "agent-sim: $response from collector, keeping batch: $(cat /tmp/sim-response)" >&2
        return 1
        ;;
    esac
}

echo "agent-sim: reporting as '$VPSM_HOST_ID' to $endpoint every ${interval}s (profile ${PROFILE:-healthy})"

tick=1

# Wait for the collector before the opening batch, so the first thing in the
# log is data rather than a stack of refused connections.
backoff=1
until post "$backfill" "$tick"; do
    sleep "$backoff"
    [ "$backoff" -lt 30 ] && backoff=$(( backoff * 2 ))
done
tick=$(( tick + backfill ))

while :; do
    sleep "$interval"
    # Tick advances whether or not the post landed, so counters keep tracking
    # wall clock across an outage instead of resuming where they left off.
    post 1 "$tick" || true
    tick=$(( tick + 1 ))
done
