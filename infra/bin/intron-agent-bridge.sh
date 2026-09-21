#!/usr/bin/env bash
#
# =============================================================================
# intron-agent-bridge.sh
#
# RUNS ON INTRON'S VOICE BOX (Calls05), NOT ON THIS PLATFORM.
# =============================================================================
#
# Every other script in this directory runs on an operator laptop against our
# own AWS account. This one is different: you copy it to a machine belonging to
# another team, in another AWS account, that is carrying live outbound calls.
# It is written defensively for that reason.
#
# WHAT IT DOES
#   Registers a SECOND voice-agent worker against the Intron SIP platform's
#   LiveKit server, so calls arriving on our platform can be answered by the
#   models already running on that box.
#
# WHY A SECOND WORKER AND NOT A CHANGE TO THE EXISTING ONE
#   A LiveKit agent worker connects OUT to exactly one server and waits to be
#   given work. Repointing the existing worker would take it off Intron's own
#   outbound calls. A second unit, with its own environment, leaves that one
#   completely alone.
#
# WHAT IT DOES NOT TOUCH, EVER
#   intron_service_convobot_worker_agent_livekit.service   the production agent
#   intron_service_convobot_worker_agent_launcher@*        the 15 outbound dialers
#   opensips.service                                       the SIP proxy
#   service_convo_bot/.env                                 the shared config
#   anything under /home/ubuntu/service_voice_bots         their git checkout
#
#   Everything this script creates lives in /opt/intron-sip-bridge plus one
#   systemd unit, and `--uninstall` removes all of it.
#
# WHAT IT SHARES, WHICH IS THE REAL COST
#   One Tesla T4. The existing worker and this one draw from the same 15 GiB.
#   Idle cost is near zero (NUM_IDLE_PROCESSES=0, models load on job entry),
#   so the contention is per concurrent call, not baseline. Watch nvidia-smi
#   under load before promising anyone capacity.
#
# USAGE
#   ./intron-agent-bridge.sh install --url wss://... --key API... --secret ...
#   ./intron-agent-bridge.sh status
#   ./intron-agent-bridge.sh logs
#   ./intron-agent-bridge.sh uninstall
#
#   Credentials may also come from the environment (LIVEKIT_URL, LIVEKIT_API_KEY,
#   LIVEKIT_API_SECRET) to keep them out of shell history.
# =============================================================================

set -euo pipefail

# ---- What we create (all removable) ---------------------------------------
BRIDGE_ROOT="/opt/intron-sip-bridge"
BRIDGE_ENV="${BRIDGE_ROOT}/livekit.env"
BRIDGE_RUN="${BRIDGE_ROOT}/run-agent.sh"
BRIDGE_PY="${BRIDGE_ROOT}/bridge_agent.py"

# =============================================================================
# THE HEALTH-PORT COLLISION, and why there is a Python launcher at all.
#
# Every livekit-agents worker starts an HTTP health server. In production mode
# it binds 8081, hardcoded as a dataclass default:
#
#   port: int | ServerEnvOption[int] = ServerEnvOption(dev_default=0, prod_default=8081)
#
# The production worker already holds it. A second worker on the same host dies
# at startup with:
#
#   OSError: [Errno 98] error while attempting to bind on ('0.0.0.0', 8081)
#
# and the traceback you actually SEE is an AssertionError from the shutdown
# path, because the worker then fails again while cleaning up - so the visible
# error names neither the port nor the collision.
#
# There is no --port flag (start takes only --log-level, --url, --api-key,
# --api-secret, --drain-timeout) and no environment variable. The only way in
# is WorkerOptions(port=...), which lives in THEIR code.
#
# Rather than edit their repo, bridge_agent.py wraps livekit.agents.cli.run_app
# to set .port on the options object before delegating, then calls their
# unmodified run_agent(). ServerOptions is a plain @dataclass, so the attribute
# is writable, and their WorkerOptions construction is reused verbatim - no
# drift if they change it.
# =============================================================================
BRIDGE_HTTP_PORT="${BRIDGE_AGENT_HTTP_PORT:-8082}"

# =============================================================================
# WARM PROCESSES, and the GPU they cost.
#
# Intron's .env sets CONVOBOT_LIVEKIT_WORKER_NUM_IDLE_PROCESSES=0, so every job
# starts a process from cold and loads every model before the agent can join:
#
#   16:23:57  job request received
#   16:23:57  no warmed process available for job, waiting for one to be created
#   16:25:19  process initialized        elapsed_time: 78.13
#
# 82 seconds from dispatch to joining the room. A caller on a phone hangs up
# long before that, and a browser tester concludes the agent never came.
#
# Their outbound flow tolerates it because the launcher dials only AFTER the
# job is accepted - the callee's phone has not rung yet. Inbound is the
# opposite: the caller is already there, listening to silence.
#
# THE COST IS REAL AND IT IS ON SOMEBODY ELSE'S HARDWARE. One warm process
# holds about 6.64 GiB of a 14.56 GiB T4 permanently, where today that memory
# is free when idle. Two workers each holding one warm process would leave
# under 2 GiB spare.
#
# 1 is the right default for a working demo. Set to 0 to give the GPU back and
# accept the ~80s first-call latency.
# =============================================================================
BRIDGE_IDLE_PROCESSES="${BRIDGE_NUM_IDLE_PROCESSES:-1}"

# Where the agent points a transfer. The user part is the reserved word, not a
# number; the host is the Kamailio edge's PRIVATE address. Both are explained
# where the value is written, in write_env below.
#
# Defaulted rather than required so that `install` with no new arguments still
# does the right thing - the address is a property of this platform, not a
# per-run choice. Override with --transfer-to when the edge moves.
BRIDGE_TRANSFER_TO="${BRIDGE_SIP_TRANSFER_TO:-sip:handoff@10.70.0.10}"
UNIT_NAME="intron-sip-bridge-agent"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}.service"
LOG_FILE="/home/ubuntu/log_intron_sip_bridge_agent_stdout.log"

# ---- What we depend on and must never modify ------------------------------
APP_ROOT="/home/ubuntu/service_voice_bots"
APP_ENV="${APP_ROOT}/service_convo_bot/.env"
VENV_ACTIVATE="/data/omnivoice-venv/bin/activate"
ENTRYPOINT="run_service_convobot_worker_agent_livekit.py"
PROD_UNIT="intron_service_convobot_worker_agent_livekit.service"

RUN_USER="ubuntu"

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    WARNING: %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31m\nERROR: %s\033[0m\n' "$*" >&2; exit 1; }

need_sudo() {
  sudo -n true 2>/dev/null || die "this script needs sudo for systemd. Run 'sudo -v' first."
}

# =============================================================================
# Preflight
#
# Every check here exists because failing it later is worse. The point of
# asserting we are on the right box is that a typo'd ssh target followed by a
# systemd unit install is a bad afternoon for somebody else.
# =============================================================================
preflight() {
  say "Preflight"

  [[ "$(id -un)" == "$RUN_USER" ]] || die "run as '$RUN_USER', not '$(id -un)'."

  systemctl list-unit-files "$PROD_UNIT" --no-legend 2>/dev/null | grep -q . \
    || die "$PROD_UNIT not found. This does not look like the Intron voice box - refusing to install."
  info "found the production agent unit (it will not be touched)"

  [[ -d "$APP_ROOT" ]]       || die "application root missing: $APP_ROOT"
  [[ -f "$APP_ENV" ]]        || die "shared env missing: $APP_ENV"
  [[ -f "$VENV_ACTIVATE" ]]  || die "virtualenv missing: $VENV_ACTIVATE"
  [[ -f "${APP_ROOT}/${ENTRYPOINT}" ]] || die "entrypoint missing: ${APP_ROOT}/${ENTRYPOINT}"
  info "application, venv and entrypoint all present"

  # The production worker should be RUNNING and should stay that way. If it is
  # already down, say so now rather than letting someone blame this script.
  if systemctl is-active --quiet "$PROD_UNIT"; then
    info "production agent is active and will be left alone"
  else
    warn "production agent is NOT active. That is pre-existing - this script does not start or stop it."
  fi

  command -v nvidia-smi >/dev/null && {
    local used total
    used=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits | head -1)
    total=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits | head -1)
    info "GPU: ${used} MiB used of ${total} MiB - this worker shares it with the existing one"
  }

}

# =============================================================================
# Stop any previous bridge worker before re-installing.
#
# MUST RUN BEFORE THE PORT CHECK. Install is meant to be idempotent, and on a
# re-install the process holding our health port is OUR OWN worker from last
# time. Checking the port first turns a routine upgrade into
# "port 8082 is already in use", which reads like a conflict with something
# else on the box and invites picking a different port to work around it.
#
# Harmless on a first install - systemctl stop on an absent unit is a no-op.
# =============================================================================
stop_existing_bridge() {
  if systemctl list-unit-files "${UNIT_NAME}.service" --no-legend 2>/dev/null | grep -q .; then
    if systemctl is-active --quiet "$UNIT_NAME"; then
      info "stopping the previous bridge worker (re-install)"
      sudo systemctl stop "$UNIT_NAME"

      # systemctl stop returns once the main process is gone, but its job
      # children can hold the listening socket for a moment longer.
      local i
      for i in 1 2 3 4 5 6 7 8 9 10; do
        ss -tln 2>/dev/null | grep -q ":${BRIDGE_HTTP_PORT} " || break
        sleep 1
      done
    fi
  fi
}

# =============================================================================
# The health port.
#
# Checked before install because otherwise the failure is an AssertionError
# from the worker's shutdown path, naming neither the port nor the collision
# that caused it - see the long note at the top of this file.
# =============================================================================
check_http_port() {
  if ss -tln 2>/dev/null | grep -q ":${BRIDGE_HTTP_PORT} "; then
    local holder
    holder=$(sudo ss -tlnp 2>/dev/null | grep ":${BRIDGE_HTTP_PORT} " | head -1 | sed -E 's/.*users:\(\("([^"]+)",pid=([0-9]+).*/\1 (pid \2)/')
    die "port ${BRIDGE_HTTP_PORT} is held by ${holder:-something}. Pick another with --http-port, or: sudo ss -tlnp | grep :${BRIDGE_HTTP_PORT}"
  fi
  info "health port ${BRIDGE_HTTP_PORT} is free (production worker keeps 8081)"
}

# =============================================================================
# Reachability
#
# Checked BEFORE installing anything. A worker pointed at an unreachable server
# starts, fails to register, and restart-loops - which looks like a problem with
# the agent rather than with the URL it was given.
#
# 401 is the CORRECT answer from /rtc/validate: the server is up and demanding
# a signed token. A 200 would be surprising and a connection failure is fatal.
# =============================================================================
check_endpoint() {
  local url="$1" https_url
  https_url="${url/wss:\/\//https://}"
  https_url="${https_url/ws:\/\//http://}"

  say "Checking ${https_url}"

  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
    "${https_url}/rtc/validate" 2>/dev/null) || die "cannot reach ${https_url} - check DNS, the URL, and egress from this box."

  case "$code" in
    401|403) info "HTTP ${code} - server is up and requires a token, as expected" ;;
    200)     warn "HTTP 200 without a token. Unexpected, but not fatal." ;;
    *)       die "HTTP ${code} from ${https_url}/rtc/validate - not a healthy LiveKit server." ;;
  esac
}

# =============================================================================
# The run wrapper.
#
# THE ORDER OF THE THREE `source` LINES IS THE WHOLE POINT and it is why this
# cannot be done with systemd's Environment= alone.
#
#   1. the shared .env      database, Redis, S3, Gemini, model paths - everything
#                           this agent needs that is NOT LiveKit
#   2. the virtualenv       omnivoice, nemo, livekit-agents
#   3. OUR overrides        LAST, because the shared .env sets LIVEKIT_* too and
#                           whichever is sourced last wins
#
# The production wrapper sources the shared .env and then re-exports LIVEKIT_URL
# from it. Anything injected by systemd would be overwritten at that point, with
# no error - the worker would simply register with Intron's own server and
# quietly do nothing for us.
#
# AGENT NAME IS DELIBERATELY NOT OVERRIDDEN. It comes from the shared .env as
# intron_convobot_agent_2, and it is used as a dictionary key in the launcher's
# run-combination config. A novel name risks a KeyError somewhere in a code path
# this script has no business changing. Two workers sharing a name is harmless:
# they register with different servers.
# =============================================================================
write_run_script() {
  sudo tee "$BRIDGE_RUN" >/dev/null <<EOF
#!/bin/bash
# Managed by intron-agent-bridge.sh. Edits will be lost on reinstall.
#
# Runs the SAME agent code as the production worker, against a DIFFERENT
# LiveKit server. See the note in intron-agent-bridge.sh about source order.
set -euo pipefail

cd "${APP_ROOT}"

# 1. Shared configuration. Everything that is not LiveKit.
set -a
source "${APP_ENV}"
set +a

# 2. The virtualenv holding omnivoice, nemo and livekit-agents.
source "${VENV_ACTIVATE}"

# 3. OUR LiveKit target. LAST, so it wins over the values just sourced.
set -a
source "${BRIDGE_ENV}"
set +a

# The agent SDK reads these three from the environment directly.
export LIVEKIT_URL LIVEKIT_API_KEY LIVEKIT_API_SECRET

# Matching the production wrapper: without it the SDK's file watcher restarts
# the worker on any touched file, which under systemd is a restart loop.
export LIVEKIT_NO_RELOAD=1

# Health port for OUR worker. 8081 belongs to the production one.
export BRIDGE_AGENT_HTTP_PORT="${BRIDGE_HTTP_PORT}"

# Fail the recording call instantly rather than letting it stall the agent for
# ~23s before it joins the room. Set to 0 once an egress service is deployed.
export BRIDGE_DISABLE_EGRESS="\${BRIDGE_DISABLE_EGRESS:-1}"

echo "[bridge] starting agent against \${LIVEKIT_URL} at \$(date -Is)"

# Their entrypoint is run_service_convobot_worker_agent_livekit.py. We go
# through bridge_agent.py instead, which imports the same code and only moves
# the health port - the one setting with no CLI flag and no env var.
exec python -u "${BRIDGE_PY}" start --log-level=INFO
EOF

  sudo chmod 0755 "$BRIDGE_RUN"
  sudo chown root:root "$BRIDGE_RUN"
}

# =============================================================================
# The Python launcher.
#
# Exists for exactly one reason: to move the worker's health server off 8081,
# which the production worker owns and which has no CLI flag or environment
# variable. See the note at the top of this file.
#
# It deliberately does NOT rebuild their WorkerOptions. It wraps run_app, sets
# one attribute, and calls their unmodified run_agent() - so entrypoint_fnc,
# prewarm_fnc, agent_name, timeouts and idle-process count all stay whatever
# their code says, today and after they change it.
# =============================================================================
write_launcher() {
  sudo tee "$BRIDGE_PY" >/dev/null <<EOF
#!/usr/bin/env python
"""Managed by intron-agent-bridge.sh. Runs Intron's agent on a free health port.

Their code is imported, never modified. Two runtime patches, both for things
that have no configuration knob:

  1. WorkerOptions.port  - cannot be set from the CLI or the environment
  2. egress              - fails fast instead of stalling the agent ~23s
"""
import os
import sys

APP_ROOT = "${APP_ROOT}"
HTTP_PORT = int(os.environ.get("BRIDGE_AGENT_HTTP_PORT", "${BRIDGE_HTTP_PORT}"))
DISABLE_EGRESS = os.environ.get("BRIDGE_DISABLE_EGRESS", "1") not in ("0", "false", "False")

# python <script> puts the SCRIPT's directory on sys.path, not the working
# directory - so their package needs adding explicitly even though the unit
# sets WorkingDirectory to it.
sys.path.insert(0, APP_ROOT)

from livekit.agents import cli as _lk_cli  # noqa: E402

_original_run_app = _lk_cli.run_app


def _run_app_with_free_port(server):
    """Set the health port, then hand straight back to livekit-agents."""
    try:
        server.port = HTTP_PORT
    except Exception as exc:  # dataclass turned frozen, or renamed
        print(
            "[bridge] FATAL: could not set WorkerOptions.port (%s). The worker "
            "would bind 8081 and collide with the production agent." % exc,
            file=sys.stderr,
        )
        raise

    print("[bridge] agent health server on port %d" % HTTP_PORT, flush=True)
    return _original_run_app(server)


# Their module holds a reference to the cli MODULE and resolves run_app at call
# time, so patching the attribute works regardless of import order.
_lk_cli.run_app = _run_app_with_free_port


# ---------------------------------------------------------------------------
# Egress: fail immediately instead of stalling the call.
#
# THIS IS WHY THE AGENT APPEARED NOT TO JOIN.
#
# livekit_entrypoint() calls start_room_composite_egress() to record the
# session, unconditionally - there is no config flag - and it does so BEFORE
# ctx.connect(). Their code correctly treats a failure as non-fatal, but the
# failure takes about 23 SECONDS to arrive, because livekit-server waits for
# an egress worker to answer over Redis before giving up with a 503.
#
# Observed on the first real test:
#     16:17:10  entrypoint starts
#     16:17:33  egress fails            <- 23s of nothing
#     16:17:38  GREETING_WAIT_TIMEOUT: no participant_connected in 5s
#
# So the caller sits in a silent room for 23 seconds while the agent waits on
# a recording service that is not deployed. Anyone testing gives up and
# reloads, which starts another room and another GPU process - which is how
# the CUDA OOM in that same log happened.
#
# Raising here turns 23 seconds into microseconds. Their except block catches
# it, logs "non-fatal, continuing without recording", and moves straight on to
# ctx.connect().
#
# TO ENABLE RECORDING LATER: deploy an egress service alongside the LiveKit
# server, give it the S3 credentials, and set BRIDGE_DISABLE_EGRESS=0. Note
# that room-composite egress runs headless Chrome even for audio_only, so it
# wants considerably more CPU and RAM than a t3.small has. Recording clinical
# audio is also a decision to take deliberately rather than by leaving a
# default on.
# ---------------------------------------------------------------------------
if DISABLE_EGRESS:
    from livekit.api import egress_service as _lk_egress  # noqa: E402

    async def _egress_disabled(self, *args, **kwargs):
        raise RuntimeError(
            "egress disabled on the Intron SIP bridge: no egress service is "
            "deployed, and letting the request time out blocks the agent for "
            "~23s before it joins the room. Set BRIDGE_DISABLE_EGRESS=0 once "
            "an egress service exists."
        )

    _lk_egress.EgressService.start_room_composite_egress = _egress_disabled
    print("[bridge] egress disabled (fails fast; recording is off)", flush=True)

from service_convo_bot.service_convobot_libs_worker_agent_livekit import (  # noqa: E402
    run_agent,
)

if __name__ == "__main__":
    run_agent()
EOF

  sudo chmod 0755 "$BRIDGE_PY"
  sudo chown root:root "$BRIDGE_PY"
}

write_env() {
  local url="$1" key="$2" secret="$3" idle="${4:-1}" transfer_to="${5:-}"

  # 0600 and owned by the running user. This file holds a signing secret that
  # grants room access on our platform.
  sudo tee "$BRIDGE_ENV" >/dev/null <<EOF
# Managed by intron-agent-bridge.sh. Intron SIP platform LiveKit credentials.
#
# Sourced AFTER service_convo_bot/.env so these override its values.
# Rotating means updating this file and: sudo systemctl restart ${UNIT_NAME}
LIVEKIT_URL=${url}
LIVEKIT_API_KEY=${key}
LIVEKIT_API_SECRET=${secret}

# Keep a process warm so an inbound caller is not held for ~80s while models
# load. Their .env sets this to 0, which suits outbound (the callee's phone
# has not rung yet) and not inbound (the caller is already listening).
#
# Costs roughly 6.64 GiB of the T4, held permanently. Set to 0 to give it back.
CONVOBOT_LIVEKIT_WORKER_NUM_IDLE_PROCESSES=${idle}

# ---------------------------------------------------------------------------
# AGENT-INITIATED TRANSFER.
#
# Their agent already has the whole mechanism - a manage_call tool, a
# TransferSIPParticipant call, the lot. It was switched off for our calls by
# two values in their .env, and these two lines are the entire fix:
#
#   CONVOBOT_SIP_TRANSFER_DIRECTIONS=OUTBOUND   excluded every call we send
#   CONVOBOT_SIP_TRANSFER_TO=                   empty, so no destination
#
# With those in force the agent would say "please hold while I transfer you",
# call its own tool, get back "SIP transfer is not enabled for INBOUND calls",
# and apologise to the caller. Nothing reached our platform - no REFER was
# ever sent.
#
# WHY OVERRIDE RATHER THAN ASK THEM TO CHANGE IT. Their value is right for
# their deployment: transfer was built for their outbound dialers, and inbound
# did not exist until this platform did. Editing their .env would change
# behaviour for the production worker too. This file is sourced after theirs
# and read only by our unit, so their outbound calls keep exactly the setting
# they have today.
#
# INBOUND,OUTBOUND rather than INBOUND: an empty list means "allow all" in
# their config.py, and this platform originates calls as well as receiving
# them. Naming both is explicit about what is permitted instead of relying on
# a default that reads as "unset".
CONVOBOT_SIP_TRANSFER_DIRECTIONS=INBOUND,OUTBOUND

# The reserved word this platform answers to, NOT a phone number.
#
# It says THAT a transfer is wanted; where it goes is resolved from the
# tenant's handoff targets once the call re-enters - see docs/API.md,
# Contract 6, and ctrl.IsTransferDestination. An agent that named a number
# directly would be choosing a destination the tenant has not authorised.
#
# THE HOST MUST BE THE EDGE'S PRIVATE ADDRESS. If FreeSWITCH dials the
# Refer-To rather than looking it up in the dialplan, it sends that INVITE
# itself - and the edge security group admits the farm by security-group
# reference, which only matches a private source. The Elastic IP would leave
# through the NAT gateway and be dropped, which looks like a transfer that
# silently never connects.
CONVOBOT_SIP_TRANSFER_TO=${transfer_to}
EOF

  sudo chmod 0600 "$BRIDGE_ENV"
  sudo chown "${RUN_USER}:${RUN_USER}" "$BRIDGE_ENV"
}

# =============================================================================
# The unit.
#
# Modelled on the production unit - same user, same working directory, same
# append-to-a-file logging - so anyone who knows that one can read this one.
#
# ONE DELIBERATE DIFFERENCE: no StartLimitAction=reboot. The production unit
# reboots the box if the agent fails repeatedly, which is a reasonable choice
# for the service that IS the box's purpose. It is an unreasonable thing for a
# bolt-on integration to do to somebody else's machine. This one gives up and
# sits in `failed` instead.
# =============================================================================
write_unit() {
  sudo tee "$UNIT_PATH" >/dev/null <<EOF
# Managed by intron-agent-bridge.sh. Edits will be lost on reinstall.
[Unit]
Description=Intron SIP platform agent bridge (second LiveKit worker)
Documentation=https://github.com/Adebayobenjamin/intron-sip-project
After=network-online.target
Wants=network-online.target

[Service]
User=${RUN_USER}
Group=${RUN_USER}
WorkingDirectory=${APP_ROOT}
ExecStart=/bin/bash ${BRIDGE_RUN}

Restart=always
RestartSec=10

# Stop after 5 failures in 10 minutes. NOT StartLimitAction=reboot: the
# production unit reboots the box, which is not this integration's call to make.
StartLimitIntervalSec=600
StartLimitBurst=5

StandardOutput=append:${LOG_FILE}
StandardError=append:${LOG_FILE}

[Install]
WantedBy=multi-user.target
EOF

  sudo chmod 0644 "$UNIT_PATH"
}

# =============================================================================
# Install
# =============================================================================
do_install() {
  local url="${LIVEKIT_URL:-}" key="${LIVEKIT_API_KEY:-}" secret="${LIVEKIT_API_SECRET:-}"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --url)       url="${2:?--url needs a value}"; shift 2 ;;
      --key)       key="${2:?--key needs a value}"; shift 2 ;;
      --secret)    secret="${2:?--secret needs a value}"; shift 2 ;;
      --http-port) BRIDGE_HTTP_PORT="${2:?--http-port needs a value}"; shift 2 ;;
      --idle-processes) BRIDGE_IDLE_PROCESSES="${2:?--idle-processes needs a value}"; shift 2 ;;
      --transfer-to) BRIDGE_TRANSFER_TO="${2:?--transfer-to needs a value}"; shift 2 ;;
      *) die "unknown option: $1" ;;
    esac
  done

  [[ -n "$url" ]]    || die "missing --url (or LIVEKIT_URL). Expected wss://livekit-beta.intron.quiverlearn.com"
  [[ -n "$key" ]]    || die "missing --key (or LIVEKIT_API_KEY)"
  [[ -n "$secret" ]] || die "missing --secret (or LIVEKIT_API_SECRET)"

  [[ "$url" == wss://* || "$url" == ws://* ]] \
    || die "--url must start with wss:// - an https:// URL works for REST calls and fails when the worker registers, with an error about the protocol rather than the scheme."

  # Validated here because their normaliser fails QUIETLY. It returns "" for
  # anything it does not recognise, and the agent only reports "SIP transfer
  # destination is not configured or invalid" at the moment a caller asks to
  # be transferred - so a typo installs cleanly, runs for days, and surfaces
  # as an apology to a caller. These three forms are what
  # util_normalize_sip_transfer_destination accepts.
  if [[ ! "$BRIDGE_TRANSFER_TO" =~ ^(sip:[A-Za-z0-9._~%+-]+@[A-Za-z0-9.-]+(:[0-9]{1,5})?(\;.+)?|tel:\+[1-9][0-9]{7,14}|\+[1-9][0-9]{7,14})$ ]]; then
    die "--transfer-to is not a form the agent accepts: '${BRIDGE_TRANSFER_TO}'. Use sip:user@host, tel:+E164 or +E164."
  fi

  # Preflight BEFORE need_sudo, deliberately. Every check in preflight is
  # read-only, and the first of them is "are we even on the right machine".
  # Asking for sudo first means a wrong-host run fails with "this script needs
  # sudo", which sends someone to fix their sudo access when the actual problem
  # is that they are on the wrong box.
  preflight
  need_sudo

  # Order matters: stop our previous worker, THEN check the port. Reversed,
  # a re-install fails on a port its own predecessor is holding.
  stop_existing_bridge
  check_http_port

  check_endpoint "$url"

  say "Installing"
  sudo mkdir -p "$BRIDGE_ROOT"
  sudo chmod 0755 "$BRIDGE_ROOT"
  write_env "$url" "$key" "$secret" "$BRIDGE_IDLE_PROCESSES" "$BRIDGE_TRANSFER_TO"
  info "wrote ${BRIDGE_ENV} (0600)"
  if [[ "$BRIDGE_IDLE_PROCESSES" -gt 0 ]]; then
    info "keeping ${BRIDGE_IDLE_PROCESSES} process warm (~6.6 GiB of GPU each, held permanently)"
  else
    info "no warm process - first call waits ~80s for models to load"
  fi
  write_launcher;                     info "wrote ${BRIDGE_PY} (health port ${BRIDGE_HTTP_PORT})"
  write_run_script;                   info "wrote ${BRIDGE_RUN}"
  write_unit;                         info "wrote ${UNIT_PATH}"

  sudo touch "$LOG_FILE"
  sudo chown "${RUN_USER}:${RUN_USER}" "$LOG_FILE"

  sudo systemctl daemon-reload
  sudo systemctl enable "$UNIT_NAME" >/dev/null 2>&1
  sudo systemctl restart "$UNIT_NAME"

  say "Waiting for the worker to register (up to 90s)"
  info "first start loads models and can be slow"

  local i
  for i in $(seq 1 30); do
    if grep -qiE "registered worker|worker registered|registered with" "$LOG_FILE" 2>/dev/null; then
      say "Registered."
      grep -iE "registered worker|worker registered|registered with" "$LOG_FILE" | tail -2
      break
    fi
    if ! systemctl is-active --quiet "$UNIT_NAME"; then
      say "The unit stopped. Last 30 lines:"
      tail -30 "$LOG_FILE" 2>/dev/null || true
      die "worker failed to start. Nothing else on this box was modified; 'uninstall' removes it."
    fi
    sleep 3
    [[ $i -eq 30 ]] && warn "no registration line after 90s. It may still be loading - check: $0 logs"
  done

  # Re-assert the thing this script promises, as an observed fact rather than
  # an intention.
  say "Production services (unchanged)"
  systemctl is-active "$PROD_UNIT" | sed 's/^/    production agent: /'
  systemctl is-active opensips 2>/dev/null | sed 's/^/    opensips:         /' || true

  say "Done"
  info "status:    $0 status"
  info "logs:      $0 logs"
  info "remove:    $0 uninstall"
}

do_status() {
  say "Bridge worker"
  systemctl status "$UNIT_NAME" --no-pager --lines=0 2>/dev/null || info "not installed"
  say "Production services (this script never modifies these)"
  systemctl is-active "$PROD_UNIT"  | sed 's/^/    production agent: /'
  systemctl is-active opensips 2>/dev/null | sed 's/^/    opensips:         /' || true
  command -v nvidia-smi >/dev/null && {
    say "GPU"
    nvidia-smi --query-gpu=memory.used,memory.total,utilization.gpu --format=csv
  }
  [[ -f "$BRIDGE_ENV" ]] && {
    say "Target"
    sudo grep '^LIVEKIT_URL=' "$BRIDGE_ENV" | sed 's/^/    /'
  }
}

do_logs() { tail -f "$LOG_FILE"; }

# =============================================================================
# Uninstall.
#
# Removes everything this script created and nothing else. Worth being able to
# say that plainly to the team whose box this is.
# =============================================================================
do_uninstall() {
  need_sudo
  say "Removing the bridge worker"

  sudo systemctl disable --now "$UNIT_NAME" 2>/dev/null || true
  sudo rm -f "$UNIT_PATH"
  sudo systemctl daemon-reload
  sudo rm -rf "$BRIDGE_ROOT"
  info "removed the unit and ${BRIDGE_ROOT}"
  info "left in place: ${LOG_FILE} (delete by hand if you want it gone)"

  say "Production services (untouched throughout)"
  systemctl is-active "$PROD_UNIT" | sed 's/^/    production agent: /'
  systemctl is-active opensips 2>/dev/null | sed 's/^/    opensips:         /' || true
}

case "${1:-}" in
  install)   shift; do_install "$@" ;;
  status)    do_status ;;
  logs)      do_logs ;;
  uninstall) do_uninstall ;;
  *)
    sed -n '3,50p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
