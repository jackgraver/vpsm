#!/usr/bin/env bash
#
# Assert the collector behaves, over HTTP, against the running stack.
#
#   docker compose up --build -d
#   ./check.sh
#
# What this is for: the protocol's contract is mostly about behaviour when
# something is wrong — which status and which code come back, what happens to
# a retried flush, what a null survives as. Unit tests cover each of those
# in process. These cover them through a real socket, a real container and a
# real SQLite file, which is where the wiring between them can be wrong while
# every unit test still passes.
#
# Several checks assert on *rendered* output rather than on JSON. That is
# deliberate: the collector renders anything it cannot derive as a dash, so a
# field that silently stopped arriving — a rename on one side of the wire, say
# — shows up here as a missing value instead of passing as a zero.
#
# Each check names the property it defends rather than the call it makes, so a
# failure says what broke and not just which line.

set -uo pipefail

cd "$(dirname "$0")"

BASE=${BASE:-http://127.0.0.1:8777}
TOKEN=${TOKEN:-dev}
COMPOSE=${COMPOSE:-docker compose}

# Host ids are fixed rather than timestamped so that re-running this script
# reuses the same rows instead of leaving a new pair of test hosts in the
# fleet view every time. The zz- prefix sorts them below the real sim hosts.
CHECK_HOST=zz-check
BATCH_HOST=zz-check-batch

pass=0
fail=0
status=
BODY=$(mktemp)
BIG=$(mktemp)
trap 'rm -f "$BODY" "$BIG"' EXIT

ok() { printf '  \033[32mok\033[0m    %s\n' "$1"; pass=$((pass + 1)); }
no() {
    printf '  \033[31mFAIL\033[0m  %s\n' "$1"
    [ -n "${2:-}" ] && printf '        %s\n' "${2//$'\n'/$'\n'        }"
    fail=$((fail + 1))
}
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# --- requests ------------------------------------------------------------
# Each sets $status and leaves the response body in $BODY.

GET() {
    local path=$1; shift
    status=$(curl -sS -o "$BODY" -w '%{http_code}' "$BASE$path" "$@")
}

POST() {
    local data=$1; shift
    status=$(curl -sS -o "$BODY" -w '%{http_code}' \
        -X POST "$BASE/v1/reports" \
        -H 'Content-Type: application/json' \
        "$@" --data-binary "$data")
}

# Forwards extra curl arguments, which matters: a version of this that
# swallowed them silently sent every "rejected" request as a valid one.
auth_POST() {
    local data=$1; shift
    POST "$data" -H "Authorization: Bearer $TOKEN" "$@"
}

# --- assertions ----------------------------------------------------------

# code reads the stable `code` field out of an error body. Deliberately the
# only thing checked on a rejection: the prose beside it is for logs, and the
# protocol says its wording is not stable.
code() { grep -o '"code":"[^"]*"' "$BODY" | head -1 | cut -d'"' -f4; }

# http <name> <want-status> [want-code]
http() {
    local what=$1 want=$2 wantcode=${3:-} got
    if [ "$status" != "$want" ]; then
        no "$what" "expected HTTP $want, got $status: $(head -c 200 "$BODY")"
        return
    fi
    if [ -n "$wantcode" ]; then
        got=$(code)
        if [ "$got" != "$wantcode" ]; then
            no "$what" "expected code '$wantcode', got '${got:-none}': $(head -c 200 "$BODY")"
            return
        fi
    fi
    ok "$what"
}

# body <name> <grep-pattern...> — every pattern must be present in $BODY.
body() {
    local what=$1; shift
    local p
    for p in "$@"; do
        if ! grep -q -- "$p" "$BODY"; then
            no "$what" "'$p' not in: $(head -c 300 "$BODY")"
            return
        fi
    done
    ok "$what"
}

# text <name> <haystack> <grep-pattern...>
text() {
    local what=$1 hay=$2; shift 2
    local p
    for p in "$@"; do
        if ! printf '%s' "$hay" | grep -q -- "$p"; then
            no "$what" "'$p' not in:"$'\n'"$hay"
            return
        fi
    done
    ok "$what"
}

# --------------------------------------------------------------------------
section 'reachability'

GET /healthz
http 'healthz answers without a token' 200

GET /v1/reports -X GET -H "Authorization: Bearer $TOKEN"
http 'GET on the ingest endpoint is a 405, not a handler' 405

# --------------------------------------------------------------------------
section 'auth'

good=$(./agent-sim/gen-report.sh "$CHECK_HOST" 1)

POST "$good"
http 'an unauthenticated report is refused' 401 unauthorized

POST "$good" -H 'Authorization: Bearer wrong'
http 'a wrong token is refused' 401 unauthorized

# --------------------------------------------------------------------------
section 'rejections carry a code the agent can branch on'

auth_POST '{"host":{"id":"x"},"samples":['
http 'truncated JSON is malformed' 400 malformed

auth_POST '{"host":{"hostname":"nameless"},"samples":[{"at":"2026-01-01T00:00:00Z","cpu":{"ticks":{}},"memory":{"total_bytes":1,"used_bytes":0}}]}'
http 'a report with no host.id is refused, never guessed at' 400 invalid

# used > total is the cheapest possible incoherence, and the one that would
# make every memory percentage downstream nonsense.
auth_POST '{"host":{"id":"zz-check-invalid"},"samples":[{"at":"2026-01-01T00:00:00Z","cpu":{"ticks":{}},"memory":{"total_bytes":100,"used_bytes":200}}]}'
http 'memory used above total is refused' 400 invalid

# The whole reason ErrorCode exists: an agent whose clock is wrong and an
# agent with a serialisation bug both get a 400 they must not retry, and the
# person who has to fix it is a different one in each case.
auth_POST "$(./agent-sim/gen-report.sh zz-check-skew 1 1 "$(date -u -d '+48 hours' +%s)")"
http 'a sample stamped two days ahead is clock_skew, not malformed' 400 clock_skew

# Over the 16 MiB wire cap. The one 4xx an agent is expected to act on rather
# than discard: halve the batch and retry.
#
# The padding goes inside a string value so the body stays parseable as far as
# it gets read. A body of plain junk would fail on its first byte and come
# back as `malformed`, which is a different contract and would make this check
# pass for the wrong reason. In the field a 413 means the batch was too big;
# this is the cheap equivalent, since a full 1024-sample flush is only ~1.4 MB.
{
    printf '{"host":{"id":"zz-check-big","hostname":"'
    head -c 17000000 /dev/zero | tr '\0' 'a'
    printf '"},"samples":[]}'
} > "$BIG"
auth_POST "@$BIG"
http 'an oversized body is 413 too_large, not a parse error' 413 too_large

auth_POST "$good" -H 'Content-Encoding: deflate'
http 'an encoding the collector does not speak is named as such' 400 unsupported_encoding

# --------------------------------------------------------------------------
section 'ingest and idempotency'

one=$(./agent-sim/gen-report.sh "$CHECK_HOST" 1)

auth_POST "$one"
http 'a valid report is accepted' 200
body 'a valid report is accepted as new' '"accepted":1' '"duplicate":0'

# The property buffering depends on: an agent that does not know whether its
# flush landed can send it again, and the second attempt is counted rather
# than erroring or writing the reading twice.
auth_POST "$one"
http 'resending the same batch is not an error' 200
body 'resending the same batch counts as duplicate, not accepted' '"accepted":0' '"duplicate":1'

# A backfill flush in the shape the agent will actually send it: process list
# on the newest sample only, absent from the superseded ones.
auth_POST "$(./agent-sim/gen-report.sh "$BATCH_HOST" 12 100)"
http 'a backfill batch is accepted' 200
body 'all 12 samples of a mixed batch land' '"accepted":12'

# --------------------------------------------------------------------------
section 'reads'

GET /v1/hosts -H "Authorization: Bearer $TOKEN"
http 'the overview reads back' 200
body 'the overview lists a host that has reported' "\"$CHECK_HOST\""

GET /v1/hosts/no-such-host -H "Authorization: Bearer $TOKEN"
http 'an unknown host is a 404 with a code, not an empty success' 404 not_found

GET "/v1/hosts/$BATCH_HOST" -H "Authorization: Bearer $TOKEN"
http 'a host read carries its process snapshot back' 200
# 780.5 is the deliberate >100% entry: cpu_pct is a share of one core. If
# anything in the stack ever validates it against 100, this is where it shows.
body 'a process at 780% of one core round-trips intact' '780.5'
# user is a nullable field with omitempty, so an unresolved uid comes back as
# an absent key — what must never happen is it arriving as a name or a zero.
if grep -q '"uid":60813,"threads"' "$BODY"; then
    ok 'an unresolved uid stays null rather than becoming a name'
else
    no 'an unresolved uid stays null rather than becoming a name' "$(grep -o '"pid":4096[^}]*' "$BODY" | head -c 300)"
fi

for h in sim-web-01 sim-db-01; do
    GET "/v1/hosts/$h" -H "Authorization: Bearer $TOKEN"
    if [ "$status" = 200 ]; then
        ok "$h is reporting over the container network"
    else
        no "$h is reporting over the container network" \
            "HTTP $status — is the stack up? \`$COMPOSE logs agent-sim\`"
    fi
done

# --------------------------------------------------------------------------
section 'the views render what landed'

# Run the CLI inside the container, against the same SQLite file the server is
# writing to. WAL mode is what makes that concurrent read work at all, so this
# is also a check on the database being opened the way storage intends.
fleet=$($COMPOSE exec -T -e NO_COLOR=1 collector vpsm-collector status 2>&1)
text 'the fleet view lists both sim hosts' "$fleet" 'sim-web-01' 'sim-db-01'

hot=$(printf '%s' "$fleet" | awk '$2=="sim-db-01"')
# Steal is graded on its own scale (warn 5, crit 15) because a few percent of
# stolen CPU means something a few percent of user CPU does not. 25% has to
# reach the flags column, where a red dot gets a visible cause.
text 'steal reaches the flags column on its own scale' "$hot" 'steal 25%'
# Fullness is the worse of bytes and inodes: this filesystem is 97% by bytes
# and 99% by inodes, so the row has to say inodes or it reads as the wrong
# problem.
text 'filesystem pressure names inodes when inodes are the binding limit' "$hot" 'inodes'
# Counters diffed into movement, not smeared into a rate.
text 'moving error counters are flagged as movement' "$hot" 'net errs'

detail=$($COMPOSE exec -T -e NO_COLOR=1 collector vpsm-collector host sim-db-01 2>&1)
text 'host detail renders the process snapshot' "$detail" 'postgres' 'kworker'
text 'host detail pairs iowait with the blocked-task count' "$detail" 'iowait' 'blocked'
text 'lifetime error totals are shown with their movement' "$detail" '(+9)'

# The four process-table properties the contract says will cause bugs
# downstream if they are not known. Fields are whitespace-separated, so this
# depends on column order but not on column width.
cpu_row=$(printf '%s' "$detail" | awk '$1=="1234"')
text 'a process over 100% of one core is displayed, not clamped' "$cpu_row" '780.5'

denied=$(printf '%s' "$detail" | awk '$1=="2048"{print $7}')
measured_zero=$(printf '%s' "$detail" | awk '$1=="4096"{print $7}')
unresolved_user=$(printf '%s' "$detail" | awk '$1=="4096"{print $2}')

# The sharpest end of "null is not zero": one process had its io read denied
# by ptrace, the other genuinely did no I/O. They must not look the same.
if [ "$denied" = 'n/a' ] && [ "$measured_zero" = '0B/s' ]; then
    ok 'unmeasured I/O renders n/a while a real zero renders 0B/s'
else
    no 'unmeasured I/O renders n/a while a real zero renders 0B/s' \
        "pid 2048 io='$denied' (want n/a), pid 4096 io='$measured_zero' (want 0B/s)"
fi

if [ "$unresolved_user" = '?' ]; then
    ok 'an unresolvable user renders ? rather than root'
else
    no 'an unresolvable user renders ? rather than root' "pid 4096 user='$unresolved_user'"
fi

# A host with one sample has no CPU percentage to derive — the display has to
# say unknown rather than draw a false 0%.
single=zz-check-single-$(date -u +%s)
auth_POST "$(./agent-sim/gen-report.sh "$single" 1)" > /dev/null
fresh=$($COMPOSE exec -T -e NO_COLOR=1 collector vpsm-collector status 2>&1 | awk -v h="$single" '$2==h')
if printf '%s' "$fresh" | grep -q '—'; then
    ok 'a single sample shows CPU as unknown, not as 0%'
else
    no 'a single sample shows CPU as unknown, not as 0%' "$fresh"
fi

# --------------------------------------------------------------------------
printf '\n'
if [ "$fail" -eq 0 ]; then
    printf '\033[32m%d checks passed.\033[0m\n' "$pass"
    # The single-sample check needs a host the collector has never seen, so it
    # cannot reuse an id the way the others do. Said out loud because an
    # unexplained extra row in `status` otherwise looks like a bug.
    printf 'Left a %s row behind; `docker compose down` clears it.\n' "$single"
    exit 0
fi
printf '\033[31m%d passed, %d failed.\033[0m\n' "$pass" "$fail"
exit 1
