#!/usr/bin/env bash
# render-fs-cli-conf.sh — write /etc/fs_cli.conf from AWS Secrets Manager.
#
# fs_cli needs two things this box cannot guess: the ESL password, and the
# address the event socket is bound to. Neither is a default.
#
#   password  lives in Secrets Manager, not in this repo
#   host      is the node's PRIVATE IP, not 127.0.0.1 — event_socket.conf.xml
#             binds listen-ip to the private address so the controller on the
#             app subnet can reach it. fs_cli's built-in default of 127.0.0.1
#             therefore fails at connect, before any password is considered.
#
# WHY A FILE AND NOT `fs_cli -p <secret>`: Linux exposes every process's
# arguments in /proc/<pid>/cmdline, which is WORLD-READABLE. A password passed
# as an argument is visible to any local account for the life of the call, on
# every invocation. A 0640 root:freeswitch file restricts it to the freeswitch
# group.
#
# Consumers, all of which are broken without it:
#   - the freeswitch role's `reload freeswitch xml` handler
#   - the unit's ExecReload (`fs_cli -x reloadxml`)
#   - any operator running fs_cli on the box to debug a call
#
# Rendered at EVERY FreeSWITCH start, so a rotated password is picked up by a
# `systemctl restart` without an Ansible run.
#
# Usage:
#   render-fs-cli-conf.sh <secret-id> [dest]
#
# Environment:
#   FS_CLI_HOST              ESL listen address     (default 127.0.0.1)
#   FS_CLI_PORT              ESL listen port        (default 8021)
#   FS_ESL_PASSWORD_FIELD    key within the secret  (default FREESWITCH_ESL_PASSWORD)
#   AWS_REGION               region for the SM call (default eu-west-1)
#
# Exit codes:
#   0  success
#   1  argument / environment error (caller bug)
#   2  SM fetch or parse failed (network, IAM, secret or field missing)
#   3  rendered file failed to write

set -euo pipefail

secret_id="${1:?usage: $0 <secret-id> [dest]}"
dest="${2:-/etc/fs_cli.conf}"

field="${FS_ESL_PASSWORD_FIELD:-FREESWITCH_ESL_PASSWORD}"
cli_host="${FS_CLI_HOST:-127.0.0.1}"
cli_port="${FS_CLI_PORT:-8021}"
aws_region="${AWS_REGION:-eu-west-1}"

# The v2 CLI installs to /usr/local/bin, which is not on the PATH systemd gives
# a unit. Add it rather than hardcoding the path, so this still works if the
# CLI moves.
export PATH="/usr/local/bin:${PATH}"

for bin in aws jq; do
  command -v "$bin" >/dev/null || {
    echo "render-fs-cli-conf: '$bin' not installed or not on PATH" >&2
    exit 1
  }
done

# --- Fetch ---------------------------------------------------------------

if ! secret_json=$(aws secretsmanager get-secret-value \
        --region "$aws_region" \
        --secret-id "$secret_id" \
        --query SecretString --output text 2>&1); then
  echo "render-fs-cli-conf: failed to fetch $secret_id: $secret_json" >&2
  exit 2
fi

if ! password=$(jq -re --arg k "$field" '.[$k] // empty' <<<"$secret_json"); then
  echo "render-fs-cli-conf: secret $secret_id has no '$field' field" >&2
  exit 2
fi

# --- Render --------------------------------------------------------------
#
# Write-then-move, so a failure partway through never leaves fs_cli reading a
# half-written file.
tmp="${dest}.new"
umask 077

if ! cat > "$tmp" <<CONF
# Managed by render-fs-cli-conf.sh — do not edit manually.
# Regenerated from ${secret_id} at every FreeSWITCH start.
[default]
host     => ${cli_host}
port     => ${cli_port}
password => ${password}
CONF
then
  echo "render-fs-cli-conf: failed to write $tmp" >&2
  rm -f "$tmp"
  exit 3
fi

# root:freeswitch 0640 — readable by the service account and root, nothing
# else. fs_cli prefers $HOME/.fs_cli_conf and falls back to this path, so an
# operator's own file still wins if they have one.
chown root:freeswitch "$tmp"
chmod 0640 "$tmp"
mv "$tmp" "$dest"

echo "render-fs-cli-conf: ${secret_id} -> ${dest} (host ${cli_host}:${cli_port})"
