# Phase 3 — the backend services

Two containers: `control-plane` (REST over Postgres) and `esl-controller`
(outbound-ESL listener). They are built from `services/` in this repo, pushed to
ECR, and run by systemd on the single app instance.

This document is the contract. Almost every value in it is already fixed by
something in `infra/` — a security group, a systemd unit, a Kamailio route — so
where a number appears here it is quoted from there rather than chosen.

```
                                  control-plane :8080 ──▶ RDS Postgres
                                        ▲   │
                DID resolution, plan     │   │ originate (loopback)
                config, CDRs, live calls │   ▼
FreeSWITCH ──socket() per call──▶ esl-controller :8022 ──▶ LiveKit (own VPC)
     ▲                                  │
     └──── inbound ESL :8021 ───────────┘   originate, uuid_*, event stream
```

**Kamailio talks to neither service.** It carries SIP and anchors media, and
that is all. It does not make an HTTP call on the inbound path
([Contract 1](#contract-1--getv1route)) and it does not drive FreeSWITCH on the
outbound one ([Contract 3](#contract-3--originate)). Every decision is made by
the controller, with the call already in hand.

---

## What the infrastructure already decided

None of these are open. Changing one means an Ansible or Terraform change
first, and most of them fail silently if the application disagrees.

| Contract | Fixed in | What it forces |
|---|---|---|
| Two images, immutable tags | `iac/stacks/ecr/_main/main.tf` | A deploy is a new tag in `control_plane.yml`. No `latest`, no re-push. |
| Both containers on **one** host, `--network host` | `roles/app-service/templates/app-service.service.j2` | No port may be claimed twice. The controller sees the real source address of each FreeSWITCH connection — that is deliberate and load-bearing. |
| Config arrives as an **env file, already rendered** | `roles/app-service/templates/service.env.tpl.j2` | The apps read `os.Getenv` and nothing else. No Secrets Manager SDK, no AWS call to start. |
| `esl-controller` has no `DATABASE_URL` | **changed** — it gets one, see [Why the split is where it is](#why-the-split-is-where-it-is) | Add `DATABASE_URL` to the `esl-controller` secret container and its env block. The controller reads and writes Postgres directly; it does not depend on the API to carry a call. |
| Kamailio makes no HTTP call on the inbound path | **changed** — `route[ADMISSION]` is removed, see [Contract 1](#kamailio-does-not-ask--and-that-is-the-change) | The DID lookup happens in the controller, with the call already in hand. Caching is still the availability model, just no longer the edge's. |
| FreeSWITCH hands the call over **pre-answer** | `roles/freeswitch/templates/dialplan-public.xml.j2` | The controller owns answer/reject. A rejection must carry a cause code, not a silent hangup. |
| Postgres, and nothing else | `iac/stacks/` | No Redis, no Temporal, no ClickHouse. Everything the reference platform does with those has to be Postgres, in-process, or explicitly deferred. |
| Debian on `m6i.large` / `c6i.xlarge` | `iac/stacks/control-plane`, `sip-core` | Images are **linux/amd64**. A laptop build on Apple silicon produces arm64 images that pull fine and then exit 255 with `exec format error`. |

---

## Why the split is where it is

**The split is by role, not by data access. Both services own the database.**

`esl-controller` carries calls. It resolves the dialled number, decides what
happens, drives FreeSWITCH, and writes the CDR — reading and writing Postgres
directly for all of it. It holds a TCP socket for the life of every call, which
is why its unit gets a 120s SIGTERM grace.

`control-plane` exposes configuration and actions: the operator CRUD surface,
Twilio trunk management, and the endpoints that *request* something of a call.
Ordinary HTTP, millisecond requests, restarts freely.

**The controller does not depend on the API to carry a call.** That is the
point of giving it a database credential, and it buys three things:

- **The API can be down and calls still work.** `control-plane` restarts on
  every deploy. A design where the controller asks it a question per call makes
  every API restart a window where inbound calls fail, on the service that was
  specifically built to restart freely.
- **One fewer hop in the call-setup path.** The DID lookup is a point query on
  an indexed column, not an HTTP round trip that wraps a point query.
- **CDR writes are a transaction, not a delivery problem.** Writing through
  another service's API needs an outbox, retries and idempotency keys to
  survive a restart. Writing to Postgres needs a transaction.

This mirrors the reference platform, where `sip-controller` reads Postgres
directly and `core-api` is purely the configuration plane.

**What it costs, and what to do about it:**

| Cost | Handling |
|---|---|
| Two services, one schema | `control-plane` is the **only** migrator. The controller never runs migrations — two processes racing at startup is a deploy-ordering problem you do not want at 3am. |
| Two connection pools on a `db.t4g.micro` | 1 GiB of RAM is roughly 110 connections. Both pools plus operator sessions must fit — cap them explicitly rather than taking a driver default. |
| Two caches, two TTLs | Accept it. An operator edit takes up to one TTL to reach each process, which for DID-to-tenant config is fine. |

That last one is not optional, and the repo says so: the `instance_class`
variable notes that `db.t4g.micro` "stops being defensible if the API does not
cache and every inbound INVITE becomes a query." That warning now points at the
controller. It is the thing querying per call, so it is the thing that must
cache.

### What the control plane exposes

Configuration and actions. Nothing on this list is in the path of a call.

**Config — CRUD, JWT-authenticated, tenant-scoped**

`/v1/tenants`, `/v1/dids`, `/v1/trunks`, `/v1/plans`, `/v1/handoff-targets`,
`/v1/agent-configs`, `/v1/business-hours`, `/v1/api-keys`

**Actions — things that make something happen**

| Endpoint | Does |
|---|---|
| `POST /v1/originate` | Queues an outbound call — [Contract 3](#contract-3--originate) |
| `POST /v1/trunks/{id}/sync` | Reconciles a trunk against Twilio: numbers, origination URI, IP ACL |
| `POST /v1/calls/{uuid}/hangup` | Operator kill switch for a call in progress |

**Read models — for an operator or a dashboard**

| Endpoint | Answers |
|---|---|
| `GET /v1/route?did=&from=` | "Would a call to this number be admitted right now, and why not?" Same code as the call path; see [Contract 1](#contract-1--admission) |
| `GET /v1/calls` | What is live right now, per tenant |
| `GET /v1/call-history` | CDRs, filtered and paginated |

`POST /v1/calls/{uuid}/hangup` is the one action that needs the controller, and
it goes through the same queue table as originate rather than growing a second
mechanism.

---

## Repo layout

Go, one module, two binaries — mirroring the shape that is already proven
against FreeSWITCH and Kamailio in the reference platform.

```
services/
  go.mod                        module github.com/Adebayobenjamin/intron-sip-project/services
  Makefile                      build, test, lint, docker, push
  cmd/
    control-plane/              main.go, wire.go
    esl-controller/             main.go, wire.go, app.go
  internal/
    store/                      repositories — the SQL both binaries share
    admission/                  DID -> tenant/plan, the cache, serve-stale
    calls/                      live-call ledger + CDR writes
    controlplane/
      handlers/ services/ requests/ router/ middleware/
    ctrl/                       per-call ESL logic: listener, inbound, bridge, teardown
    eslclient/                  inbound ESL pool (controller -> FS :8021)
    livekit/                    room creation, dial-string construction
    shared/                     config, logger, models, httpserver, errors
  migrations/                   golang-migrate, applied by control-plane at start
  Dockerfile.control-plane
  Dockerfile.esl-controller
```

**`internal/store` is the shared layer, and it is shared deliberately.** Both
binaries read the same tables, and two hand-written copies of "resolve a DID"
drift — one gets the soft-delete filter and the other does not, and the
difference shows up as a call routed to a tenant that was removed last week.
One package, two callers.

`internal/admission` and `internal/calls` are leaf packages over `store`, used
by both binaries. `internal/shared` must not import from `internal/controlplane`
or `internal/ctrl`.

Handlers parse and write; services hold the logic; `store` holds SQL. One file
per domain in each layer.

---

## Contract 1 — admission

Resolve a dialled number to a tenant and a plan, and decide whether the call
lives. **This is a package (`internal/admission`), not an endpoint.** The
controller calls it in-process, against the database, with the call already in
hand.

### Nobody asks over HTTP any more

Two changes landed on this in sequence, and the endpoint did not survive them.

It began as `GET /v1/route`, called by Kamailio from `route[ADMISSION]` on
every inbound INVITE. Then Kamailio stopped asking — it now accepts an INVITE
from a known trunk and proxies straight to FreeSWITCH. Then the controller got
a database credential, so the one remaining caller had no reason to go through
HTTP either.

```
was     carrier ─▶ Kamailio ──HTTP, blocking──▶ control-plane ─▶ Postgres
                       └─▶ FreeSWITCH ─▶ controller

is      carrier ─▶ Kamailio ─▶ FreeSWITCH ─▶ controller ─▶ Postgres
                                                 └─▶ reject if unknown
```

Keep `GET /v1/route` as an **operator endpoint** — "who owns this number, and
would a call to it be admitted right now?" is the first question asked when a
client says calls are not arriving, and answering it from the same code the
call path uses is worth more than the endpoint costs. It is a debugging
affordance, not a dependency: nothing in the call path calls it.

**Why it is better, in order:**

**Nothing in the call path depends on another process.** `control-plane`
restarts on every deploy. A per-call question asked of it makes each of those
restarts a window where inbound calls fail.

**Kamailio is process-per-message.** A worker blocked on HTTP handles nothing
else, so inbound capacity was capped at roughly `children ÷ timeout` — 8
workers against a 2s timeout is 4 calls/second in the worst case, and any
slowness in the lookup became a receive-queue backlog on the proxy rather than
a slow lookup. The same work in the controller blocks a goroutine, which costs
nothing.

**The lookup is now a point query, not an HTTP round trip wrapping one.**
Kamailio was on the edge box and the API on the app box, so every lookup
crossed subnets to reach a process that then did a `SELECT`. The controller
does the `SELECT`.

**It stops being a platform-wide single point of failure.** Kamailio failed
closed: no answer from the API meant `503` on every inbound call, so a single-AZ
RDS blip was a total inbound outage. Now the call is already in hand when the
lookup happens, and the failure is one call's decision rather than the edge
refusing everything.

**Two silent-failure traps disappear outright.** No JSON parsing in Kamailio
means no `admitted` boolean-versus-string mismatch. No HTTP call from Kamailio
means no missing `Authorization` header. Both were on the fail-silently list;
both are now impossible rather than merely handled.

**The perimeter does not weaken.** What kept the internet out was never this
lookup — it is `carrier_sip_cidrs` on the edge security group
(`sip-core/_main/security-groups.tf:54`), which admits 5060 from carrier ranges
and nothing else. The DID check is a business rule, not a security boundary,
and it belongs where the business logic is.

**What it costs.** A call to a DID we do not own now reaches FreeSWITCH,
allocates a session and opens a TCP socket before being rejected, instead of
dying at the edge. That is real work per junk call — but only from an IP a
carrier trunk is allowed to originate from, so the volume is bounded by the
trunk, not by the internet.

### What it returns

```go
type Verdict struct {
    Admitted    bool
    TenantID    string
    PlanID      string
    Destination string   // ai | handoff | reject
    Recording   bool
    Reason      string   // ok, unknown_did, tenant_suspended,
                         // out_of_hours, over_capacity
}
```

It returns **the whole plan**, not two opaque ids. The controller needs the
plan config regardless, and resolving it in the same pass as the DID is one
query rather than two. When this had to squeeze through Kamailio's
`jansson_get` that was not an option; now it is a struct.

`Reason` is for logs and the operator endpoint. `Admitted == false` always
carries one — a refusal with no reason is a support ticket that starts from
zero.

### Latency and failure

| Budget | Value |
|---|---|
| Cache hit | < 1ms |
| Cache miss, database healthy | < 30ms p99 |
| Query timeout | 2s, then hang up `NORMAL_TEMPORARY_FAILURE` |

**The controller caches, and this is not optional.** It is the process doing a
query per call, so it is the process that has to. The `instance_class` variable
in `iac/stacks/database` says the quiet part directly — `db.t4g.micro` "stops
being defensible if the API does not cache and every inbound INVITE becomes a
query." Burstable instances do not fail under that load, they exhaust CPU
credits and then throttle, which presents as call setup latency degrading over
hours and recovering overnight.

The control plane caches too, for its own reads. Two processes, two TTLs, and
that is fine: the staleness budget is the TTL either way, and an operator edit
reaching one process a few seconds before the other changes nothing anyone can
observe.

The cache is in-process, keyed by normalised DID, TTL on the order of a minute.
Two rules that matter more than the TTL:

**Serve stale on database error.** A DID→tenant mapping is operator-edited
config; it changes on the order of days. If Postgres is unreachable and the
cache holds an entry, answer from it and log loudly. Single-AZ RDS still sits
behind every inbound call (`docs/ROLLOUT.md`, Stage 4); this is what turns a
database blip into stale-but-working. Moving the lookup off Kamailio made the
blast radius one call instead of the whole edge — it did not make the cache
optional.

**Cache the schedule, not the verdict.** Business hours are a static row, but
whether a plan is open depends on the clock. Cache the row and evaluate
openness fresh on every call, or a plan stays shut for up to a TTL after it
opened.

Warm the cache at startup. A cold process after a deploy otherwise takes its
first few hundred calls at database latency, and fails them all if Postgres
happens to be the thing that was wrong.

Removing the admission lookup means deleting things in `infra/` that would
otherwise sit there looking load-bearing — see
[Infra changes this design requires](#infra-changes-this-design-requires).

### The egress direction

Admission answers for **ingress only** — it maps a dialled number to the tenant
that owns it, which is a question only inbound calls have.

Deleting `route[ADMISSION]` dissolves half of what used to be a problem here:
egress INVITEs from FreeSWITCH no longer get 404'd by an admission check that
was never meant for them. What remains is that Kamailio still has to tell the
two directions apart — carrier-inbound goes to the FreeSWITCH dispatcher,
FreeSWITCH-outbound goes to a carrier or LiveKit — so the source-address branch
is still needed, now as plain routing rather than as a workaround. See
[Contract 5](#contract-5--egress-how-freeswitch-dials-out-through-kamailio).

**The policy is not.** Nothing yet asks *may this tenant dial this
destination*. Trunk selection being application-layer does not answer it —
knowing which trunk a call leaves by is not the same as deciding whether it
may leave at all, and on a platform wired to carriers that gap is how toll
fraud gets billed.

Where it goes when it is wanted: `POST /v1/originate` already validates before
claiming a slot, which covers outbound. Handoff targets are operator-configured
and self-limiting. So the remaining hole is narrow, and a separate `/v1/egress`
endpoint is probably unnecessary — keep egress policy in the originate path
rather than making one cached `/v1/route` answer serve two different questions.

---

## Contract 2 — call state, written directly

The controller owns the `calls` and `call_history` tables. No endpoint sits in
front of them; the earlier design routed these through an internal API because
the controller had no database credential, and that constraint is gone.

| Moment | What the controller writes |
|---|---|
| Socket accepted, DID resolved | `INSERT INTO calls` — claims the concurrency slot |
| Answer, bridge, handoff | `UPDATE calls` — state and timestamps |
| Every 30s while the call runs | `UPDATE calls SET heartbeat_at = now()` |
| Hangup | `INSERT INTO call_history`, `DELETE FROM calls`, **one transaction** |

**Slot claim and cap check are one statement, not two.** Reading the count and
then inserting is a race that lets two simultaneous calls both pass a cap of
one. Insert conditionally and let the database arbitrate:

```sql
INSERT INTO calls (call_uuid, tenant_id, ...)
SELECT $1, $2, ...
WHERE (SELECT count(*) FROM calls WHERE tenant_id = $2) < $cap
ON CONFLICT (call_uuid) DO NOTHING
RETURNING call_uuid;
```

No row returned means the cap is reached — hang up `NORMAL_CIRCUIT_CONGESTION`.
`ON CONFLICT DO NOTHING` makes a retried insert a no-op, using the channel UUID
FreeSWITCH already generated as the natural key.

**A crashed controller leaks slots.** Its rows stay claimed with nobody holding
the calls. Hence `heartbeat_at`, and a sweeper deleting anything untouched for
five minutes. Without it, one crash permanently reduces a tenant's capacity and
nothing anywhere says why.

**The hangup write is a transaction, and that is the point of this change.**
CDR insert and slot release commit together or not at all. Sent through another
service's API it needed an outbox, a retry policy and an idempotency key to
survive a restart mid-write — three mechanisms to approximate what one
`BEGIN` gives for free.

Keep the retry for the *connection*, not the delivery: if Postgres is briefly
unreachable at hangup, buffer in memory and retry with backoff, flushing within
the 120s SIGTERM grace. A lost CDR is invisible until someone goes looking for
a call that definitely happened.

---

## Contract 3 — originate

`kamailio.cfg.j2:281` answers `501 Not Implemented` with a literal `TODO:
implement against the control plane's originate contract`. **Do not implement
it. Delete it.**

Originate is issued over **inbound ESL**, from the controller straight to
FreeSWITCH, on the connection it already holds.

```
POST /v1/originate            (operator -> control-plane :8080)
{ "tenant_id", "to", "from", "plan_id", "metadata" }
→ 202 { "call_uuid" }

control-plane resolves the from-DID, queues an originate_requests row
      │
      ▼  claimed via FOR UPDATE SKIP LOCKED
esl-controller
      │  bgapi originate {origination_uuid=<uuid>,
      │                   origination_caller_id_number=<the DID>,
      │                   sip_route_uri=sip:<kamailio>:5060, ...}
      │           sofia/internal/sip:<to>@<trunk host>
      ▼  inbound ESL, FS:8021 — already open, already credentialled
FreeSWITCH ──INVITE──▶ Kamailio ──▶ carrier
```

### The from-DID is resolved before the call is placed

`from` names the DID the call presents. It is not a hint and it is not taken on
trust — the control plane looks it up in `dids` and refuses the originate
unless the row is **active and owned by `tenant_id`**. Without that check,
whoever can call the API can present any number on the platform, including
another tenant's, and caller-ID validation that trusts its caller is not
validation.

**One lookup settles two things.** A `dids` row carries both the tenant and the
trunk, so resolving the from-DID also resolves the route out:

```
from = +4930123456
   └─▶ dids row ─┬─▶ owned by t_9f3a?        → authorisation
                 ├─▶ becomes the From header → caller ID
                 └─▶ trunk_id → trunks row   → SIP host for the Request-URI
```

That coupling is not incidental, it is required. Carriers verify the calling
number belongs to the account the call arrives on — Twilio rejects an Elastic
SIP Trunking call whose `From` is not a number on the account. Presenting
tenant A's DID over a trunk that does not carry it is a `403` at best and a
call that completes with an unroutable caller ID at worst. Deriving the trunk
*from the DID* makes the mismatch unrepresentable rather than merely unlikely.

If `from` is omitted, fall back to the plan's default DID, then the tenant's.
Never fall back to a platform-wide number: it works, which is the problem — see
[Things that will fail silently](#things-that-will-fail-silently).

Kamailio stays in the **signalling** path — the INVITE still leaves through it,
RTPEngine still anchors the media, the Elastic IP is still what the carrier
sees. What it comes out of is the **control** path, where it was never adding
anything.

### Why this is better, in descending order of how much it matters

**The HTTP path is not wired, and wiring it is a security regression.**
`mod_xml_rpc` is not in `freeswitch_modules` (`roles/freeswitch/defaults/main.yml:56`)
— FreeSWITCH does not load it, so Kamailio has nothing to drive. And
`fs_xmlrpc` in `sip-core/_main/security-groups.tf:261` admits the **app**
subnets only, not the edge. Making the HTTP path work therefore costs: a
FreeSWITCH rebuild to add the module (~30 minutes), plus a new rule opening a
full remote-control interface on FreeSWITCH to the box that faces the internet.
The ESL path needs neither — `mod_event_socket` is loaded, `fs_esl_inbound`
already admits the app tier, and `FREESWITCH_ESL_PASSWORD` is already in the
controller's env.

**Kamailio's xhttp handler runs in a SIP worker.** `event_route[xhttp:request]`
is dispatched by the same `children` processes that handle INVITEs. An
originate serviced there blocks one of them, and an originate blocks until the
far end answers or times out — seconds to tens of seconds, not the 2s the
admission lookup is capped at. This is the pathology the `http_client` timeout
comment warns about, with a limit an order of magnitude worse. A handful of
concurrent outbound calls would stall inbound call setup on the edge.

**Three hops and two extra credentials become one command.** The HTTP route is
control-plane → Kamailio xhttp → FreeSWITCH xmlrpc, needing
`CONTROL_PLANE_API_TOKEN` at Kamailio and `FREESWITCH_XMLRPC_USER`/`_PASSWORD`
at FreeSWITCH, on top of the ESL password already in play. Every one is a thing
to rotate and a place to be out of step.

**The UUID is known before the call exists.** `origination_uuid=<uuid we chose>`
means the `&socket()` that arrives when the far end answers is matched
deterministically. Through HTTP you get whatever FreeSWITCH assigned, relayed
back through two hops — or you correlate on caller/callee, which breaks the
first time two calls to the same number overlap.

**`bgapi` is asynchronous and reports on the channel we already read.** It
returns a Job-UUID immediately and the outcome arrives as a `BACKGROUND_JOB`
event on the same ESL connection. The HTTP path holds a connection open for the
duration of the ring.

**Failure detail survives.** ESL hands back the hangup cause —
`NO_ANSWER`, `USER_BUSY`, `CALL_REJECTED`. Through xhttp→xmlrpc it is an HTTP
status and a string to parse, two hops from where the failure happened.

### What this needs that does not exist yet

**A Kamailio route for INVITEs from the FreeSWITCH subnet.** The same egress
branch the LiveKit B-leg needs — see
[Contract 5](#contract-5--egress-how-freeswitch-dials-out-through-kamailio).
One fix covers both. No sofia gateway is required; the dial string carries the
destination.

**A handoff from the control plane to the controller.** Only the controller
holds ESL connections, so the API cannot place a call itself. With a shared
database the handoff is a table, not a call:

```
POST /v1/originate ─▶ control-plane validates the from-DID,
                      INSERT INTO originate_requests (status='pending')
                                    │
                      controller claims it:
                      UPDATE ... SET status='claimed' WHERE status='pending'
                        AND id = (SELECT id FROM originate_requests
                                  WHERE status='pending'
                                  ORDER BY created_at
                                  FOR UPDATE SKIP LOCKED LIMIT 1)
                      RETURNING *
                                    │
                                    ▼  bgapi originate over ESL
```

No new port, no new credential, no HTTP surface on the controller, and
`FOR UPDATE SKIP LOCKED` means a second controller would be safe without any
further thought. A queued originate survives a controller restart, which an
in-flight HTTP request would not.

The cost is poll latency — sub-second with a short interval, and irrelevant for
outbound dialing. `LISTEN`/`NOTIFY` removes even that if it ever matters.

### Scope

Beta scope is unchanged: outbound is the lowest-priority contract here and
nothing in Phase 4 depends on it. But the *direction* should be settled now,
because the alternative costs a FreeSWITCH rebuild and a security group
widening if it is discovered late.

What this deletes from Kamailio is listed in
[Infra changes this design requires](#infra-changes-this-design-requires).

---

## Contract 4 — the ESL call lifecycle

FreeSWITCH connects to `esl.beta.intron.internal:8022` with
`socket ... async full` and holds the connection for the life of the call.

Per call, in order:

1. Accept. Set a TCP keepalive of `ESL_TCP_KEEPALIVE_SECONDS` (60) on the
   **accepted** socket — `net.ListenConfig{KeepAlive:}`. An `async full` socket
   goes genuinely silent when a caller is on hold with music, and any middlebox
   in the path reaps an idle flow with no FIN. The call drops at a
   suspiciously round interval with nothing in either log.
2. `myevents` subscribe. The outbound mode does not do this for you; miss it
   and you get the initial `CHANNEL_DATA` and then silence forever, including
   no `CHANNEL_HANGUP`.
3. Read the call context off the channel: `uuid`, `destination_number`,
   caller ID, `intron_node`. The tenant and plan are **not** here any more —
   Kamailio no longer looks them up, so `X-Intron-Tenant` / `X-Intron-Plan` and
   the `intron_tenant` / `intron_plan` channel variables are gone.
4. `admission.Resolve(did, from)` — in-process, cache first, Postgres behind
   it. Not admitted → hang up `UNALLOCATED_NUMBER`, which the carrier sees as a
   SIP `404`. No answer within 2s → `NORMAL_TEMPORARY_FAILURE`.
5. Claim the concurrency slot with the conditional insert in
   [Contract 2](#contract-2--call-state-written-directly). No row back → the
   tenant is at its cap, hang up `NORMAL_CIRCUIT_CONGESTION`.
6. Act on `destination`: AI leg, straight to human, or reject.
7. Answer — and not before. The dialplan deliberately does not answer, because
   answering starts carrier billing on a call that may be unroutable and makes
   a rejection look to the caller like a call that connected and died. Every
   rejection above happens **pre-answer**, which is the whole reason that
   ordering matters.
8. Recording, if the plan says so: `record_session` to `/mnt/recordings`, with
   the filename fixed now (`<tenant>__<call_uuid>.wav`) even though
   `recording_bucket` is empty and the uploader is not deployed. The naming is
   the contract between FreeSWITCH, the eventual uploader, and the CDR's
   `recording_url` — changing it later means two systems to fix.
9. Bridge the B-leg to `livekit-sip` through Kamailio. **A `sips:` URI when
   `livekit_sip_transport` is `tls`** — the scheme selects TLS, not the port. A
   `sip:` URI aimed at 5061 is dropped by the far end and surfaces as "no
   route", naming nothing about TLS.
10. Handoff, on request: bridge the caller out to the tenant's handoff target,
    in order, with failover.
11. `CHANNEL_HANGUP` → one transaction: insert `call_history`, delete the
    `calls` row, commit. Then close the socket.

Every rejection carries a cause, and the cause is the only thing the carrier
and the CDR ever see:

| Situation | Cause | Carrier sees |
|---|---|---|
| Nobody owns this DID | `UNALLOCATED_NUMBER` | `404 Not Found` |
| Tenant suspended, or plan closed | `CALL_REJECTED` | `403 Forbidden` |
| Over the tenant's concurrency cap | `NORMAL_CIRCUIT_CONGESTION` | `503` |
| Control plane unreachable | `NORMAL_TEMPORARY_FAILURE` | `503` |

A bare hangup with no cause reads as normal clearing, which makes a rejected
call indistinguishable from one that simply ended — in the carrier's records
and in ours.

On SIGTERM: stop accepting, flip health to unhealthy, let in-flight calls run,
flush any buffered CDR writes, close the pool, exit inside 120s. Without signal
handling that grace period is just a delayed `SIGKILL`.

---

## Contract 5 — egress: how FreeSWITCH dials out through Kamailio

Every call that leaves the platform — the LiveKit B-leg, a human handoff, an
outbound originate — is an INVITE from FreeSWITCH into Kamailio. All three take
this path, and none of them works today.

```
controller ──ESL:8021──▶ FreeSWITCH ──INVITE──▶ Kamailio ──▶ carrier / LiveKit
                                                    │
                                              RTPEngine anchors,
                                              advertises the Elastic IP
```

### The dial string

The Request-URI carries the **real destination**; the packet is sent to
Kamailio as the next hop. Those are two different things and the distinction is
the whole trick:

```
{sip_route_uri=sip:10.70.0.10:5060,\
origination_uuid=<uuid>,\
origination_caller_id_number=+4930123456,\
intron_tenant=t_9f3a,intron_direction=outbound,\
sip_h_X-Intron-Egress=carrier}\
sofia/internal/sip:+2348012345678@intron-beta.pstn.frankfurt.twilio.com
```

`sip_route_uri` forces the next hop to Kamailio while leaving the URI pointed
at the trunk's host. Both the caller ID and that hostname come from the same
place — the from-DID resolved at originate time, and the trunk its `dids` row
points at (see [Contract 3](#the-from-did-is-resolved-before-the-call-is-placed)).
So **Kamailio holds no carrier config at all**, consistent with trunking being
application-layer.

Verify the variable against the FreeSWITCH build in use. The equivalent is a
sofia gateway with `proxy` set to the trunk host and `outbound-proxy` set to
Kamailio — which works, but puts one static gateway per trunk back into
Ansible, which is the thing being avoided.

**No sofia gateway is needed for this.** `internal.xml.j2` defines one profile
with no `<gateways>` block, and it can stay that way: a profile plus a URI is a
complete dial string. That is one fewer template to maintain and one fewer
place for a carrier address to be written down.

`origination_caller_id_number` must be set, and it must be a DID the tenant
owns. It becomes the `From`, which is what the carrier bills, displays, and
screens on. Left unset it is FreeSWITCH's default and the carrier rejects the
call or presents something meaningless.

### The Kamailio branch

`request_route` sends every tagless INVITE through `route[ADMISSION]`, which
looks the Request-URI user up as a tenant DID. For an egress INVITE that user
is a carrier number or a LiveKit endpoint, so the lookup misses and the call
gets a `404`. The symptom is an inbound call that answers and then dies at
bridge — late, and pointing at the wrong layer.

Branch on source address before admission:

```
if (is_method("INVITE")) {
    if (src_ip == FREESWITCH_SUBNET) {
        route(EGRESS);
    } else {
        route(ADMISSION);
        route(DISPATCH);
    }
    exit;
}

route[EGRESS] {
    route(MEDIA_OFFER);

    if ($hdr(X-Intron-Egress) == "livekit") {
        # BOTH ARGUMENTS UNQUOTED. See the note below the block.
        if (!ds_select_dst(DS_LIVEKIT, 4)) {
            sl_send_reply("503", "No LiveKit Destination");
            exit;
        }
    }
    # carrier: relay to $ru as FreeSWITCH built it. No gateway, no dispatcher
    # set, no carrier address anywhere in this config.

    route(RELAY);
}
```

**LiveKit and carrier are handled differently, deliberately.** LiveKit is one
platform-level destination, already in dispatcher set 2, already probed with
OPTIONS — and that `AP`/`IP` probe state is the diagnostic `docs/LIVEKIT.md`
tells you to read when the AI leg fails. Keeping it on the dispatcher preserves
that. Carriers are per-tenant configuration that changes as clients onboard,
so they come from the database and Kamailio never learns their names.

**`ds_select_dst` takes unquoted integers, and this is not a style point.** An
earlier version of this document wrote `ds_select_dst("DS_LIVEKIT", "4")`, and
`route[DISPATCH]` in the Ansible template had the same form. `#!define` is a
preprocessor *token* substitution — it does not reach inside string literals —
so Kamailio receives the literal text `DS_LIVEKIT` and refuses to start:

```
fixup_igp_igp(): Cannot convert function parameter 1 to FPARAM_INT | FPARAM_PVS
fix_actions(): fixing failed (code=-1) at cfg:/etc/kamailio/kamailio.cfg:213
```

which names a fixup function and a parameter index — not the define, not the
quotes, not the line you would look at first.

**`kamailio -c` does not catch it.** It parses and prints `config file ok`;
fixups only run during full module init. A config change is verified by
starting the service, not by syntax-checking it.

Two more things in that route are not optional:

**`route(MEDIA_OFFER)` must run on egress too.** Without it the SDP carries a
FreeSWITCH private address to the far end. The call sets up perfectly and has
no audio — the same failure the advertised-address note in the README warns
about, reached from the other direction.

**`record_route()` must still have run.** It is above this in `request_route`
already. If an egress INVITE escapes it, the BYE goes end to end without
touching Kamailio, `rtpengine_delete()` never fires, and the RTPEngine port
range leaks until a restart.

### What each egress type sets

| Egress | `X-Intron-Egress` | Request-URI host | Selected by |
|---|---|---|---|
| Outbound call | `carrier` | SIP host of the from-DID's trunk | the application, via the `dids` row |
| Human handoff | `carrier` | handoff target's host, or the tenant's trunk for an E.164 | the application |
| LiveKit B-leg | `livekit` | placeholder — dispatcher overwrites it | `DS_LIVEKIT`, set 2 |

One header, one branch, three call types. The alternative — inferring intent
from the shape of the Request-URI — works until a handoff target is a bare
E.164 that looks exactly like a carrier destination.

---

## Contract 6 — agent-initiated transfer, over SIP REFER

The AI agent decides, mid-call, that the caller needs a human. It tells
livekit-sip, which sends a **REFER** on the established B-leg. FreeSWITCH dials
the referred destination, bridges the caller to it, and drops the agent's leg.

Different from `destination: handoff`, which is decided from the plan *before
the call is answered* and never changes. Here the call starts on the AI leg and
moves.

### The prerequisite, and it is not optional

**REFER is an in-dialog request. It follows the Record-Route, exactly as BYE
does.** So until Kamailio stops advertising its public Elastic IP toward a
LiveKit that sits on the same private subnet, every REFER the agent sends is
dropped at the edge security group and nothing happens at all — no error, no
log, no transfer.

That is not a related bug, it is the same bug. It was found because the agent
could not hang up: LiveKit's BYE went to `63.182.163.164`, hairpinned out
through the internet gateway, came back sourced from LiveKit's own EIP, and was
refused by a security group that admits `10.70.0.30/32`. A REFER travels the
identical path.

Fix the advertised address first, or none of what follows can be tested.

### The flow

FreeSWITCH does the work. The controller is not in the path.

```
agent decides to transfer
      │
      ▼  REFER, in-dialog on the B-leg
livekit-sip ──▶ Kamailio ──▶ FreeSWITCH
      │
      ▼  FreeSWITCH dials the Refer-To destination,
      │  out through Kamailio like any other egress leg
      ▼  bridges the caller to it, drops the LiveKit leg
caller ◀────────────────────▶ human
```

The new leg leaves the platform the same way every other egress leg does — so
it needs `sip_route_uri` pointing at Kamailio and `X-Intron-Egress: carrier`,
or it bypasses the edge and RTPEngine and arrives with a private address in its
SDP. See [Contract 5](#contract-5--egress-how-freeswitch-dials-out-through-kamailio).

### What this costs, and it is accounting rather than call handling

The call works. What breaks is the record of it, and the reason is worth being
precise about.

> **Corrected 20 Sep 2026, from a live call.** This section used to say the
> controller "is blocked in `bridge`". It was not, and never had been. The
> outbound ESL socket is async: FreeSWITCH's `+OK` means the command was
> *accepted*, not that the application finished, so `bridge` returned within
> milliseconds of the call starting. Every CDR in beta recorded about two
> seconds, and every concurrency slot was released two seconds in — a cap of
> three admitting far more than three. It also made the transfer signal below
> useless, because "no hangup yet and the channel still exists" is true of
> *every* call two seconds in.
>
> The controller now tags the bridge with an `Event-UUID` and waits for the
> matching `CHANNEL_EXECUTE_COMPLETE`, so it really does block for the length
> of the call. Everything below is written against that, and is true because of
> it rather than in spite of it.

The controller is blocked in `bridge` on the caller's channel. When FreeSWITCH
transfers that channel, the bridge ends and the command returns — which without
the detection below is indistinguishable from the call being over. So the
controller would:

- **writes a CDR with `end_at` set to the transfer**, not to the hangup, and
  with no record of who took the call;
- **releases the concurrency slot** by deleting the `calls` row, while the call
  is still connected. The tenant's cap then permits one more call than it
  should, for as long as the transferred call lasts.

The second is the one that matters: a cap that is wrong in the permissive
direction is a cap that is not doing its job.

**The fix is for the controller to tell a transfer from a hangup.** Once the
bridge has genuinely completed it waits for `CHANNEL_HANGUP_COMPLETE`; on a
transfer no such event arrives, because the channel is still alive. So a
*completed* bridge with no hangup event is the signal — confirm the channel
still exists over the inbound ESL connection, and if it does, keep the row and
the socket and wait for the real ending.

The word *completed* is carrying the weight. Against the old behaviour this
test fired on every call, because the bridge "returned" immediately; it only
distinguishes anything now that the wait is real.

**Unknown, and only a real switch can settle it: whether the outbound ESL
socket survives the transfer at all.** The socket belongs to the `socket()`
application, and a transfer may end that application along with the bridge. If
it does, the controller loses the channel at the moment of transfer and cannot
observe the ending however it waits — and the CDR has to be completed by
something else, most plausibly a `CHANNEL_HANGUP_COMPLETE` seen on the inbound
ESL connection instead.

### On what the Refer-To may name

The agent chooses the destination. That is the point of agent-initiated
transfer, and it is reasonable here **because LiveKit is inside this VPC, in
this account, running code this team wrote** — it is not a third party.

Worth stating plainly because an earlier draft of this document got it wrong:
the concern with letting a REFER name an arbitrary destination is toll fraud,
and that concern is real only when whatever sends the REFER is not trusted. It
is not the case today.

It becomes the case the moment an agent runs somewhere less controlled, so the
boundary is worth knowing where it is: the `Refer-To` is the authorisation, and
nothing downstream re-checks it. If that changes, validate the destination
against the tenant's `handoff_targets` before dialling rather than after
something is billed.

### Open, and to be settled on the first real REFER

- **Whether sofia dials the Refer-To directly or drops it into the dialplan.**
  FreeSWITCH's blind transfer normally performs a dialplan lookup on the
  `Refer-To` user, which would land in the `public` context and hit the
  catch-all — opening a second socket to the controller rather than dialling
  out. If that is what happens, the dialplan needs an extension that recognises
  a transfer destination and bridges it out through Kamailio itself.
- **Whether the ESL socket survives**, as above.
- **What the caller hears during the transfer.** The AI leg ends before the
  human answers, so there is a gap. A product decision, not a technical one.
- **What happens when nobody answers.** The AI leg is gone by then and there is
  nothing to fall back to, so the call ends with a cause — and the agent should
  probably not offer a transfer a tenant cannot take.

---

## Data model

Postgres, applied by `control-plane` at startup via golang-migrate. Single
writer — only one container has the credential, which removes the "who applies
migrations" question entirely.

| Table | Holds |
|---|---|
| `tenants` | id, name, status, concurrency cap |
| `trunks` | carrier or BYOT SBC, SIP host/port, auth, source CIDRs, channel limit. See below — this is the one table with a settled shape |
| `dids` | number (E.164), tenant, plan, trunk |
| `did_ranges` | range-based allocation, for blocks bought at once |
| `inbound_plans` | destination (`ai` \| `handoff` \| `reject`), recording on/off, business hours, out-of-hours behaviour |
| `handoff_targets` | per tenant, ordered: SIP URI or E.164, hours, enabled |
| `agent_configs` | LiveKit room policy, agent identity, metadata template, per plan |
| `business_hours` | schedule rows, referenced by plans |
| `api_keys` | machine tokens, hashed, with a label so they can be rotated one at a time |
| `calls` | live calls — the concurrency ledger, with heartbeat |
| `call_history` | CDRs. Append-mostly |
| `audit_log` | config changes, who and what |

`call_history` columns worth fixing early because they are painful to
backfill: `call_uuid`, `sip_call_id`, `tenant_id`, `direction`, `did`,
`caller`, `callee`, `trunk_id`, `plan_id`, timestamps for invite/ring/answer/end,
`sip_code`, `hangup_cause`, `hangup_by`, `outcome`, `handoff_target_id`,
`livekit_room`, `recording_url`, `metadata jsonb`.

Every query carries `tenant_id`. Every cache key includes it.

### `trunks` — the shape is settled, copy it

Trunking is managed in the **application layer**, not in Kamailio config. This
is a deliberate carry-over from the contact-centre platform, where the same
decision has already survived contact with multiple carriers, so the column
set is worth copying rather than rediscovering:

```
id            tenant_id        slug            name
type          inbound | outbound | bi_directional
carrier       state            status          is_default
sip_host      sip_port         sip_username    sip_password (encrypted, bytea)
sip_auth_method   ip_whitelisting | registration
signalling_cidrs  media_cidrs    channel_size
did_range     caller_id_format   metadata jsonb
health_status health_checked_at  health_since
created_at    updated_at         deleted_at
```

Three columns carry more weight than they look like they do:

- **`sip_password` as `bytea`, encrypted at the application layer** — not
  `text`. A carrier credential in a database that gets dumped for a support
  ticket is a toll-fraud incident.
- **`sip_auth_method`** — the two carriers behave differently enough that
  branching on a free-text carrier name eventually breaks. Twilio is
  `ip_whitelisting`; a BYOT SBC that registers is `registration`.
- **`health_status` / `health_checked_at` / `health_since`** — a trunk that
  stopped accepting calls at 03:00 is a very different conversation from one
  that is merely marked inactive, and reconstructing *when* it changed after
  the fact is not possible without these.

**What this does NOT replace: the security group.** Trunk records decide
policy — may this call be admitted, which gateway does it leave by. The
security group decides whether the packet reaches the edge at all, and it is
evaluated long before any row is read. A trunk added in the database with no
matching entry in `carrier_sip_cidrs` produces calls that never arrive, with
nothing logged on the edge, because they are dropped before Kamailio sees
them. The two layers have to be changed together, and nothing enforces that.

### Default trunk — Twilio

Twilio is the default trunk for beta. Seed it as:

| Column | Value |
|---|---|
| `carrier` | `twilio` |
| `slug` | `twilio-frankfurt` |
| `type` | `bi_directional` |
| `is_default` | `true` |
| `sip_host` | `intron-beta.pstn.frankfurt.twilio.com` |
| `sip_port` | `5060` |
| `sip_auth_method` | `ip_whitelisting` |
| `sip_username` / `sip_password` | null — IP auth both directions |
| `signalling_cidrs` | the eight `/30`s in `iac/.env` |
| `media_cidrs` | `168.86.128.0/18` |
| `caller_id_format` | `e164` |
| `channel_size` | unset — no agreed concurrency cap yet |

`is_default` means *the trunk chosen for egress when no per-tenant rule
selects another*. Because trunk selection is application-layer, it happens at
originate time from this row — Kamailio gets a destination, not a configured
gateway, which is why the dispatcher has no carrier set and should not grow
one.

**Two things are Twilio-console work, not Terraform, and both are required
before a call completes in either direction:**

1. **Origination URI** → `sip:63.182.163.164:5060` (the edge Elastic IP).
   Without it Twilio has nowhere to deliver inbound calls, and the symptom is
   silence on our side — no packets, no logs, nothing to debug.
2. **Termination IP ACL** → add `63.182.163.164`. Twilio authenticates *us*
   for outbound by source address, since `sip_auth_method` is
   `ip_whitelisting`. Without it, outbound INVITEs are rejected by Twilio with
   a 403 that names our address, which at least fails loudly.

Both services expose `HEALTH_PATH` (`/healthz`) on `HEALTH_LISTEN_ADDR`. The
Ansible role checks it over loopback after a deploy; nothing else scrapes it.

The role's own comment insists the endpoint assert **dependencies**, not
liveness, and it is worth honouring:

- `control-plane` — Postgres reachable, migrations at the expected version,
  cache populated.
- `esl-controller` — Postgres reachable, and at least one FreeSWITCH node's
  inbound ESL (`:8021`) accepts and authenticates. **Not** the control-plane
  API: the controller does not need it to carry a call, so checking it would
  make the controller report unhealthy for a dependency it does not have.

**Port collision, today.** Both units render
`HEALTH_LISTEN_ADDR=0.0.0.0:8088` from the single `app_health_port`, and both
run `--network host` on the same instance. Whichever container starts second
cannot bind. The fix is one inventory change — a second variable, e.g.
`esl_controller_health_port: 8089` — and it belongs to the infra side, but it
will present as a container that restarts five times and lands in `failed`,
which reads as an application bug.

---

## Configuration

The full set of keys each service receives, from
`roles/app-service/templates/service.env.tpl.j2`. **Adding a key is an Ansible
change**, so anything the application needs must appear here first.

`control-plane`: `APP_ENV`, `AWS_REGION`, `NODE_ID`, `API_LISTEN_ADDR`,
`DATABASE_URL`, `DATABASE_MAX_CONNS`, `CONTROL_PLANE_API_TOKEN`,
`JWT_SIGNING_KEY`, `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`,
`HEALTH_LISTEN_ADDR`, `HEALTH_PATH`.

`esl-controller`: `APP_ENV`, `AWS_REGION`, `NODE_ID`, `ESL_LISTEN_ADDR`,
`ESL_TCP_KEEPALIVE_SECONDS`, `FREESWITCH_NODES`, `FREESWITCH_ESL_PORT`,
`FREESWITCH_ESL_PASSWORD`, **`DATABASE_URL`**, **`DATABASE_MAX_CONNS`**,
`LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `HEALTH_LISTEN_ADDR`,
`HEALTH_PATH`.

Three changes from what is rendered today: `DATABASE_URL` and
`DATABASE_MAX_CONNS` are added to the controller, `KAMAILIO_NODES` /
`KAMAILIO_XHTTP_PORT` are dropped from the control plane, and
`CONTROL_PLANE_API_URL` / `CONTROL_PLANE_API_TOKEN` leave the controller
entirely — it no longer calls that API.

**Set the pool sizes explicitly.** `db.t4g.micro` has 1 GiB of RAM, which is
roughly 110 Postgres connections. Two services taking a driver default can
consume most of that between them, and the failure is `FATAL: sorry, too many
clients already` on the service that started second — at which point neither
the API nor calls work, for a reason that names nothing about memory.

Both fail to start on a missing required key rather than defaulting. The secret
fetch already fails closed; the application should match it.

`CONTROL_PLANE_API_TOKEN` exists in three Secrets Manager containers
(`kamailio`, `esl-controller`, `control-plane`) and nothing keeps them in step.
The API should accept **a set** of valid tokens, not one, so a rotation can be
done in two passes instead of one synchronised restart.

---

## Build and deploy

```bash
cd services
make build                  # both binaries
make docker                 # linux/amd64, always
make push TAG=v0.1.0        # ECR, immutable
```

Then bump `image_tag` in `inventories/beta/group_vars/control_plane.yml` and
run the `control-plane` play. The tag is the deploy; there is no other lever.

Images must be `linux/amd64`. A `docker build` on Apple silicon defaults to
arm64, pushes cleanly, pulls cleanly, and then the container exits immediately
with `exec format error` — which looks like a broken image rather than a
wrong-architecture one.

---

## Infra changes this design requires

Everything in `infra/` that this document's decisions add, change or kill, in
one place. Three of the decisions are deletions, and a deletion left undone is
worse than one never proposed — the config stays in the repo looking
load-bearing, and the next person wires something to it.

None of these block writing the services. All of them block a call working end
to end.

### Add — the controller needs a database credential

[Why the split is where it is](#why-the-split-is-where-it-is). This one is a
prerequisite: without it the controller cannot carry a call at all.

| File | Change |
|---|---|
| `iac/stacks/secrets-manager/_main/main.tf` | Add `DATABASE_URL` to the `esl-controller` container's expected keys. |
| `infra/bin/db-dsn-to-sm` | Write the composed DSN into the `esl-controller` container as well as `control-plane`. Merge, do not overwrite — that container also holds the ESL password and the LiveKit keys. |
| `roles/app-service/templates/service.env.tpl.j2` | Add `DATABASE_URL=${DATABASE_URL}` and `DATABASE_MAX_CONNS` to the esl-controller block. Drop `CONTROL_PLANE_API_URL` and `CONTROL_PLANE_API_TOKEN` from it. |

No security group change: the app security group already reaches RDS on 5432
for the control plane, and both containers share the host's network.

### Delete — the admission lookup moves to the controller

[Contract 1](#contract-1--admission)

| File | Change |
|---|---|
| `roles/kamailio/templates/kamailio.cfg.j2` | Delete `route[ADMISSION]` and its call from `request_route`. Drop `loadmodule "http_client.so"` and `"jansson.so"`, and the `httpcon` / `connection_timeout` modparams. |
| `roles/freeswitch/templates/dialplan-public.xml.j2` | Delete the `intron_tenant` and `intron_plan` sets — the headers they read are no longer appended. Keep `intron_node`. |
| `inventories/beta/group_vars/all.yml` | `routing_service_url` has no consumer left. Delete it. |
| `iac/stacks/secrets-manager/_main/main.tf` | `CONTROL_PLANE_API_TOKEN` drops out of the `kamailio` and `esl-controller` containers. Two fewer copies of a credential nothing kept in step. |

Security groups are untouched. `carrier_sip_cidrs` is doing the work it always
was.

### Delete — originate goes over ESL, not HTTP

[Contract 3](#contract-3--originate)

| File | Change |
|---|---|
| `roles/kamailio/templates/kamailio.cfg.j2` | Delete `event_route[xhttp:request]` and the `/originate` stub (lines 279-287), and the `listen=tcp:...:{{ kamailio_xhttp_port }}` control listener (line 53). |
| `iac/stacks/sip-core/_main/security-groups.tf` | Delete the `edge_ctrl_http` rule (line 154). |
| `roles/app-service/templates/service.env.tpl.j2` | Drop `KAMAILIO_NODES` and `KAMAILIO_XHTTP_PORT` from the control-plane block. |

### Add — the egress branch

[Contract 5](#contract-5--egress-how-freeswitch-dials-out-through-kamailio)

| File | Change |
|---|---|
| `roles/kamailio/templates/kamailio.cfg.j2` | Add `route[EGRESS]`, and branch on source address in `request_route` so FreeSWITCH-originated INVITEs take it. Must call `route(MEDIA_OFFER)`. |
| `roles/freeswitch/templates/dialplan-public.xml.j2` | Export the next hop onto the outbound leg: `<action application="export" data="nolocal:sip_route_uri=sip:{{ edge_private_ips[0] }}:{{ sip_port }}"/>` |

No sofia gateway and no `<gateways>` block — the dial string carries the
destination, and `sip_route_uri` carries the next hop.

**The next hop belongs to FreeSWITCH, not to the controller.** The controller
never talks to Kamailio; it drives FreeSWITCH, and which edge FreeSWITCH sends
through is a fact about the FreeSWITCH node. Ansible already holds it —
`edge_private_ips` is built from the inventory — so putting a second copy in
the controller's environment would be one address in two places with nothing
keeping them in step, which is the failure class the README's "ports defined in
two places" note is about.

`nolocal:` sets the variable on the outbound leg only, which is the one making
the request. Verify the export lands on the B-leg the first time a call
bridges: without it the INVITE goes straight to whatever the Request-URI
resolves to — for a carrier host, the carrier directly — bypassing the edge and
RTPEngine, and producing a call that sets up cleanly with no audio.

### Fix — the health port collision

[Health](#health)

| File | Change |
|---|---|
| `inventories/beta/group_vars/control_plane.yml` | Add a second port, e.g. `esl_controller_health_port: 8089`. Both units render `8088` today and both run `--network host` on one instance, so the second container cannot bind. |
| `roles/app-service/templates/service.env.tpl.j2` | Render `HEALTH_LISTEN_ADDR` per service rather than from one shared variable. |

### Correct — prose that now describes the old design

| File | What is stale |
|---|---|
| `README.md` | The architecture blurb has Kamailio asking the control plane whether a number is admitted. Phase 3's note that the API must cache "because Kamailio fails closed" is right about the cache and wrong about why. |
| `docs/ROLLOUT.md` | Stage 4's warning that single-AZ RDS is a single point of failure for the whole platform, because Kamailio rejects every INVITE when the API cannot answer. The blast radius is now one call. |

---

## Things that will fail silently

Ordered by how long they cost.

**A stale `intron_tenant` read.** The dialplan still sets
`intron_tenant=${sip_h_X-Intron-Tenant}` from a header Kamailio no longer
appends, so the variable exists and is empty. Code that reads it instead of
calling `/v1/route` gets `""`, and an empty tenant scopes every query to
nothing — no error, just a call that behaves as though the tenant has no
configuration. Delete the lines from the dialplan rather than leaving them to
be believed.

**A rejection with no hangup cause.** The call ends, the caller hears nothing
useful, and the CDR is indistinguishable from a normal clearing. See the cause
table in [Contract 4](#contract-4--the-esl-call-lifecycle).

**An egress INVITE that skips `route(MEDIA_OFFER)`.** The SDP carries a
FreeSWITCH private address out to the carrier. Signalling is clean end to end,
the call answers, and there is no audio in one direction.

**A missing `origination_caller_id_number`.** The `From` is whatever FreeSWITCH
defaults to. Some carriers reject it, some deliver the call presenting a number
the tenant does not own, and the second is worse because it works.

**A missing `myevents` subscribe.** The call connects, and the controller never
learns it ended. The socket stays open, the concurrency slot is never released,
and no CDR is ever written.

**No TCP keepalive on accepted sockets.** Calls drop at a round interval, most
often ones that were put on hold. Looks like a media problem.

**A cache with no serve-stale.** A ten-second RDS blip becomes a ten-second
total inbound outage, because Kamailio fails closed on every call in that
window.

**Leaked live-call rows.** A controller crash leaves rows claimed forever, and
the tenant's effective capacity drops silently until someone counts rows by
hand.

**Both health endpoints on 8088.** The second container never starts; the first
is healthy and everything looks fine from the outside.

---

## Open questions

1. **Egress admission policy.** The egress *path* is settled
   ([Contract 5](#contract-5--egress-how-freeswitch-dials-out-through-kamailio));
   what is not is whether anything checks a tenant may dial a given
   destination, and where. Not blocking, but it is the toll-fraud surface.
2. **`sip_route_uri` against this FreeSWITCH build.** Contract 5 leans on it to
   keep carrier addresses out of Ansible. Verify before the first outbound
   call; the fallback is a per-trunk sofia gateway.
3. **How the AI asks for a human.** LiveKit data channel back to the controller,
   a webhook into the control plane, or a DTMF fallback? This decides whether
   the controller needs an inbound HTTP surface of its own.
4. **Whether `/v1/route` should return the dispatcher set.** It does not today —
   Kamailio picks the FreeSWITCH node itself, because it is the only thing that
   knows which nodes are answering probes. Keep it that way unless there is a
   reason not to.
5. **Operator auth.** `JWT_SIGNING_KEY` is provisioned, but there is no
   identity provider and no admin UI in this repo. Self-issued tokens for beta,
   or wait?
