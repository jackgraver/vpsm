#!/bin/sh
#
# Emit one protocol-conformant report on stdout.
#
# This is a stand-in for mini-bot's unbuilt reporting half (see
# ../README.md). It exists so the collector's ingest path can be exercised
# end to end over real HTTP today, and so there is a known-good set of wire
# bytes for the agent side to be compared against once it starts sending.
#
# Hand-written JSON is the right shape for this job for the same reason the
# protocol is JSON over HTTP: no schema compiler, readable in a diff, and
# what an agent author needs to see is the bytes. The cost is that it can
# drift from protocol.go silently — a renamed field would be ignored by Go's
# decoder rather than rejected. ../check.sh is what catches that: it asserts
# on *rendered values*, so a field that stops arriving shows up as a dash.
#
# Usage:
#   gen-report.sh <host-id> [samples] [tick] [end-epoch]
#
#   host-id     VPSM_HOST_ID equivalent. Required; the collector 400s without it.
#   samples     How many samples the report carries (default 1, max 1024).
#   tick        Counter ordinal for the oldest sample. Advance it between
#               calls so cumulative counters move forward and the collector
#               has something to diff.
#   end-epoch   Timestamp of the NEWEST sample, as unix seconds (default now).
#
# Environment:
#   INTERVAL_SECS  Spacing between samples in a batch (default 5).
#   PROFILE        healthy (default) or hot. See below.
#
# PROFILE=hot produces a host in trouble on the axes the collector grades
# separately: 25% steal, a filesystem at 97% bytes and 99% inodes, swap in
# use, and network error counters that are moving. It is the case the fleet
# overview's flags column exists for, so it is worth being able to produce on
# demand.

set -eu

host_id=${1:?usage: gen-report.sh <host-id> [samples] [tick] [end-epoch]}
samples=${2:-1}
tick=${3:-1}
end_epoch=${4:-$(date -u +%s)}

interval=${INTERVAL_SECS:-5}
profile=${PROFILE:-healthy}

# A 4-core box at 100 Hz accrues 400 ticks of CPU time per wall second, so one
# interval is 400 * interval ticks spread across the eight states. Keeping the
# per-sample deltas summing to exactly that is what makes the collector's
# derived percentages come out as intended rather than as arbitrary noise.
cores=4
ticks_per_sample=$(( cores * 100 * interval ))

case "$profile" in
healthy)
    d_steal=$(( ticks_per_sample / 200 ))     # 0.5%
    swap_used=0
    fs_used=21474836480                       # 20 GiB of 50
    fs_avail=29527900160
    fs_inodes_used=412000
    net_err_growth=0
    ;;
hot)
    d_steal=$(( ticks_per_sample / 4 ))       # 25% — past the 15% crit line
    swap_used=1610612736
    fs_used=50465865728                       # 96.9% by df's reckoning
    fs_avail=1610612736
    fs_inodes_used=3250000                    # 99.2% of inodes
    net_err_growth=9
    ;;
*)
    echo "gen-report.sh: unknown PROFILE=$profile (want healthy or hot)" >&2
    exit 2
    ;;
esac

# Whatever steal takes, idle gives up, so the eight deltas still total
# ticks_per_sample and the host does not appear to gain or lose a CPU.
d_user=$(( ticks_per_sample * 15 / 100 ))
d_nice=0
d_system=$(( ticks_per_sample * 5 / 100 ))
d_iowait=$(( ticks_per_sample * 3 / 100 ))
d_irq=$(( ticks_per_sample / 200 ))
d_softirq=$(( ticks_per_sample / 100 ))
d_idle=$(( ticks_per_sample - d_user - d_nice - d_system - d_iowait - d_irq - d_softirq - d_steal ))

mem_total=8589934592
mem_avail=3221225472
# total - MemAvailable, never total - MemFree. The collector documents this as
# the thing nothing downstream should recompute; sending it the other way
# would make a healthy box read as ~100% full.
mem_used=$(( mem_total - mem_avail ))

printf '{'
printf '"host":{'
printf '"id":"%s",' "$host_id"
printf '"hostname":"%s",' "${SIM_HOSTNAME:-$host_id}"
printf '"os":"Alpine Linux 3.22",'
printf '"kernel":"6.18.63",'
printf '"cores":%d,' "$cores"
printf '"agent_version":"agent-sim"'
printf '},'
printf '"samples":['

i=0
while [ "$i" -lt "$samples" ]; do
    # i counts from the oldest sample, so counters advance with it and the
    # newest sample is the one stamped end_epoch.
    n=$(( tick + i ))
    at=$(date -u -d "@$(( end_epoch - interval * (samples - 1 - i) ))" +%Y-%m-%dT%H:%M:%SZ)

    [ "$i" -gt 0 ] && printf ','
    printf '{'
    printf '"at":"%s",' "$at"

    # Not the configured interval: the agent reports the window it actually
    # measured, because a loaded box is when the scheduler is least punctual.
    printf '"elapsed_secs":%d.08,' "$interval"
    printf '"uptime_secs":%d,' $(( 100000 + n * interval ))

    # Host gauges on every sample, not only inside the process block. Paired
    # with CPU iowait they are what separates "stuck on storage" from "busy",
    # and that only reads as an answer if they have history.
    printf '"procs_running":3,'
    printf '"procs_blocked":%d,' "$( [ "$profile" = hot ] && echo 11 || echo 1 )"

    printf '"cpu":{"ticks":{'
    printf '"user":%d,' $(( 400000 + n * d_user ))
    printf '"nice":%d,' $(( 1000 + n * d_nice ))
    printf '"system":%d,' $(( 90000 + n * d_system ))
    printf '"idle":%d,' $(( 3000000 + n * d_idle ))
    printf '"iowait":%d,' $(( 20000 + n * d_iowait ))
    printf '"irq":%d,' $(( 5000 + n * d_irq ))
    printf '"softirq":%d,' $(( 8000 + n * d_softirq ))
    printf '"steal":%d' $(( 1500 + n * d_steal ))
    printf '},'
    printf '"load1":%s,"load5":%s,"load15":%s' \
        "$( [ "$profile" = hot ] && echo 7.21 || echo 0.42 )" \
        "$( [ "$profile" = hot ] && echo 6.04 || echo 0.55 )" \
        "$( [ "$profile" = hot ] && echo 4.88 || echo 0.61 )"
    printf '},'

    printf '"memory":{'
    printf '"total_bytes":%d,' "$mem_total"
    printf '"used_bytes":%d,' "$mem_used"
    printf '"available_bytes":%d,' "$mem_avail"
    printf '"swap_total_bytes":2147483648,'
    printf '"swap_used_bytes":%d' "$swap_used"
    printf '},'

    # Two mounts, so the "fullest filesystem" column has something to pick
    # between. available_bytes is carried because df-style fullness is
    # used/(used+available) — the reserved-for-root blocks are excluded, which
    # is why it is not redundant with total-used.
    printf '"filesystems":['
    printf '{"device":"/dev/vda1","mount_point":"/","fs_type":"ext4",'
    printf '"total_bytes":53687091200,"used_bytes":%d,"available_bytes":%d,' "$fs_used" "$fs_avail"
    printf '"inodes_total":3276800,"inodes_used":%d},' "$fs_inodes_used"
    printf '{"device":"/dev/vdb1","mount_point":"/var/lib/docker","fs_type":"ext4",'
    printf '"total_bytes":107374182400,"used_bytes":32212254720,"available_bytes":75161927680,'
    printf '"inodes_total":6553600,"inodes_used":198000}'
    printf '],'

    # Counters, not rates. Errors and drops are counters too: the question is
    # never the rate, it is whether they are moving at all.
    printf '"net":[{'
    printf '"iface":"eth0",'
    printf '"rx_bytes":%d,' $(( 900000000 + n * 1250000 ))
    printf '"tx_bytes":%d,' $(( 400000000 + n * 300000 ))
    printf '"rx_packets":%d,' $(( 4200000 + n * 1100 ))
    printf '"tx_packets":%d,' $(( 3100000 + n * 900 ))
    printf '"rx_errors":%d,' $(( 7 + n * net_err_growth ))
    printf '"tx_errors":0,'
    printf '"rx_dropped":%d,' $(( 3 + n * net_err_growth ))
    printf '"tx_dropped":0'
    printf '}]'

    # Processes ride on the NEWEST sample only. This mirrors what the agent
    # will do: the collector keeps one snapshot per host and discards the
    # rest, so attaching a list to every buffered sample would make a
    # post-outage flush ~85% payload the collector throws away. Producing the
    # mixed-batch shape here is deliberate — it is the shape with the most
    # room to be mishandled.
    if [ "$i" -eq $(( samples - 1 )) ]; then
        printf ',"processes":{'
        printf '"total":284,"threads":1104,'
        printf '"running":3,"sleeping":279,"blocked":1,"zombie":1,"stopped":0,'
        printf '"top":['

        # cpu_pct is a share of ONE core and legitimately exceeds 100. An
        # 8-thread process at 780% is the case that a 0-100 validation
        # anywhere in the stack would wrongly reject, so the harness always
        # sends it.
        printf '{"pid":1234,"ppid":1,"name":"postgres","command":"postgres: parallel worker",'
        printf '"state":"R","uid":70,"user":"postgres","threads":8,'
        printf '"cpu_pct":780.5,"rss_bytes":545259520,"virtual_bytes":2147483648,"mem_pct":6.3,'
        printf '"io":{"read_bytes_per_sec":1048576.0,"write_bytes_per_sec":262144.0}},'

        # No "io" key at all: ptrace denied the read. Null, not zero —
        # rendering it as 0 B/s would turn a permissions problem into a
        # silently wrong answer.
        printf '{"pid":2048,"ppid":1,"name":"nginx","command":"nginx: worker process",'
        printf '"state":"S","uid":33,"user":"www-data","threads":1,'
        printf '"cpu_pct":12.5,"rss_bytes":104857600,"virtual_bytes":419430400,"mem_pct":1.2},'

        # "user":null — the uid did not resolve to a name. Also null, not
        # "root" and not an empty string.
        printf '{"pid":4096,"ppid":1,"name":"mystery","command":"/opt/vendor/bin/daemon --quiet",'
        printf '"state":"S","uid":60813,"user":null,"threads":2,'
        printf '"cpu_pct":0.3,"rss_bytes":2147483648,"virtual_bytes":4294967296,"mem_pct":25.0,'
        printf '"io":{"read_bytes_per_sec":0.0,"write_bytes_per_sec":0.0}},'

        # A kernel thread: bracketed command, no io counters of its own.
        printf '{"pid":57,"ppid":2,"name":"kworker/3:1","command":"[kworker/3:1]",'
        printf '"state":"I","uid":0,"user":"root","threads":1,'
        printf '"cpu_pct":0.1,"rss_bytes":0,"mem_pct":0.0}'

        printf ']}'
    fi

    printf '}'
    i=$(( i + 1 ))
done

printf ']}'
printf '\n'
