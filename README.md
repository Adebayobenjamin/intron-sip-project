# Intron SIP Infrastructure — beta

A self-hosted SIP platform that puts a caller on a phone line into a LiveKit
room with an AI agent, and can hand the call off to a human contact centre.

Terraform for the AWS resources, Ansible for what runs on them. No GitOps —
both are driven from a laptop via `make`.

```
docs/
├── ROLLOUT.md   stage-by-stage bring-up, with a gate per stage
├── LIVEKIT.md   the AI leg: our LiveKit server and Intron's agent worker
└── API.md       Phase 3: the two backend services and their contracts
infra/
├── bin/         aws-env-run, env-file-to-sm, db-dsn-to-sm
├── iac/         Terraform. stacks/<stack>/{_main,beta}
└── ansible/     Playbooks and roles, over the SSM connection plugin
services/        Go. The two backend binaries, built to ECR
```

Paths here are from the repo root. Cross-references *inside* `infra/` are
relative to it — a comment in an Ansible role pointing at `iac/stacks/sip-core`
means `infra/iac/stacks/sip-core`.

---

## What we're building

```
carrier trunks ──▶ EDGE (public subnet, Elastic IP)
                   Kamailio + RTPEngine, one box
                        │ SIP          │ RTP
                        ▼              ▼
                   FARM (private subnet)
                   FreeSWITCH, dumb B2BUA
                        │ outbound ESL, one socket per call
                        ▼  esl.beta.intron.internal:8022
                   APP (private)
                   ESL controller + control-plane API
                        │                      │
                        ▼                      ▼
                   RDS Postgres          LiveKit box (edge subnet)
                   tenants, DIDs,        livekit-sip + livekit-server,
                   trunks, handoff       B-leg over private UDP 5060
                                                ▲
                                                │ agent connects out, wss://
                                         Intron's GPU box (their account)
                                         STT / LLM / TTS
```

A call arrives from a trunk to Kamailio, which accepts it because the source is
an allowed carrier range and dispatches it to a FreeSWITCH node. FreeSWITCH
opens an ESL socket to the controller and hands it the call. The controller —
now holding the call — resolves the dialled number to a tenant and plan against
Postgres, and rejects it there if the number is not ours. If it lives, the
controller creates a LiveKit room and bridges the B-leg back out through
Kamailio to `livekit-sip`, where the AI agent answers. If the call needs a
human, it is bridged out through Kamailio to the client's contact centre.

**Admission happens in the controller, not at the edge.** Kamailio asks nothing
over HTTP and holds no carrier config; the only thing keeping the internet out
is `carrier_sip_cidrs` on the edge security group. See [docs/API.md](docs/API.md).

Four properties the layout is built around:

**The farm has no public address.** Every packet in arrives through the edge.
RTPEngine rewrites SDP so a FreeSWITCH private address never reaches a carrier.

**FreeSWITCH targets a DNS name, not an address.** `esl.beta.intron.internal`
resolves straight to the app instance, so the controller can be moved by
repointing a record rather than touching a FreeSWITCH node.

**One of everything.** This is a beta and it is built like one — one edge, one
FreeSWITCH node, one app instance, single-AZ Postgres. Nothing is redundant,
and a restart of any tier is an outage for the calls it is carrying.

**Access is SSM only.** No SSH key, no port 22 rule, `key_name = null` on every
instance. Ansible runs over `community.aws.aws_ssm`; a shell is
`make shell TARGET=<host>`.

---

## Phases

| Phase | What | Status |
|---|---|---|
| **1** | Infrastructure — VPC, SIP core, database, app tier | **Done** |
| **2** | Configuration — Kamailio, RTPEngine, FreeSWITCH, Docker | **Done** |
| **3** | Backend services — ESL controller, control-plane API | **Deployed to beta** (`v0.1.2`) |
| **4** | Integration — carrier trunks, LiveKit | **Done** |

### Phase 1 — Infrastructure ✅

Eight Terraform stacks, applied in order. Full detail and per-stage gates in
[docs/ROLLOUT.md](docs/ROLLOUT.md).

| Stack | What |
|---|---|
| `tfstate` | State bucket (versioned, encrypted, TLS-only) |
| `secrets-manager` | Six secret containers |
| `network` | VPC, 8 subnets across 2 AZs, NAT, S3 endpoint |
| `ecr` | Image repositories, immutable tags |
| `sip-core` | Edge + FreeSWITCH instances, Elastic IP, IAM |
| `database` | RDS Postgres 16 |
| `control-plane` | App instance, private DNS zone |
| `livekit` | LiveKit server for the AI leg, public DNS zone ([docs/LIVEKIT.md](docs/LIVEKIT.md)) |

Running today: ~**$400/month** (~$384 plus ~$17.50 for `livekit`). The
disposable `test-pbx` stack is extra while it exists.

### Phase 2 — Configuration ✅

Three plays, in this order — it matters, and is encoded in `site.yml`:

```
control-plane → freeswitch → edge
```

The ESL listener must accept before FreeSWITCH starts dialling it, and Kamailio
comes up last because carriers can deliver the moment it does.

| Play | What |
|---|---|
| `freeswitch` | FreeSWITCH, built from source (~30 min on a fresh node) |
| `edge` | Kamailio + RTPEngine |
| `control-plane` | The two Phase 3 containers, pulled from ECR |
| `livekit` | Not in `site.yml`; run on its own. See [docs/LIVEKIT.md](docs/LIVEKIT.md) |

### Phase 3 — Backend services ✅

Two containers, built from `services/` and pulled from ECR. Both run on the
app instance, image `v0.1.2`:

- **`esl-controller`** — raw TCP listener on 8022. FreeSWITCH opens a socket
  per call and hands the call over. The controller decides whether to accept
  it, sends it to the AI agent or a human, and writes the call record.
- **`control-plane`** — REST API over Postgres: tenants, DIDs, trunks, handoff
  targets, and `POST /v1/originate` for outbound calls. Used by operators and
  the tenant's own systems. **Nothing in the call path calls it.**

**Kamailio does not ask anyone whether to accept a call.** It carries SIP and
anchors media, and that is all: no HTTP request on an INVITE, and no carrier or
tenant config. The only filter at the edge is the `carrier_sip_cidrs` source
allowlist. The accept-or-reject decision happens in the controller, after
FreeSWITCH has handed it the call: it looks the dialled number up (cache first,
Postgres behind), and rejects with a SIP cause if the number isn't ours, the
tenant is suspended, or the tenant is at its concurrency limit. See
[docs/API.md](docs/API.md).

So a control-plane outage stops configuration changes, not calls. During a
database outage the controller keeps answering from its cache, serving expired
entries for up to 15 minutes; after that it refuses calls it can't look up.

The controller handles SIGTERM: it stops accepting new calls and waits for
calls in progress, up to its systemd grace period. **This is not a drain.**
There is one app instance, so new calls fail until it is back, and calls still
running when the grace period ends are cut off. A controller restart is a short
outage.

### Phase 4 — Integration ✅

- **Carrier trunks.** Twilio is the default trunk —
  `intron-beta.pstn.frankfurt.twilio.com`, IP-authenticated both ways. Its
  signalling ranges are applied as `carrier_sip_cidrs` on the edge security
  group, and Twilio's Origination URI and Termination IP ACL point at the
  edge Elastic IP `63.182.163.164`. Trunk records are application-layer, not
  Kamailio config — see [docs/API.md](docs/API.md). Each client's own SBC
  (bring-your-own-trunk) is added as a trunk record, and its ranges to
  `carrier_sip_cidrs`, as they onboard.
- **LiveKit.** We run our own LiveKit server in this VPC
  (`stacks/livekit`), reached from Kamailio over private UDP 5060. The AI
  models stay on Intron's GPU box, where a second agent worker connects out to
  our server (`infra/bin/intron-agent-bridge.sh`). Real calls have reached the
  agent. Recording is off, and the GPU fits 1–2 concurrent AI calls. See
  [docs/LIVEKIT.md](docs/LIVEKIT.md).

---

## Running it

```bash
# Terraform
cd infra/iac
make preflight                              # tooling, identity, region
make plan  STACK=<stack> ENV=beta
make apply STACK=<stack> ENV=beta
make lint                                   # before any security group change

# Ansible
cd infra/ansible
make galaxy boto-deps                       # one-time
make ping TARGET=all ENV=beta               # prove the SSM path works
make play PLAYBOOK=freeswitch ENV=beta
```

First time through, follow [docs/ROLLOUT.md](docs/ROLLOUT.md) instead — it has
a gate after each stage, because the failures here are mostly silent.

### Credentials

Everything authenticates with the **`klarity_aws`** profile, named explicitly
rather than inherited from the environment — in every Terraform provider,
backend and `terraform_remote_state` block, and as `ansible_aws_ssm_profile` in
the inventory. So a bare `terraform plan` or `ansible-playbook` uses the same
identity `make` does.

Three forms, and they differ for a reason:

| Block | Form | Why |
|---|---|---|
| `provider "aws"` | `profile = var.aws_profile` | Overridable per environment |
| `backend "s3"` | `"klarity_aws"` *(literal)* | Backend blocks **cannot** interpolate |
| `terraform_remote_state` | `profile = var.aws_profile` | Configures its own backend; inherits nothing from the provider |

Miss the third and the provider authenticates fine while a remote_state lookup
falls back to the default chain — with an error naming the bucket rather than
the credentials.

The profile carries **no region**, so a bare `aws` call fails with `NoRegion`
unless `AWS_REGION` is set. Both Makefiles export it.

### Secrets

Terraform owns the *container*; the *value* is pushed out of band. A secret
passed through Terraform is stored in cleartext in state, in every historical
version, forever.

```bash
infra/bin/env-file-to-sm intron-sip/beta/freeswitch ./fs.env
infra/bin/db-dsn-to-sm beta                 # composes DATABASE_URL from RDS
```

Instances fetch at service start via `ExecStartPre` →
`fetch-and-render-secrets.sh`, using the instance profile. Config files ship as
`.tpl` with `${VAR}` placeholders and are substituted **on the box**, so no
resolved secret reaches this repo, Ansible's S3 transport bucket, or a play's
diff. The script fails closed.

`FREESWITCH_ESL_PASSWORD` lives in **three** containers (`kamailio`,
`freeswitch`, `esl-controller`). Nothing keeps them in step — rotate together.

---

## Things that fail silently

Ordered by how long they cost when hit.

**The advertised address.** An Elastic IP is mapped 1:1 at the VPC edge — the
OS only ever sees `10.70.0.10`. Kamailio needs `advertise`, RTPEngine needs the
`!public` half of its `interface` line. Get either wrong and the call sets up
perfectly, signalling is clean end to end, and there is no audio.

**A stale `freeswitch_esl_outbound_target`.** Every inbound call drops at
answer with nothing in the controller's log, because the controller was never
reached. The FreeSWITCH role probes it at the end of the play for this reason.

**RTPEngine falling back to userspace.** The kernel module is DKMS, built
against the running kernel; an upgrade can leave it unbuilt. Calls keep working
and the box quietly loses most of its packet-per-second capacity. The role
asserts rather than assumes.

**Security group descriptions.** AWS accepts only a restricted character set,
enforced by the EC2 API rather than the Terraform schema — so an em dash or an
apostrophe passes `validate` and `plan`, then fails *mid-apply*. `make lint`
catches it.

**Ports defined in two places.** Terraform opens what the daemons bind to. A
mismatch shows up as no audio (RTP range) or a refused ESL socket — neither
names a port.

**IMDS hop limit.** 1 everywhere except the app tier, which needs 2 because a
container's IMDS traffic crosses the Docker bridge. At 1 the container finds no
credentials while `aws sts get-caller-identity` on the host works fine.
