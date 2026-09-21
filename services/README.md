# Backend services

The two Go binaries the app tier runs. Design and contracts are in
[docs/API.md](../docs/API.md); this file is how to build and run them.

| Binary | Role |
|---|---|
| `esl-controller` | Carries calls. FreeSWITCH opens one socket per call to `:8022`; this resolves the DID against Postgres, decides, and drives the call. |
| `control-plane` | Configuration and actions: operator CRUD, Twilio trunks, queued originates. Nothing it serves is in the path of a call. |

Both talk to Postgres directly. The split is by role, not by data access — the
controller does not need the API to carry a call, which matters because the API
restarts on every deploy.

## Layout

```
cmd/<name>/          main.go (error handling only) + wire.go (composition)
internal/
  admission/         does anyone own this number, and may the call proceed
  calls/             live-call ledger and call records
  route/             the dial strings for calls leaving the platform
  twilio/            reads Twilio's trunk config, and diffs it against ours
  esl/               the FreeSWITCH Event Socket protocol, both directions
  eslclient/         inbound ESL: controller -> FreeSWITCH :8021
  ctrl/              the listener, the per-call lifecycle, the sweeper
  store/             connection pool, migrations, the SQL both services run
  controlplane/      the operator API: auth, handlers, router
  shared/            config, logger, health, httpserver, run
migrations/          the schema, embedded into both binaries
```

`internal/shared` must not import from any app-level package.

**Only `control-plane` migrates.** Both services hold a database credential,
but two processes racing golang-migrate at startup is a deploy-ordering problem
nobody wants to debug at 3am. `esl-controller` calls `AssertSchema` instead and
refuses to start against a schema older than the build — so **deploy
control-plane first**.

## Running locally

```sh
cp .env.example .env
make run-control-plane      # or run-esl-controller
```

Health is on `:8088` by default and returns **503 until its dependencies are
wired** — that is the intended state, not a bug. The endpoint asserts
dependencies rather than liveness, because a check that only proves the process
is running keeps a wedged node in service.

```sh
curl -s localhost:8088/healthz | jq
```

Both services refuse to start on a missing required key and report **every**
missing key at once:

```
esl-controller: configuration is incomplete
  missing (set in the Secrets Manager container, not here): DATABASE_URL, FREESWITCH_ESL_PASSWORD
```

## Build, test, deploy

```sh
make check                  # vet + tests, what CI runs
make test-integration       # needs a local Postgres; each test makes its own database
make build                  # both binaries, linux/amd64
make push TAG=v0.1.0        # ECR, then follow the printed Ansible step
```

`make docker` and `make push` need Docker on PATH. Docker Desktop installs the
CLI at `~/.docker/bin/docker`, which is not always on a non-login shell's path:

```sh
export PATH="$HOME/.docker/bin:$PATH"
```

Two things the Makefile enforces rather than trusts:

**`linux/amd64`, always.** The app tier is x86_64. An arm64 image built on
Apple silicon pushes and pulls cleanly, then exits with `exec format error`.

**No pushing `dev` or a dirty tree.** ECR repositories use immutable tags, so a
tag names one set of bytes forever. A failed deploy needs a *new* tag, never a
re-push.

## API documentation

```sh
open http://localhost:8080/docs        # Scalar UI
curl localhost:8080/openapi.json       # the spec itself
```

The spec is **embedded in the binary** (`internal/controlplane/openapi`), so an
immutable image tag names the code and its description together — a spec served
from a mounted file drifts from the code that shipped with it.

**The first `servers` entry is whatever host you asked from.** The API is
private, so the usual way to reach it is an SSM port-forward on an arbitrary
local port:

```sh
aws ssm start-session --target <app-instance-id> \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["18080"]}'
```

A "Try it" button aimed at `api.beta.intron.internal:8080` from a browser that
cannot resolve it fails in a way that reads as the service being down. So the
handler rewrites `servers[0]` to the requesting origin — honouring
`X-Forwarded-Proto` and `X-Forwarded-Host`, because behind a TLS-terminating
load balancer `r.TLS` is nil here and reporting `http` would produce a
mixed-content failure instead. The documented addresses stay in the list after
it, and the response is `Cache-Control: private` since the body now varies by
Host.

Both routes are **unauthenticated**, and that is the only exception on this
mux. The spec describes endpoint shapes rather than data, and requiring a token
to read the documentation makes it useless to the person whose problem is
working out how to get a token. Scalar itself loads from a CDN rather than
being vendored: the runtime image is ~5MB and bundling a docs UI would roughly
double it to serve a page nobody opens from inside the VPC.

## Configuration

Environment only. On the instance, Ansible's `app-service` role renders an env
file from Secrets Manager at every service start — so **adding a key to the
application means adding it to
`infra/ansible/roles/app-service/templates/service.env.tpl.j2` first**, and to
the Secrets Manager container if it is a secret.

`.env.example` mirrors that template. Keep them in step.

## Admission

`internal/admission` answers what every inbound call starts with: does anyone
own this number, and may the call proceed. Both binaries import it — the
controller per call, the control plane behind `GET /v1/route` for an operator
asking why a number is not working.

```sh
curl -s 'localhost:8080/v1/route?did=004930123456' | jq
```

Three behaviours worth knowing before changing it:

**A refusal is not an error.** An unowned number is the system working, so
`Resolve` returns a verdict with a reason, not an error. Only a failed lookup
is an error — the two need opposite SIP responses (404 versus 503), and a
caller telling them apart by inspecting an error will eventually get it wrong.

**Misses are cached too, on a shorter TTL.** Kamailio proxies every INVITE from
an allowed carrier range straight through, so scanner traffic reaches this
code. Without a negative cache each junk call is a fresh query. The TTL is
shorter than for hits because a stale miss refuses a number that was just
provisioned — which is exactly when someone is watching.

**Serve-stale is the point of the cache, not a fallback.** If Postgres is
unreachable, an expired-but-recent entry is served for up to 15 minutes and the
verdict is marked `Stale`. A DID-to-tenant mapping changes on the order of
days; refusing calls it could have routed is the worse mistake.

## The ESL listener

`internal/esl` speaks the Event Socket protocol; `internal/ctrl` accepts the
socket FreeSWITCH opens per call and drives it.

**The protocol is hand-written, and that was a decision.**
`github.com/percipia/eslgo` is the obvious dependency, but its outbound entry
point calls `net.Listen` itself — so there is no way to supply a
`net.ListenConfig`, and a TCP keepalive on accepted sockets is the single most
important setting here. It also has no graceful shutdown, and its connection
constructor is unexported, so its protocol layer cannot be reused over a
listener we own. The wire formats were read off it rather than guessed.

Three things this code exists to get right:

**Keepalive on the listener**, so every accepted socket inherits it. An
`async full` socket goes silent while a caller is on hold with music; a
middlebox reaps the idle flow with no FIN, and calls drop at suspiciously round
intervals looking like a media fault.

**`myevents`, immediately.** Without it the connection gets the channel data and
then nothing — no answer, no hangup, ever. The call connects, this service
never learns it ended, and the socket holds a call nobody is on.

**A hangup cause on every refusal.** It is the only thing the carrier and the
CDR ever see. A bare hangup reads as `NORMAL_CLEARING`, making a refused call
indistinguishable from one that simply ended.

| Verdict | Cause | Carrier sees |
|---|---|---|
| Unknown or withdrawn number | `UNALLOCATED_NUMBER` | 404 |
| Suspended tenant, inactive plan, plan set to reject | `CALL_REJECTED` | 403 |
| At capacity | `NORMAL_CIRCUIT_CONGESTION` | 503 |
| Provisioned with no plan, or database unreachable | `NORMAL_TEMPORARY_FAILURE` | 503 |

Refusals happen **pre-answer**. The dialplan hands the call over unanswered on
purpose: answering starts carrier billing on a call that may be unroutable, and
makes a rejection look to the caller like a call that connected and died.

Tests run against a fake FreeSWITCH over a real TCP socket
(`fakefs_test.go`) — the failures that matter here are protocol failures, and a
mocked connection would only prove the mock works.

### The two ESL directions

They are easy to confuse, configured in different places, and fail differently:

```
outbound ESL   FS -> controller :8022   one socket per call, FS initiates
inbound  ESL   controller -> FS :8021   originate, uuid_*, the event stream
```

The handshakes are mirror images. Outbound, FreeSWITCH waits for `connect`.
Inbound, FreeSWITCH speaks first with an `auth/request` and nothing happens
until it is answered — a client written as though the connection were usable on
connect simply hangs.

`internal/eslclient` holds the inbound connections, one per node, with
backoff-and-jitter reconnect. **Its health check is what the Ansible role reads
after a deploy**, so a switch that is unreachable shows up there rather than as
a call that fails later. A node being down is not fatal: calls in progress
arrive on the *other* direction and do not depend on this one, and one healthy
node is enough to report healthy.

### Hangup: observed, not inferred

`bridge` returning only says the call ended, not who ended it. The answer is on
`CHANNEL_HANGUP_COMPLETE`.

**`CHANNEL_HANGUP` and `CHANNEL_HANGUP_COMPLETE` are not interchangeable.** The
first fires as teardown starts; the second fires once FreeSWITCH has finished
and is the one carrying the populated CDR variables — the SIP disposition, the
final cause. Taking whichever arrives first means recording fields that are not
filled in yet. So the controller waits up to 2s for the complete event and
falls back to the early one only if it never comes.

```
recv_bye   FreeSWITCH received a BYE on the caller's channel   caller
send_bye   FreeSWITCH sent it, because the far end ended       callee
(neither)  unset — a wrong attribution is worse than a missing one
```

A hangup **this service** issued keeps its own cause. FreeSWITCH echoing back
`NORMAL_CLEARING` would erase the reason a call was refused, which is the one
field anyone asks about afterwards.

## The operator API

Everything on `:8080` is authenticated. There is no unauthenticated route on
that mux at all — health is a separate listener on its own port, so there is
nothing needing an exception, and an exception is what eventually gets copied
onto a route that should not have one.

```sh
curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/tenants
```

| | |
|---|---|
| Config | `/v1/tenants`, `/v1/plans`, `/v1/dids`, `/v1/trunks`, `/v1/handoff-targets` — list, create, patch, delete |
| Keys | `/v1/api-keys` — mint, list, revoke |
| Outbound | `/v1/originate` — queue a call; `/v1/originate/{call_uuid}` — what happened to it |
| Diagnostics | `/v1/route?did=` — would this number be admitted, and why not<br>`/v1/trunks/{id}/drift` — does Twilio agree with us |
| Docs | `/docs` (Scalar UI) and `/openapi.json` — **unauthenticated**, see below |
| Read models | `/v1/calls` (live), `/v1/call-history` |

**Bearer keys, not JWT.** `JWT_SIGNING_KEY` is provisioned and deliberately
unused: validating a token is the easy half, and there is no identity
provider, no users table and no admin UI to issue one. Half an identity system
— self-issued tokens with no account, no revocation, no audit trail — would be
worse than keys that have all three.

**Keys are stored as a SHA-256 hash and shown once.** A database dumped for a
support ticket leaks nothing usable. A lost key is replaced, not recovered,
which is what makes the stored hash worth anything. SHA-256 rather than bcrypt
is deliberate: these are 256-bit random tokens, not passwords — no dictionary
to defend against, and a per-request bcrypt would add ~100ms to every call.

**`CONTROL_PLANE_API_TOKEN` is a bootstrap credential** and grants admin. It
has to work before any key exists, including for the request that creates the
first one. It parses as a *set*, so a rotation runs in two passes rather than
needing every holder to change in the same instant.

Four rules the boundary rests on, each with a test:

- **A tenant-scoped key gets its own tenant whatever it asked for.** One
  function, `Principal.Scope`, is what stops `?tenant_id=` being a
  cross-tenant read, and every handler goes through it.
- **A key cannot mint a key with a scope it does not hold.** Otherwise any
  admin-scoped tenant key could issue itself a platform key.
- **Not-found and not-yours are indistinguishable.** Answering 403 for a row
  that exists but belongs to someone else confirms it exists, which lets
  anyone enumerate another tenant's identifiers.
- **Unknown JSON fields are rejected.** A typo in `recording` would otherwise
  be silently ignored, and the caller would find out from a call that behaved
  wrongly.

DIDs are **normalised on write** with the same function the call path uses, so
`0049 30 123-456` and `+4930123456` are one row and the unique index means
what it says.

### Three resources that are not quite CRUD

**Agent config is part of the plan, not its own resource.** One row per plan,
and a plan routing to `ai` with no agent configured is a call refused
pre-answer. So `agent_identity` and `room_template` are fields on the plan:
required when `destination=ai`, rejected otherwise. There is no request
sequence that leaves a plan half-configured.

**A trunk is half an operation, and the response says so.** Its
`signalling_cidrs` must also reach `TF_VAR_carrier_sip_cidrs` and be applied,
or calls from it are dropped at the security group — a trunk that looks
perfectly configured and never receives a call. Nothing enforces the pair, so
the create response carries the next step and a CIDR change logs a warning.

```json
"next_steps": ["add these signalling_cidrs to TF_VAR_carrier_sip_cidrs and
  apply the sip-core stack — until then calls from this trunk are dropped at
  the security group, with nothing logged on the edge"]
```

`sip_password` is **refused with 501**, not ignored: it is stored encrypted at
the application layer and that encryption does not exist yet. Accepting it
would either store a carrier credential in the clear or silently drop it, and
a trunk that appears to have a password but does not fails at the first call.
It is never returned either — `has_password` is a boolean.

**Deleting a trunk with DIDs pointing at it is a 409.** Soft-deleting it would
leave those DIDs resolving to a trunk with no host, and the symptom is an
outbound leg aimed at nothing.

### Outbound calls

`POST /v1/originate` validates, claims a concurrency slot, and **queues** the
call. It does not dial — only the controller holds ESL connections.

```
POST /v1/originate → 202 {call_uuid, status: "pending"}
        │  INSERT originate_requests
        ▼  controller polls every 250ms:
           UPDATE ... WHERE id = (SELECT id ... FOR UPDATE SKIP LOCKED LIMIT 1)
        ▼  bgapi originate {origination_uuid=…} sofia/… &socket(…)
        ▼  customer answers → FreeSWITCH opens a socket → same path as inbound
```

**Why a table and not an HTTP call.** A queued row survives a controller
restart; an in-flight request does not. No new port, no new credential, and the
row *is* the audit trail — who asked, when, what happened, why it failed.

**`FOR UPDATE SKIP LOCKED` is the whole mechanism.** It locks the row it picks
and skips ones another transaction holds, so two controllers polling at the
same instant get different rows. Without it the second blocks, then claims the
same row after the commit — placing the call twice, which means ringing a
customer twice and billing for both. Tested with eight workers against forty
requests.

**One lookup settles three things:** the `from` DID must be active and owned by
the tenant (authorisation), it becomes the caller ID, and its row supplies the
trunk. Deriving the trunk from the DID makes a caller-ID/trunk mismatch
unrepresentable — carriers verify the calling number belongs to the account the
call arrives on.

`202`, not `201`: nobody's phone has rung yet. `placed` means *dialled*, not
answered; whether anyone picked up is in `/v1/call-history`.

### Trunk drift

Four pieces of configuration live in Twilio's console with nothing checking
them against ours, and every mismatch fails silently.

```sh
curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/trunks/1/drift
```

| Check | If it is wrong |
|---|---|
| `origination_uri` | **No inbound call ever arrives**, and there is nothing to see on our side — the packet goes wherever Twilio was told and never reaches the edge |
| `termination_acl` | Outbound calls refused with 403 |
| `phone_numbers` | Numbers on Twilio we do not know → refused as unknown. Numbers we have that Twilio does not → nothing ever calls them |
| `signalling_cidrs` | Recorded but **not verifiable**: Twilio publishes these as documentation, not through the API. The check says so rather than implying it checked |

**Read only, on purpose.** Nothing writes back to Twilio. Detecting drift is
most of the value; correcting it automatically is a much larger commitment and
easy to get wrong in a direction that takes a trunk down.

`EDGE_SIP_URI` is what origination is compared against. It is **not** stored
per trunk: the edge's address is infra state, and Ansible already holds it as
`kamailio_public_ip` from the sip-core stack. One address, one source — the
same reasoning that kept Kamailio's address out of the controller.

Two details the comparison gets right, because a check that cries wolf gets
ignored:

- `sip:1.2.3.4` and `sip:1.2.3.4:5060` are the same destination, as are
  differing case, a `;transport=` parameter, and a userinfo part.
- A bare address and a `/32` are the same allowlist entry.

The report is always **200**, even when it finds problems: "I checked and found
drift" and "I could not check" are different answers, and a monitoring script
should not confuse them.

## Concurrency and call records

A row in `calls` is a claim on a tenant's concurrency; `call_history` is the
permanent record. The claim is taken before anything that costs money, and the
record is written on **every** path out of a call — a refused call is still one
someone will ask about.

**The cap is enforced with an advisory lock, not a clever INSERT.** The obvious
form counts rows in the same statement as the insert:

```sql
INSERT INTO calls (...) SELECT ...
WHERE (SELECT count(*) FROM calls WHERE tenant_id = $2) < cap
```

That looks atomic and is not. Under `READ COMMITTED` the subquery cannot see
rows from transactions that have not committed, so simultaneous calls all count
the same total and all pass. Measured, not theorised: without the lock,
**4 claims succeed against a cap of 3**
(`TestClaimCall_CapHoldsUnderConcurrency`). Claims for one tenant are
serialised with `pg_advisory_xact_lock`, released automatically at commit.

That test needs a 30-connection pool to fail. The first version used the shared
4-connection one and passed against the broken implementation — worth knowing
before anyone "simplifies" it.

**The hangup write is one transaction:** record inserted, slot released,
together. Doing them separately means a crash in between either leaks a slot
forever or loses the record of a call that definitely happened.

**A crashed controller is swept.** Its rows stay claimed with nobody on the
calls, so capacity drops silently and the symptom is congestion on a platform
carrying nothing. Live calls heartbeat every 30s; anything quiet for 5 minutes
is reclaimed, and the sweeper runs once at startup because the most likely
reason stale rows exist is the crash this process is the restart of.

## Answering and bridging

`internal/route` builds the dial strings. Two facts decide where a call ends
up, and this package owns only the first:

```
Request-URI     who the call is for      the trunk, or a LiveKit room   ← here
next hop        where the packet goes    Kamailio                       ← the dialplan
```

**The next hop is not the controller's business.** The controller never talks
to Kamailio; it drives FreeSWITCH, and which edge FreeSWITCH sends through is a
fact about the FreeSWITCH node. Ansible already holds it — `edge_private_ips`
is built from the inventory — and the dialplan exports `sip_route_uri` onto the
outbound leg. A second copy in this service's environment would be one address
in two places with nothing keeping them in step.

Real output, against a seeded database:

```
AI plan
  answer
  set             RECORD_STEREO=true
  record_session  /mnt/recordings/t_acme__ai-9.wav
  bridge          {sip_h_X-Intron-Egress=livekit,
                   sip_h_X-Intron-Room=intron-t_acme-ai-9,
                   origination_caller_id_number=2348012345678}
                  sofia/internal/sip:intron-t_acme-ai-9@livekit.invalid

handoff plan
  answer
  bridge          {sip_h_X-Intron-Egress=carrier,...}sofia/internal/sip:queue@sbc.acme.example
                 |{...}sofia/internal/sip:+442071234567@trunk-host
```

Things worth knowing before changing any of it:

**Everything that can refuse happens before the answer.** A plan with no agent
configured, or a tenant with no handoff target, is refused pre-answer —
answering first starts carrier billing on a call about to fail and makes the
failure look to the caller like a connection that died.

**Order is answer → record → bridge.** Recording after answer because there is
no media before it, and before the bridge so the whole conversation is captured
rather than everything after the far end picked up. `RECORD_STEREO` because a
mixed-down mono file cannot be separated back into who said what.

**The LiveKit host is `livekit.invalid` on purpose.** Kamailio's dispatcher set
2 holds the real address and overwrites it; the placeholder is deliberately
unresolvable so an unconfigured dispatcher fails loudly rather than sending a
call somewhere real.

**A header decides the direction, not the shape of the URI.** Inference works
until a handoff target is a bare E.164 that looks exactly like a carrier
destination — at which point a call meant for a contact centre is dispatched to
LiveKit.

**Handoff failover is `|`-separated in priority order,** and a target that
cannot be dialled is skipped rather than failing the call: a misconfigured
third target should not cost a call the first two could have taken.

## Status

Steps 1–6 of the build order in [docs/API.md](../docs/API.md). A call arrives,
is resolved, claims a slot, is answered, recorded if the plan says so, and
bridged to LiveKit or a human — with a record written on every path:

```
 call_uuid | plan_id | livekit_room       | handoff_target | recorded | outcome | hangup_by
 ai-9      | p_ai    | intron-t_acme-ai-9 |                | t        | bridged | callee
 hu-9      | p_hu    |                    | 1              | f        | bridged | callee
```

A tenant can now be configured entirely through the API and then called:
create tenant → plan → DID → mint a key → place a call → read it back from
`/v1/call-history`.

A tenant is configurable entirely over HTTP: tenant → trunk → plan (with its
agent) → handoff targets → DIDs → a key, then place a call and read it back.

**Not done:** the audit log, credential encryption for `sip_password`, and
writing corrections back to Twilio.

Also not done: the LiveKit room is named and passed as a SIP header, but nothing
calls LiveKit's API to create it. Whether that call is needed depends on how
the LiveKit side's dispatch rules are configured, which is an open question in
[docs/LIVEKIT.md](../docs/LIVEKIT.md) — building a client against an
unverifiable assumption would be worse than leaving the seam visible.

Next: deployment. The gap now is the `infra/` checklist in
[docs/API.md](../docs/API.md) and one real call, not more code.
