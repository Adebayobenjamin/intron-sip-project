# The LiveKit leg

How a caller on this platform ends up talking to Intron's AI agent: what runs
where, how the pieces connect, and the traps found while building it.

> **This replaces an earlier version of this document** that described our
> Kamailio dialling Intron's own LiveKit over the public internet, with SIP-TLS
> on 5061 and SRTP required. That design was abandoned. Nothing below depends on
> it; if you find a reference to it elsewhere, it is stale.

---

## The short version

We run **our own LiveKit server** in our AWS account (eu-west-1). Our
Kamailio sends the AI leg of a call to it over a **private** SIP leg inside the
VPC. The **AI models stay on Intron's GPU box** in their own account
(eu-west-2): a worker process there connects *out* to our LiveKit server and
waits to be given calls.

```
 caller ─▶ carrier ─▶ Kamailio ─▶ FreeSWITCH ─▶ esl-controller
                         ▲                          │ bridges the B-leg
                         └──────────────────────────┘ back out via Kamailio
                         │
                         │ SIP + RTP, private, UDP 5060
                         ▼
          ┌──── our LiveKit box (10.70.0.30, eu-west-1) ────┐
          │  livekit-sip ──▶ livekit-server ◀── Caddy (TLS)    │
          └──────────────────────────▲─────────────────────────┘
                                     │ WebRTC + wss://, over the internet
                                     │ (the agent connects OUT to us)
          ┌──── Intron's GPU box (g4dn.xlarge, eu-west-2) ─────┐
          │  intron-sip-bridge-agent  ─ STT / LLM / TTS         │
          └─────────────────────────────────────────────────────┘
```

Nothing connects *into* Intron's box. Their production service, their SIP
proxy and their outbound dialers are untouched.

---

## Why not use Intron's existing LiveKit

Intron already runs LiveKit on the same GPU box (Calls05). It could not take
our calls without changing things that carry their live traffic:

- It had **no inbound trunk and no dispatch rule**. It has only ever made
  outbound calls.
- Its SIP proxy (OpenSIPS) accepts calls only from a **hardcoded list** of
  carrier IPs, and ours is not on it.
- Its SIP leg is **plain UDP with encryption disabled**, and reaching it meant
  crossing the internet between two AWS accounts with patient audio.

What made our own server cheap is that **a LiveKit agent is a client**. It
opens a WebSocket out to whichever server its environment names. Pointing a
second worker at our server needed no firewall change on their side.

---

## What runs where

### Our LiveKit box

| | |
|---|---|
| Terraform | `infra/iac/stacks/livekit/` |
| Ansible | `infra/ansible/roles/livekit/`, `playbooks/livekit.yml`, `group_vars/livekit.yml` |
| Instance | `t3.small`, no GPU. Private `10.70.0.30`, Elastic IP `3.69.195.246` |
| Public names | `livekit-beta.intron.quiverlearn.com`, and `livekit-beta-turn.…` for TURN |
| Credentials | Secrets Manager `intron-sip/beta/livekit` (`LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`) |

Four containers under one systemd unit, `intron-livekit`:

| Container | Job |
|---|---|
| `livekit-server` v1.13.7 | Rooms and the API. Forwards media; runs no models. |
| `livekit-sip` v1.15.0 | Turns a SIP call into a room participant. |
| `redis` | LiveKit's state, **including the SIP trunk and dispatch rule**. Persisted to disk. |
| `caddy` (caddyl4) v2.11.3 | Gets the TLS certificate and splits port 443 between the API and TURN. |

No GPU because LiveKit only forwards packets. It does not decode audio or run
a model.

### Intron's box

| | |
|---|---|
| Host | `18.134.42.219`, `g4dn.xlarge` (Tesla T4), eu-west-2, **their account** |
| Installed by | `infra/bin/intron-agent-bridge.sh` (run on their box) |
| Our files | `/opt/intron-sip-bridge/` and one unit, `intron-sip-bridge-agent` |
| Log | `/home/ubuntu/log_intron_sip_bridge_agent_stdout.log` |

It runs **their agent code, unmodified**, as a second worker registered to our
server under the name `intron_convobot_agent_2`. `./intron-agent-bridge.sh
uninstall` removes everything the script created.

---

## How a call reaches the agent

1. A caller dials one of our numbers. The carrier sends it to Kamailio.
2. Kamailio passes it to FreeSWITCH, which hands it to the esl-controller.
3. The controller admits the call and bridges a second leg (the "B-leg") back
   out through Kamailio, tagged `X-Intron-Egress: livekit`.
4. Kamailio sends it to dispatcher set 2: `sip:10.70.0.30:5060`, plain UDP.
5. `livekit-sip` accepts it on the inbound trunk, which only allows calls from
   Kamailio's private address `10.70.0.10`.
6. The dispatch rule creates a room named **`intron_sip__<caller E.164>`** and
   sends the job to `intron_convobot_agent_2`.
7. Intron's worker, already connected to our server, joins the room. The
   caller and the agent now hear each other.

The agent can hand the caller to a human with a SIP REFER on that same leg.
See [API.md, Contract 6](API.md#contract-6--agent-initiated-transfer-over-sip-refer).

---

## The contract with Intron's agent code

Their code makes assumptions. Each of these is set on our side to match them;
none needed a change in their repo.

**The agent name is `intron_convobot_agent_2`.** Their worker sets a name,
which turns off LiveKit's automatic dispatch. So our dispatch rule names the
agent explicitly (`room_config.agents`). Without that, the caller lands in a
room with no agent in it, and nothing logs an error.

**The room name carries the caller's number.** Their agent reads
`room_name.split("__")[1]` as the phone number. The dispatch rule uses the
prefix `intron_sip_` with `no_randomness: true`, and LiveKit adds its own `_`,
giving `intron_sip__+2348012345678`. **Verified on real calls**: the agent log
shows the parsed numbers. If a room name ever changes shape, the agent still
answers but records the caller as `unknown`.

**Settings we override, and how.** Our worker loads their `.env` first and our
`/opt/intron-sip-bridge/livekit.env` last, so ours wins for every key it sets:

| Key | Ours | Why |
|---|---|---|
| `LIVEKIT_URL`, `_API_KEY`, `_API_SECRET` | our server | Register with us, not them. |
| `CONVOBOT_LIVEKIT_WORKER_NUM_IDLE_PROCESSES` | `1` | Theirs is `0`. See [cost](#capacity-and-cost). |
| `CONVOBOT_SIP_TRANSFER_DIRECTIONS` | `INBOUND,OUTBOUND` | Theirs allows transfer on outbound calls only. |
| `CONVOBOT_SIP_TRANSFER_TO` | `sip:handoff@10.70.0.10` | A reserved word our platform resolves to the tenant's handoff target. |

Systemd `Environment=` would not work here: their own start-up script reloads
`.env` and would overwrite it. Loading our file last is the fix.

Everything else (database, Redis, S3, Gemini, model paths) comes from their
`.env`. **So our AI leg depends on their services being up.**

---

## Network

| Port | From | What |
|---|---|---|
| 443/tcp | internet | API, WebSocket, TURN over TLS (Caddy, split by hostname) |
| 80/tcp | internet | Let's Encrypt certificate renewal only |
| 50000–60000/udp | internet | WebRTC media to the agent and browsers |
| 7881/tcp | internet | WebRTC over TCP, for networks that block UDP |
| 3478/udp | internet | TURN requests |
| 30000–40000/udp | internet | TURN relay media |
| **5060/udp** | **Kamailio's security group only** | SIP from our platform |
| **10000–20000/udp** | **Kamailio's security group only** | RTP from RTPEngine |

**The internet-facing ports are open to everyone on purpose.** The clients are
a browser and Intron's worker behind a NAT address nobody controls. Access is
controlled by the API secret: every connection needs a token signed with it.
Protect the secret, not the port list.

**The SIP leg is plain UDP with no encryption, on purpose.** It never leaves
our VPC and only Kamailio's security group can reach it. The TLS and SRTP in
the old design existed to protect an internet hop that no longer exists. If
this leg ever crosses the internet again, set `livekit_sip_transport: tls` and
`livekit_srtp_required: true` together, in both `group_vars/all.yml` and
`stacks/sip-core/beta/main.tf`.

**Kamailio advertises its private address on this leg.** Otherwise the agent's
BYE and REFER go to the Elastic IP, loop out through the internet and get
dropped. See [API.md, Contract 6](API.md#contract-6--agent-initiated-transfer-over-sip-refer).

---

## Operating it

### Deploy order

```bash
cd infra/iac
make populate-iac ENV=beta          # livekit_cidrs = ["10.70.0.30/32"]
make apply STACK=sip-core ENV=beta  # edge side of the SIP leg
make apply STACK=livekit  ENV=beta  # the LiveKit box and its ports

cd ../ansible
make play PLAYBOOK=livekit ENV=beta # containers, trunk, dispatch rule
make play PLAYBOOK=edge    ENV=beta # Kamailio dispatcher → 10.70.0.30:5060
```

First time only: the zone `intron.quiverlearn.com` is delegated from
Cloudflare (which hosts `quiverlearn.com`; Namecheap is only the registrar)
with four NS records. Get them from
`make output STACK=livekit ENV=beta EXTRA="livekit_zone_nameservers"`. Until
they exist there is no certificate, and the failure looks like a TLS error.

Then on Intron's box, from your laptop:

```bash
scp -i ~/.ssh/intron-voicebot-deploy-2026.pem \
  infra/bin/intron-agent-bridge.sh ubuntu@18.134.42.219:~/

infra/bin/aws-env-run intron-sip/beta/livekit -- bash -c '
ssh -i ~/.ssh/intron-voicebot-deploy-2026.pem ubuntu@18.134.42.219 \
  "LIVEKIT_URL=wss://livekit-beta.intron.quiverlearn.com \
   LIVEKIT_API_KEY=$LIVEKIT_API_KEY LIVEKIT_API_SECRET=$LIVEKIT_API_SECRET \
   ./intron-agent-bridge.sh install"'
```

It refuses to run unless it finds their production agent, stops our previous
worker before reinstalling, and prints their services' status before and after.

### Checking it works

| Check | Where | Expect |
|---|---|---|
| `kamcmd dispatcher.list` | edge | set 2 shows `AP`. `IP` means no reply to probes (every 10s). |
| `lk sip inbound list` / `lk sip dispatch list` | LiveKit box | one trunk and one rule, both named `intron-sip-inbound` |
| `./intron-agent-bridge.sh status` | Intron's box | unit active; production agent and OpenSIPS active |
| `registered worker` in the bridge log | Intron's box | `agent_name: intron_convobot_agent_2` |

`lk` on the LiveKit box needs `HOME` and the API key in the environment. The
provisioning script, `/etc/intron/livekit/sip-provision.sh`, sets both and is
safe to re-run.

### Testing without a phone

`livekit-examples/agent-starter-react` (the hosted Agents Playground now only
works with LiveKit Cloud). Put `LIVEKIT_URL`, the key and secret, and
`AGENT_NAME=intron_convobot_agent_2` in `.env.local`, then `pnpm dev`. Join
once and wait; don't reload, since every reload starts a new room and a new GPU
process.

---

## Capacity and cost

**One warm agent process holds about 6.7 GiB of the T4's 15 GiB, all the
time.** That is the cost of `NUM_IDLE_PROCESSES=1`. With `0`, the first call
waits about **80 seconds** for the models to load, which for a caller already on
the line is a failed call. Their outbound dialer can use `0` because the phone
doesn't ring until the agent is ready.

**Practical limit: 1 concurrent AI call, 2 at a squeeze.** A second call starts
another ~6.7 GiB process; a third runs out of GPU memory and fails rather than
waiting in a queue. There is no GPU-aware limit to prevent that.

**AWS cost:** the GPU box is Intron's and billed per hour regardless, so the
warm process costs them headroom, not money. Our LiveKit box is about
**$17.50/month** (t3.small, disk, the Route53 zone). Internet transfer for the
agent's audio is roughly $0.002 per call-hour on each side.

Tell Intron about the 6.7 GiB before they find it in `nvidia-smi`.
`--idle-processes 0` gives it back.

---

## Things that fail silently

Every one of these was hit while building this. Most produce a call that
connects and then has no audio, or a play that passes while something is
broken.

**Advertised address, both directions.** `livekit-server` must advertise the
Elastic IP (`use_external_ip: true`) because the agent and browsers reach it
from the internet. `livekit-sip` must advertise the **private** address
(`use_external_ip: false`) because only RTPEngine, inside the VPC, talks to it.
Swap either and the call connects with no audio.

**Port ranges live in two files.** Terraform opens them; `livekit.yaml` and
`sip.yaml` bind them. Every row in the [network table](#network) must match
on both sides. The TURN relay range
(30000–40000) was missed at first: TURN then accepts the request and relays
into a closed port, failing only for callers on locked-down networks.

**Redis must persist.** The SIP trunk and dispatch rule live only in Redis. With
persistence off, every restart deleted them and the next play recreated them
with new IDs, which looked like normal provisioning. Test with
`systemctl restart intron-livekit`: the IDs must not change.

**A failed play never runs its handlers.** The next run then sees nothing to
change and restarts nothing. The role now checks that `livekit-sip` is
actually running and listening, instead of relying on change notifications.

**Server older than the browser client.** `livekit-server` v1.9 rejected every
browser token with `unknown field "tags"`, which looks like a bad secret. A
newer server accepts older clients; the reverse fails. Keep the server image
current.

**The `lk` request shapes.** `lk sip inbound create` wants `{"trunk": {...}}`
and `lk sip dispatch create` wants `{"dispatch_rule": {...}}`. Without the
wrapper you get `unknown field "name"` or `missing rule`. `lk --curl` shows the
request it would build, **and also sends it**.

**Two workers, one health port.** Every agent worker binds port 8081 and
there's no setting for it. The second one dies with an `AssertionError` from
its shutdown code that never mentions the port. The bridge moves ours to 8082.

**Recording stalls the agent for 23 seconds.** Their code starts a recording
before joining the room. We don't run LiveKit's recording service (egress), so
that request waited 23 seconds to fail while the caller heard silence. The
bridge makes it fail immediately. **Recording is off.**

---

## Known limits and open work

- **Recording is off.** Turning it on needs LiveKit's egress service (which
  runs headless Chrome, too heavy for a t3.small), the S3 credentials, and a
  deliberate decision about storing patient audio. Then set
  `BRIDGE_DISABLE_EGRESS=0`.
- **GPU capacity is 1–2 concurrent calls,** on hardware we don't own.
- **Our AI leg depends on Intron's database, Redis, S3 and API keys.**
- **The room-name contract is fragile.** Passing the caller's number in
  dispatch metadata would be cleaner, but their code only reads metadata on
  outbound calls, so it needs a change on their side.
- **Transfer has open questions** (how FreeSWITCH handles the REFER, what the
  caller hears, no-answer behaviour). See
  [API.md, Contract 6](API.md#contract-6--agent-initiated-transfer-over-sip-refer).

The long-term fix for most of these is running the agent on a GPU instance in
**our** account and region, using their code and models. The media path would
then never leave AWS eu-west-1, and capacity would be ours to size.
