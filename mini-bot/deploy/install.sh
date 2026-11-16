#!/bin/sh
#
# Install or upgrade mini-bot on this host. Run as root, from a directory
# holding the `mini-bot` binary and `mini-bot.service`:
#
#   sudo ./install.sh <host-id> [pull-user]
#
#   host-id    VPSM_HOST_ID: the name this host's data is filed under. Must be
#              unique across your hosts and must never change.
#   pull-user  The login the collector SSHes in as to run `mini-bot export`.
#              Defaults to the user who ran sudo.
#
# Re-running is safe: it upgrades the binary and unit, and leaves an existing
# /etc/vpsm/mini-bot.env and the spool alone.
#
# Access model: the agent runs as root and writes its spool under
# /var/lib/vpsm; the pull user reads it and deletes what it has collected.
# Both are in group `vpsm`, the directory is setgid so new files inherit the
# group, and both ends run with umask 007. No sudo is involved in a pull.

set -eu

host_id=${1:?usage: sudo ./install.sh <host-id> [pull-user]}
pull_user=${2:-${SUDO_USER:-}}
here=$(cd "$(dirname "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh: run as root (sudo)" >&2
    exit 1
fi
if [ -z "$pull_user" ]; then
    echo "install.sh: name the user the collector will SSH in as" >&2
    exit 1
fi
id "$pull_user" >/dev/null

install -m 0755 "$here/mini-bot" /usr/local/bin/mini-bot

getent group vpsm >/dev/null || groupadd --system vpsm
usermod -aG vpsm "$pull_user"

install -d -m 2770 -o root -g vpsm /var/lib/vpsm /var/lib/vpsm/spool
# Upgrades: make sure anything already there is shared the same way.
chgrp -R vpsm /var/lib/vpsm
chmod -R g+rwX,o-rwx /var/lib/vpsm

install -d -m 0755 /etc/vpsm
if [ ! -e /etc/vpsm/mini-bot.env ]; then
    cat >/etc/vpsm/mini-bot.env <<EOF
# mini-bot configuration. Restart after editing:
#   sudo systemctl restart mini-bot

# The key every sample is filed under. Never change it for this host.
VPSM_HOST_ID=$host_id

# Seconds between samples. Below 5 buys frequency, not per-process
# precision: per-process CPU comes from a 100 Hz tick, so at 2s it moves in
# 0.5-point steps. Agent cost at 2s is roughly 0.2% of one CPU.
VPSM_INTERVAL_SECS=2

# Uncollected data is capped here. Past half of it the oldest samples are
# thinned to one per VPSM_SPOOL_THIN_SECS; past all of it they are dropped.
VPSM_SPOOL_MAX_MB=200
VPSM_SPOOL_THIN_SECS=60

VPSM_TOP_PROCESSES=10
VPSM_PROCESS_IO=true
EOF
    echo "wrote /etc/vpsm/mini-bot.env (host id: $host_id)"
else
    echo "kept existing /etc/vpsm/mini-bot.env"
fi

install -m 0644 "$here/mini-bot.service" /etc/systemd/system/mini-bot.service
systemctl daemon-reload
systemctl enable mini-bot >/dev/null 2>&1
systemctl restart mini-bot

sleep 1
systemctl --no-pager --lines=5 status mini-bot || true
echo
echo "installed. $pull_user can now be pulled from with:"
echo "  vpsm-collector pull $pull_user@<this host>"
echo "(a session already open as $pull_user must log in again to pick up the vpsm group)"
