# Roadmap

The full build, in the order it has to happen, from an empty AWS account to a
platform carrying a thousand concurrent calls.

Two phases, answering different questions.

**Phase 1 — does a call work?** A caller reaches the AI agent, can be handed to
a human, and the conversation is recorded according to policy — for any
combination of trunk and handoff target.

**Phase 2 — does it work a thousand times at once**, without paying for a
thousand at three in the morning?

Each milestone has a **gate**: something observable, not a judgement call. The
failures on this platform are mostly silent — a call that sets up perfectly and
has no audio, a security group that drops packets before anything logs them —
so "it seems to work" is not a state worth recording.

Phase 2 should not begin before **M16**. Autoscaling policies are written
against a traffic shape, and that shape does not exist until real calls do.

---

## Phase 1 — one call, every combination, correct

### Foundation

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M1** | **State and secrets** | Terraform state bucket — versioned, encrypted, TLS-only, native S3 locking. Secret containers created empty; Terraform owns the container, never the value. | `terraform plan` locks and releases · every secret ARN exists · no credential anywhere in state | S |
| **M2** | **Network** | One VPC. Subnets across two AZs: edge (public), sip, app, data (private). NAT for egress, S3 gateway endpoint. | Private subnets reach the internet via NAT · S3 endpoint on every private route table · no SSH anywhere | S |
| **M3** | **Compute and identity** | Edge, FreeSWITCH, app and LiveKit instances. Security groups per tier. Instance roles scoped to one secret each. `key_name = null` throughout — SSM is the only way in. | Every box reachable by `make ping` · each tier can read **only** its own secret · port 22 open nowhere | M |

### SIP plane

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M4** | **Edge — Kamailio + RTPEngine** | Advertised address (`advertise` / `private!public`), kernel-mode media forwarding, dispatcher sets, the source-address branch separating inbound from egress. | Kamailio advertises the Elastic IP, not `10.70.x` · `xt_RTPENGINE` loaded and the table bound · ng socket answers a `ping` with `pong` | M |
| **M5** | **Farm — FreeSWITCH** | Built from source with a pinned module list. Session limits, codecs, the internal profile, the dialplan: hand over **pre-answer**, export `sip_route_uri`, socket to the controller. | 19 modules present · SIP and ESL listening · ESL ACL admits the app subnet and refuses everything else · dispatcher shows the node `AP` | M |

### Data and services

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M6** | **Database** | RDS Postgres. DSN composed out-of-band from the RDS-managed secret and merged into the containers that need it — never through Terraform. | Both services' containers hold an identical DSN · reachable from the app subnet and nowhere else | S |
| **M7** | **The data model** | `tenants`. `trunks` carrying the connection **and** its DID inventory as a multirange — `tenant_id NULL` is the platform trunk, set is BYOT. `traffic_configs` holding trunk, number range, direction, destination and `recording_mode`. `handoff_targets` with a tenant default and an optional per-line override. Exclusion constraint so two active configs cannot claim one number. | All four trunk × handoff combinations resolve · Postgres **refuses** an overlapping claim · a tenant on the platform trunk can use an E.164 handoff | **L** |
| **M8** | **Backend services** | `control-plane` (REST over Postgres, owns migrations) and `esl-controller` (per-call socket listener, ESL pool, originate worker, sweeper). Admission resolved **in-process against Postgres** — no HTTP in the call path. | Both containers healthy on separate ports · `GET /v1/route` answers for a seeded number · `:8022` accepts a connection | **L** |

### Integration

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M9** | **LiveKit** | Server, Redis, TLS termination and `livekit-sip` on one host. An inbound trunk restricted to the edge, and a dispatch rule that summons the agent by name. | Trunk and dispatch rule exist · Kamailio's set 2 probes it `AP` · two independent locks on who may present a call | M |
| **M10** | **AI agent** | Five configuration values and one code change on the agent worker — see [ARCHITECTURE.md §8](ARCHITECTURE.md). Transfer enabled for inbound, a warm process, egress gated. **Not ours to merge.** | The worker registers against our LiveKit and shows as available | S (ours) |
| **M11** | **Carrier** | Twilio trunk. Origination URI pointing at the edge, Termination IP ACL admitting it, DIDs assigned. Signalling ranges for **all** regional edges in the security group. | A dialled DID produces an INVITE visible on the edge — even if the call is then rejected | S |
| **M12** | **Handoff target** | A disposable Asterisk PBX standing in for a client contact centre, reached over the public path so the media crosses a NAT boundary as a real SBC would. | A softphone registers and the handoff URI resolves | S |

### Proving it

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M13** | **First AI call** | Seed a tenant, trunk, traffic config and DID. Dial in. | **The bot answers and you hear it.** This proves the entire inbound path at once | S |
| **M14** | **Handoff** | Both kinds: a SIP URI at an SBC, and an E.164 dialled back out over the trunk. | The human's phone rings · two-way audio · **the caller stays connected across the leg swap** | M |
| **M15** | **Outbound** | `POST /v1/originate` end to end — from-DID validated and owned, trunk taken from the same row. | The destination rings, presenting the correct caller ID | M |
| **M16** | **Recording and the matrix** | `record_session` to the recordings volume, upload, `recording_url` on the CDR. Then walk every combination: their trunk / ours × SIP URI / E.164 × each recording mode. | A `.wav` lands in storage · `ai_only` demonstrably **stops** at handoff and `full` does not · every combination completes | M |

---

## Phase 2 — a thousand concurrent, elastically

| # | Milestone | What it covers | Gate | Size |
|---|---|---|---|---|
| **M17** | **Measure** | Load-test each tier with real media and find the knee. Confirm the per-call GPU cost with Intron. Until this exists every capacity number is reasoning, not data. | A **measured** limit per tier | M |
| **M18** | **Remove the ceilings** | `max-sessions` — a call is two channels, so 2000 is a cliff at exactly 1000 calls. The single-node LiveKit validation rule. Every burstable instance on a sustained path. | No hard limit sits below the target | S |
| **M19** | **Scale the farm** | FreeSWITCH in an autoscaling group. Scale-out is nearly free — the dispatcher already round-robins and probes. Scale-in needs a lifecycle hook: mark inactive, wait for zero channels, terminate. | A node drains and terminates **without dropping a call** | M |
| **M20** | **Scale LiveKit** | Multi-node with Redis, `livekit-sip` alongside. | Same drain gate, per node | M |
| **M21** | **Scale the controller** | Behind a network load balancer, draining on shutdown. One socket per live call, so scale-in must wait them out. | A controller is removed while another carries calls | M |
| **M22** | **Database** | Aurora Serverless v2 — the only tier whose compute genuinely autoscales. | Scales under load without exhausting connections | S |
| **M23** | **The edge** | **The hard one.** The Elastic IP is what carriers address, so adding capacity means new origination URIs at the carrier and every BYOT client re-allowlisting. | Two edges serving; losing one does not lose inbound | **L** |
| **M24** | **Agent capacity** | At roughly 6.6 GiB of VRAM per concurrent call, a thousand calls is hundreds of GPUs. This is an architecture decision — hosted inference — not a sizing exercise. **Not ours, and it dominates cost.** | A thousand concurrent sessions without local VRAM as the ceiling | **L** |
| **M25** | **Autoscaling and observability** | Policies, dashboards, alerting. Scale on **concurrent channels, not CPU** — CPU on a media box is a poor proxy and will scale at the wrong moment. | Capacity tracks load unattended, and scale-in never drops a call | M |

---

## Two things worth being blunt about

**M24 decides whether a thousand is reachable at all.** Everything from M17 to
M23 is tractable engineering on infrastructure under our control. The GPU
ceiling is neither. At the current per-call cost the SIP tier would idle at a
few thousand a month while inference ran into six figures — which is also why
it is where elasticity is worth real money, and where self-hosting fights it
hardest.

**Scale-in is where the bugs are.** Every tier can add capacity in minutes;
none can remove it without draining, and draining takes as long as the longest
call. Scale-in faults do not appear in testing — they appear as dropped live
calls when a node terminates with conversations still on it. That is why
M19–M21 each gate on *draining*, not on *scaling*.
