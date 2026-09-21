#!/usr/bin/env bash
# Boot-time secret hydration.
#
# Fetches an AWS Secrets Manager secret (a JSON object), exposes each top-level
# key as an environment variable, then envsubst's every *.tpl file under the
# given config directory into its sibling file.
#
# The set of substituted names is derived FROM THE SECRET ITSELF, so unrelated
# environment variables on the host cannot leak into a rendered config through
# a name collision.
#
# Usage:
#   fetch-and-render-secrets.sh <secret-id> <config-dir> [owner[:group]]
#
# Exit codes:
#   0  success
#   1  argument / environment error (caller bug)
#   2  fetch or parse failed (network, IAM, secret missing or malformed)
#   3  a rendered file could not be written (disk full, permissions)
#
# Designed for systemd ExecStartPre. FAILS CLOSED: any error aborts the unit
# start, so a service never comes up holding a stale secret or a config still
# containing a literal ${PLACEHOLDER}. A daemon that starts with an
# unsubstituted password is far worse than one that does not start — it
# authenticates against nothing and the failure surfaces as a call problem.

set -euo pipefail

secret_id="${1:?usage: $0 <secret-id> <config-dir> [owner[:group]]}"
config_dir="${2:?usage: $0 <secret-id> <config-dir> [owner[:group]]}"
owner="${3:-}"

aws_region="${AWS_REGION:-eu-central-1}"

command -v aws      >/dev/null || { echo "fetch-and-render-secrets: aws cli not installed" >&2; exit 1; }
command -v jq       >/dev/null || { echo "fetch-and-render-secrets: jq not installed" >&2; exit 1; }
command -v envsubst >/dev/null || { echo "fetch-and-render-secrets: envsubst not installed (apt: gettext-base)" >&2; exit 1; }
[[ -d "$config_dir" ]] || { echo "fetch-and-render-secrets: config dir does not exist: $config_dir" >&2; exit 1; }

# --- Fetch ----------------------------------------------------------------

if ! secret_json=$(aws secretsmanager get-secret-value \
        --region "$aws_region" \
        --secret-id "$secret_id" \
        --query SecretString --output text 2>&1); then
  echo "fetch-and-render-secrets: failed to fetch $secret_id: $secret_json" >&2
  exit 2
fi

if ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"$secret_json"; then
  echo "fetch-and-render-secrets: secret $secret_id is not a JSON object" >&2
  exit 2
fi

# --- Export keys, and build the envsubst allowlist ------------------------
#
# Iterating keys and fetching each value separately, rather than one jq that
# emits a delimited stream into a bash read-loop. The pipeline version is
# shorter and has a failure mode worth avoiding: a NUL-versus-space delimiter
# mismatch makes substitution silently no-op, and the service then starts with
# placeholders still in its config.
allowlist=""
while IFS= read -r key; do
  value=$(jq -r --arg k "$key" '.[$k]' <<<"$secret_json")
  export "$key=$value"
  allowlist+="\${$key} "
done < <(jq -r 'keys[]' <<<"$secret_json")

if [[ -z "$allowlist" ]]; then
  echo "fetch-and-render-secrets: secret $secret_id has no keys — has it been populated?" >&2
  exit 2
fi

# --- Render ---------------------------------------------------------------
#
# Write to a temporary file and move into place. If envsubst fails partway
# through, the previous config stays where it is — the service is never left
# holding a half-written file at start time.
rendered=0
while IFS= read -r -d '' tpl; do
  out="${tpl%.tpl}"
  tmp="${out}.new"

  if ! envsubst "$allowlist" <"$tpl" >"$tmp"; then
    echo "fetch-and-render-secrets: envsubst failed for $tpl" >&2
    rm -f "$tmp"
    exit 3
  fi

  if [[ -n "$owner" ]]; then
    chown "$owner" "$tmp"
  fi
  chmod 0640 "$tmp"
  mv "$tmp" "$out"
  rendered=$((rendered + 1))
done < <(find "$config_dir" -type f -name '*.tpl' -print0)

echo "fetch-and-render-secrets: $secret_id -> $rendered file(s) under $config_dir"
