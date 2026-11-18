#!/bin/sh
#
# Install or upgrade mini-bot on this host. Run as root, from a directory
# holding the `mini-bot` binary, `mini-bot.service` and
# `journald-vpsm.conf`:
#
#   sudo ./install.sh <host-id> [pull-user] [deploy-user]
#
#   host-id      VPSM_HOST_ID: the name this host's data is filed under. Must
#                be unique across your hosts and must never change.
#   pull-user    The login the laptop SSHes in as to run `mini-bot export` and
#                `mini-bot logs`. Defaults to the user who ran sudo.
#   deploy-user  Optional: the login j-cicd-agent runs as, to let it upgrade
#                mini-bot without root (see "Deploys" below).
#
# Re-running is safe: it upgrades the binary and unit, and leaves an existing
# /etc/vpsm/mini-bot.env and the spool alone.
#
# Access model: the agent runs as root and writes its spool under
# /var/lib/vpsm; the pull user reads it and deletes what it has collected.
# Both are in group `vpsm`, the directory is setgid so new files inherit the
# group, and both ends run with umask 007. No sudo is involved in a pull.
#
# Binary: /opt/vpsm/bin/mini-bot, which the unit runs; /usr/local/bin/mini-bot
# is a symlink to it so `ssh <host> mini-bot export` finds it on PATH.
#
# Deploys: with a deploy-user, /opt/vpsm/bin belongs to that user, so
# j-cicd-agent (running as it) can replace the binary atomically, and a
# sudoers rule lets it run exactly `systemctl restart mini-bot.service` and
# nothing else as root. That makes the deploy user able to choose what root
# runs here: give it to a user who could already become root (sudo or docker
# group), not to a restricted one. Without a deploy-user the directory stays
# root's and upgrades go through this script.
#
# Logs: the pull user joins `systemd-journal`, which reads the journal and
# nothing else, and journald gets a disk cap and persistent storage
# (journald-vpsm.conf). Docker's log driver is NOT changed here: that needs a
# Docker restart and recreated containers, so the script prints the steps
# instead (see docker-daemon.json).

set -eu

host_id=${1:?usage: sudo ./install.sh <host-id> [pull-user] [deploy-user]}
pull_user=${2:-${SUDO_USER:-}}
deploy_user=${3:-}
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
# Re-running without a deploy-user keeps the one set up before, rather than
# quietly taking the directory back and breaking deploys.
if [ -z "$deploy_user" ] && [ -d /opt/vpsm/bin ]; then
    owner=$(stat -c %U /opt/vpsm/bin)
    if [ "$owner" != root ]; then
        deploy_user=$owner
        echo "keeping $deploy_user as the deploy user (owner of /opt/vpsm/bin)"
    fi
fi
bin_owner=root
if [ -n "$deploy_user" ]; then
    id "$deploy_user" >/dev/null
    bin_owner=$deploy_user
fi

install -d -m 0755 /opt/vpsm
install -d -m 0755 -o "$bin_owner" -g "$(id -gn "$bin_owner")" /opt/vpsm/bin
# Written beside it and renamed over it, like the agent does, so the running
# service never sees a half-written file.
install -m 0755 -o "$bin_owner" "$here/mini-bot" /opt/vpsm/bin/.mini-bot.new
mv -f /opt/vpsm/bin/.mini-bot.new /opt/vpsm/bin/mini-bot
ln -sfn /opt/vpsm/bin/mini-bot /usr/local/bin/mini-bot

sudoers=/etc/sudoers.d/vpsm-mini-bot
if [ -n "$deploy_user" ]; then
    systemctl_path=$(command -v systemctl)
    tmp=$(mktemp)
    printf '# Written by vpsm install.sh: lets j-cicd-agent restart mini-bot after a deploy.\n%s ALL=(root) NOPASSWD: %s restart mini-bot.service\n' \
        "$deploy_user" "$systemctl_path" >"$tmp"
    if visudo -cqf "$tmp"; then
        install -m 0440 "$tmp" "$sudoers"
        echo "wrote $sudoers ($deploy_user may restart mini-bot.service)"
    else
        echo "install.sh: generated sudoers rule failed visudo -c; not installed" >&2
    fi
    rm -f "$tmp"
fi

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

# Logs live in journald; the pull user reads them through `mini-bot logs`.
usermod -aG systemd-journal "$pull_user"
install -d -m 0755 /etc/systemd/journald.conf.d
install -m 0644 "$here/journald-vpsm.conf" /etc/systemd/journald.conf.d/vpsm.conf
systemctl restart systemd-journald

install -m 0644 "$here/mini-bot.service" /etc/systemd/system/mini-bot.service
systemctl daemon-reload
systemctl enable mini-bot >/dev/null 2>&1
systemctl restart mini-bot

sleep 1
systemctl --no-pager --lines=5 status mini-bot || true
echo
echo "installed. $pull_user can now be pulled from with:"
echo "  vpsm-collector pull $pull_user@<this host>"
echo "and its logs read with:"
echo "  ssh $pull_user@<this host> mini-bot logs --since -1h | vpsm-logs"
echo "(a session already open as $pull_user must log in again to pick up the new groups)"
if [ -n "$deploy_user" ]; then
    echo
    echo "j-cicd deploys: add to $deploy_user's j-cicd-agent config:"
    echo "  - name: mini-bot"
    echo "    files: [{name: mini-bot, path: /opt/vpsm/bin/mini-bot}]"
    echo "    systemd: {unit: mini-bot.service, settle: 15s}"
fi

if command -v docker >/dev/null 2>&1; then
    driver=$(docker info --format '{{.LoggingDriver}}' 2>/dev/null || true)
    if [ "$driver" != "journald" ]; then
        echo
        echo "Docker logs to '${driver:-unknown}', not journald, so container logs won't reach mini-bot logs."
        echo "When a brief restart of every container is acceptable:"
        echo "  sudo cp $here/docker-daemon.json /etc/docker/daemon.json   # merge by hand if one exists"
        echo "  sudo systemctl restart docker"
        echo "  docker compose up -d --force-recreate                     # in each project"
        echo "Only containers created after the restart use the new driver."
    fi
fi
