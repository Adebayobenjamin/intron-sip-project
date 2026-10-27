# Phase 1 — Infrastructure rollout

Six stages (0-5), each with a **gate**: a check that must pass before starting
the next one. The gates exist because the failures here are mostly silent — an
instance that never registered with SSM looks identical to one that did until
you try to reach it, and by then you have built two more stages on top.

Run every command from `infra/iac` unless a stage says otherwise. Credentials
come from the `intron_aws` profile named in the config - in every Terraform
provider, backend and remote_state block, and in the Ansible inventory. Nothing
needs exporting and there is no MFA wrapper.

| Stage | Stack(s) | Creates | Added cost | Gate |
|---|---|---|---|---|
| **0** | `tfstate`, `secrets-manager` | State bucket, 6 secret containers | ~$2/mo | `make populate-iac` round-trips |
| **1** | `network` | VPC, 8 subnets, NAT, endpoints | ~$42/mo | Subnets in 2 AZs, S3 endpoint on every route table |
| **2** | `ecr` | 2 image repositories | ~$0 | Repos exist, login works |
| **3** | `sip-core` | Edge + FreeSWITCH EC2, EIP, SGs, IAM | ~$234/mo | **Both instances Online in SSM** |
| **4** | `database` | RDS Postgres 16, subnet group, SG | ~$17/mo | Instance `available`, DSN written |
| **5** | `control-plane` | 1 app EC2, private DNS | ~$88/mo | DNS resolves *from inside the VPC* |

**Steady state ≈ $384/month**, excluding data transfer. Figures are eu-central-1
on-demand, pulled from the Pricing API when the platform ran there. eu-west-1
is usually slightly cheaper for the same instance types, so treat them as the
right order of magnitude, not a quote.

Stages are operational, not structural. There are no "stage" directories: the
stacks already sit at the right granularity, and renaming them would change
every backend `key` and orphan the state.

---

## Preflight

```bash
cd infra/iac && make preflight
```

Checks tooling versions, that the profile authenticates, and that the region
resolves. Two decisions it cannot check for you:

**`vpc_cidr` should not overlap Intron's network.** `10.70.0.0/16` today.
**Not blocking** — our LiveKit server runs inside this VPC, and Intron's agent
reaches it over the internet, which does not care about overlap (see
[LIVEKIT.md](LIVEKIT.md)). Intron's GPU box is in `172.31.0.0/16`, so there is
no overlap now. Keep it that way: peering with their VPC, to keep the agent's
audio off the internet, becomes impossible without renumbering if the ranges
ever collide.

**`carrier_sip_cidrs` is empty**, so no calls will arrive. Not blocking for any
apply — the edge comes up fine without it — but nothing rings until it is
populated from Twilio's current eu-central signalling ranges.

---

## Stage 0 — Bootstrap

State bucket and secret containers. Everything else depends on both.

### Apply

The state bucket cannot store its own state before it exists, so this stack
applies against a local backend and then **adopts the bucket it just created**:

```bash
cd stacks/tfstate/beta
terraform init && terraform apply

$EDITOR main.tf                    # uncomment the backend "s3" block
terraform init -migrate-state      # answer yes; copies local state into S3
rm -f terraform.tfstate terraform.tfstate.backup
cd ../../..

make apply STACK=secrets-manager ENV=beta
```

### Gate

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1

# Versioning is what makes a corrupted state file recoverable. If this says
# anything other than Enabled, stop — every later stage writes state here.
aws s3api get-bucket-versioning --bucket intron-sip-tfstate-eu-west-1

# This stack's own state should now be IN the bucket.
aws s3 ls s3://intron-sip-tfstate-eu-west-1/stacks/tfstate/beta/

aws secretsmanager list-secrets \
  --query 'SecretList[?starts_with(Name,`intron-sip/beta/`)].Name' --output table

# The real gate: prove the read path every later stack depends on.
cp .env.example .env && make populate-iac ENV=beta
make plan STACK=network ENV=beta      # must reach "Terraform will perform"
```

That last pair matters more than it looks. Every stack except these two is
wrapped in `aws-env-run intron-sip/beta/iac`, which reads the secret and
exports `TF_VAR_*`. If the container is missing or unreadable, `make plan`
fails before Terraform starts — and the error names the secret, not the wrapper.

### Rollback

```bash
make destroy STACK=secrets-manager ENV=beta
```

**Gotcha:** secrets have a 7-day recovery window, so re-applying within a week
fails with *"a secret with this name is already scheduled for deletion"*. To
reuse the names immediately:

```bash
for s in iac ansible kamailio freeswitch esl-controller control-plane; do
  aws secretsmanager delete-secret --secret-id intron-sip/beta/$s \
    --force-delete-without-recovery
done
```

The state bucket carries `prevent_destroy = true`, so `terraform destroy` on
`tfstate` **will fail by design**. That is deliberate — losing it means losing
the record of every resource in the account. To actually remove it, delete the
`lifecycle` block first.

---

## Stage 1 — Network

VPC, 8 subnets across 2 AZs, IGW, one NAT gateway, S3 gateway endpoint.

**First stage that costs real money**, and it starts the moment the NAT gateway
exists — $0.052/hr whether anything uses it or not, plus $0.052/GB processed
and $3.65/mo for its public IPv4 address.

```bash
make plan  STACK=network ENV=beta      # read it; 27 resources
make apply STACK=network ENV=beta
```

### Gate

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1

# Set this FIRST — without it the filters below read Values= and match nothing,
# which looks like an empty VPC rather than an unset variable.
VPC=$(make output STACK=network ENV=beta EXTRA="-raw vpc_id")

# 8 subnets: edge/app/sip/data × 2 AZs. Fewer means az_count did not take.
aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC" \
  --query 'Subnets[].[Tags[?Key==`Name`]|[0].Value,AvailabilityZone,CidrBlock]' \
  --output table

# The S3 gateway endpoint must be on EVERY route table. If it is missing, the
# instances in Stage 3 pull their SSM agent over the NAT gateway instead —
# which works, bills per GB, and hides the fact that the endpoint is broken.
aws ec2 describe-vpc-endpoints --filters "Name=vpc-id,Values=$VPC" \
  --query 'VpcEndpoints[].[ServiceName,VpcEndpointType,length(RouteTableIds)]' --output table

aws ec2 describe-nat-gateways --filter "Name=vpc-id,Values=$VPC" \
  --query 'NatGateways[].[NatGatewayId,State]' --output table
```

Expect: 8 subnets, one Gateway endpoint attached to 3 route tables (2 private +
1 public), one NAT gateway `available`.

### Rollback

`make destroy STACK=network ENV=beta` — clean, nothing is protected here.

---

## Stage 2 — ECR

Two image repositories with immutable tags and lifecycle policies.

Cheap and out of the way. Nothing to push yet — the backend services do not
exist — but having the repos means CI has somewhere to go the moment they do.

```bash
make apply STACK=ecr ENV=beta
```

### Gate

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1

make output STACK=ecr ENV=beta EXTRA="repository_urls"

aws ecr get-login-password | docker login --username AWS \
  --password-stdin "$(aws sts get-caller-identity --query Account --output text).dkr.ecr.eu-west-1.amazonaws.com"
```

---

## Stage 3 — SIP core

The edge (Kamailio + RTPEngine, public subnet, Elastic IP) and the FreeSWITCH
farm (private subnet, no public address). Largest cost step: ~$234/mo.

```bash
make plan  STACK=sip-core ENV=beta
make apply STACK=sip-core ENV=beta
```

### Gate — the important one

**These instances have no SSH key, no port 22 rule and no bastion. SSM is the
only way in.** If the agent does not register, the box is unreachable and the
only remedy is to replace it. Check before doing anything else:

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1

# Give it ~3-5 minutes after apply. Both instances must appear, PingStatus Online.
aws ssm describe-instance-information \
  --query 'InstanceInformationList[].[InstanceId,ComputerName,IPAddress,PingStatus]' \
  --output table
```

If an instance is missing, **do not rebuild blindly** — read why first:

```bash
aws ec2 get-console-output --instance-id i-xxxx --output text | tail -60
```

`user_data` logs to `/var/log/vm-bootstrap.log` and echoes to the console. The
likely causes, in order: the S3 gateway endpoint missing so the agent `.deb`
could not be fetched (Stage 1 gate would have caught it), no NAT route from the
private subnet, or the instance profile not attached.

Then confirm the rest:

```bash
# Note this down — it goes to the carrier, and Kamailio/RTPEngine advertise it.
make output STACK=sip-core ENV=beta EXTRA="-raw edge_public_ips"

# Paste into infra/ansible/inventories/beta/hosts.yml
make output STACK=sip-core ENV=beta EXTRA="-raw ansible_inventory_hint"

# Only needed for a RESTRICTED operator identity - the minimum an Ansible
# operator requires. Skip it if yours holds AdministratorAccess.
make output STACK=sip-core ENV=beta EXTRA="-raw operator_iam_policy_snippet"
```

### Rollback

**`terraform destroy` will fail here by design.** The Elastic IP carries
`prevent_destroy = true`, because releasing it means AWS will not give the same
address back and every carrier allowlist referencing it is silently wrong. To
tear the stack down deliberately, remove the `lifecycle` block in
`stacks/sip-core/_main/sip-edge.tf` first.

---

## Stage 4 — Database

RDS Postgres for the control plane: tenants, DIDs, trunks, handoff targets.
Configuration data, not call records.

Separate stack from `control-plane` deliberately — a database outlives the
application in front of it, and tearing down the app tier must not be able to
take the data with it.

```bash
make apply STACK=database ENV=beta
```

~$17/mo (`db.t4g.micro` $14 + 20 GB gp3). Creation takes **5-10 minutes**.

### Read this before sizing it down

**The blast radius is one call, not the platform.** This used to read that
Kamailio failed closed — no answer from the control-plane API meant a 503 on
every inbound INVITE, which made this single-AZ database a single point of
failure for everything. That is no longer true: Kamailio asks nothing, and
`route[ADMISSION]` does not exist. Admission is resolved by the ESL controller
in-process against this database, with the call already answered at the edge
and in hand.

So a blip now costs the calls that arrive during it, one decision at a time,
rather than closing the door. Still worth fixing, in this order: a **controller-
side cache with serve-stale** of tenant/DID/plan data — it changes on human
timescales and is read on every call — and then `multi_az = true`, which
roughly doubles instance cost and is a one-variable change when real traffic
depends on this.

Note this database is now on the call path for TWO services rather than one:
the controller and the API both hold a DSN, and `db.t4g.micro` has a low
`max_connections`. See `esl_controller_db_max_conns`.

### The password is never in Terraform state

`manage_master_user_password = true` hands the credential to RDS, which
generates it and holds it in its own Secrets Manager secret. Terraform never
sees the value, so it never lands in state — including every historical version
in the state bucket.

The application still reads one container, as everything else here does. An
operator composes the DSN once:

```bash
../../bin/db-dsn-to-sm beta          # from infra/iac
```

That reads the endpoint from state and the password from the RDS secret,
assembles the DSN in memory, and **merges** `DATABASE_URL` into
`intron-sip/beta/control-plane`. Merge, not overwrite — that container also
holds `CONTROL_PLANE_API_TOKEN`, `JWT_SIGNING_KEY` and the Twilio credentials,
and clobbering them would surface as the API rejecting every Kamailio request.

**Re-run it if the password is ever rotated.**

### Gate

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1

aws rds describe-db-instances --db-instance-identifier beta-intron-sip-db \
  --query 'DBInstances[].[DBInstanceStatus,Engine,EngineVersion,MultiAZ,PubliclyAccessible,StorageEncrypted]' --output table
```

Expect `available`, `postgres`, `16.x`, `MultiAZ=False`, and — the two that
matter — `PubliclyAccessible=False` and `StorageEncrypted=True`.

```bash
# DSN present without printing it
aws secretsmanager get-secret-value --secret-id intron-sip/beta/control-plane \
  --query SecretString --output text | jq -r 'keys | join(", ")'
```

`DATABASE_URL` must appear in that list.

**Connectivity is not testable yet** — the security group admits the app
subnets only, and no app instance exists until Stage 5. That check is part of
Stage 5's gate.

### Rollback

`terraform destroy` **fails by design**: `deletion_protection = true`. Set it
false and apply that first, then destroy — two deliberate steps to delete the
only state here that cannot be rebuilt from code. A final snapshot is taken
regardless.

---

## Stage 5 — Control plane

One app instance running the ESL controller and the control-plane API, plus the
private hosted zone FreeSWITCH resolves its `&socket()` target through.

```bash
make apply STACK=control-plane ENV=beta
```

**~$88/mo** — one `m6i.large` ($84), 40 GB gp3, private hosted zone ($0.50).

**No load balancer.** FreeSWITCH resolves `esl.beta.intron.internal` straight
to the instance. See the README for why that trade was taken, and what it costs
(a controller restart ends the calls it is holding).

### Gate

The hosted zone is private by design, so resolution has to be checked from
inside the VPC:

```bash
export AWS_PROFILE=intron_aws AWS_REGION=eu-west-1
FS=$(make output STACK=sip-core ENV=beta EXTRA="-raw freeswitch_instance_ids" | tr -d '[]", ')

CMD=$(aws ssm send-command --document-name AWS-RunShellScript \
  --targets "Key=InstanceIds,Values=$FS" \
  --parameters 'commands=["getent hosts esl.beta.intron.internal","getent hosts api.beta.intron.internal","timeout 3 bash -c \"</dev/tcp/10.70.10.10/8022\" 2>&1 || true"]' \
  --query Command.CommandId --output text)

sleep 10
aws ssm get-command-invocation --command-id "$CMD" --instance-id "$FS" \
  --query StandardOutputContent --output text
```

Both names must resolve to `10.70.10.10`. If they do not, the private hosted
zone is not associated with the VPC, or `enable_dns_hostnames` is off — and the
symptom much later is every inbound call dropping at answer with nothing in the
controller's log, because FreeSWITCH could not open its `&socket()`.

**Expect port 8022 to be refused.** Nothing is deployed yet. `Connection
refused` is the *right* failure — it means the packet reached the instance and
the security group permitted it. A blocked rule would time out instead, and
that distinction is the quickest way to tell a missing container from a missing
rule.

### Then the database check deferred from Stage 4

Run it from the *app* instance, the only tier the database security group
admits:

```bash
APP=$(make output STACK=control-plane ENV=beta EXTRA="-raw app_instance_ids" | tr -d '[]", ')
DBH=$(make output STACK=database ENV=beta EXTRA="-raw db_address")

CMD=$(aws ssm send-command --document-name AWS-RunShellScript \
  --targets "Key=InstanceIds,Values=$APP" \
  --parameters "commands=[\"timeout 5 bash -c '</dev/tcp/$DBH/5432' && echo PG_REACHABLE || echo PG_UNREACHABLE\"]" \
  --query Command.CommandId --output text)

sleep 8
aws ssm get-command-invocation --command-id "$CMD" --instance-id "$APP" \
  --query StandardOutputContent --output text
```

`PG_REACHABLE` confirms the subnet group, the security group and the data-tier
routing all line up.

```bash
make output STACK=control-plane ENV=beta EXTRA="-raw esl_target"
make output STACK=control-plane ENV=beta EXTRA="-raw ansible_inventory_hint"
```

---

## Stage 6 — LiveKit

Our own LiveKit server for the AI leg. Why it exists, and how it connects to
Intron's agent, is in [LIVEKIT.md](LIVEKIT.md).

Needs `sip-core` (for the edge security group and the Ansible transport
bucket) and a populated `intron-sip/beta/livekit` secret:

```bash
printf 'LIVEKIT_API_KEY=API%s\nLIVEKIT_API_SECRET=%s\n' \
  "$(openssl rand -hex 6)" "$(openssl rand -base64 32)" > /tmp/livekit.env
../bin/env-file-to-sm intron-sip/beta/livekit /tmp/livekit.env && rm /tmp/livekit.env

make apply STACK=livekit ENV=beta
```

**~$17.50/mo** — one `t3.small`, 20 GB gp3, a public hosted zone ($0.50).

### The manual step: DNS delegation

The stack creates the public zone `intron.quiverlearn.com`, but nothing
resolves until the parent domain points at it. `quiverlearn.com` is hosted on
**Cloudflare** (Namecheap is only the registrar), so add four NS records there,
all with name `intron`:

```bash
make output STACK=livekit ENV=beta EXTRA="livekit_zone_nameservers"
```

Until this is done there is no certificate, and the error you see is about TLS,
not DNS.

### Gate

```bash
dig +short livekit-beta.intron.quiverlearn.com @1.1.1.1        # the Elastic IP
curl -s -o /dev/null -w '%{http_code}\n' \
  https://livekit-beta.intron.quiverlearn.com/rtc/validate     # 401
```

**401 is the pass.** It means DNS, the certificate and LiveKit all work and the
server is asking for a token. Then paste the inventory fragment and run the
play, which also creates the SIP trunk and dispatch rule:

```bash
make output STACK=livekit ENV=beta EXTRA="-raw ansible_inventory_hint"
cd ../ansible && make play PLAYBOOK=livekit ENV=beta
```

The `livekit` play is **not** in `site.yml`. Run it on its own, before the
`edge` play, so Kamailio's dispatcher has something to reach.

---

## Phase 1 complete — on to Phase 2

The infrastructure exists and nothing is configured. Next is Ansible, in the
order `site.yml` uses and for the same reason — the ESL listener must accept
before FreeSWITCH starts dialling it, and Kamailio comes up last because
carriers can deliver the moment it does:

```
control-plane → freeswitch → edge
```

Ansible authenticates with the same `intron_aws` profile, set as
`ansible_aws_ssm_profile` in the inventory. Prove the path works first:

```bash
cd infra/ansible
make galaxy boto-deps          # one-time
make ping TARGET=all ENV=beta  # expect SUCCESS from all three hosts
```

Then, knowing the two ordering quirks:

**`freeswitch` first, not `control-plane`.** The controller runs a container
that does not exist until Phase 3, so its play fails its own preflight on the
image tag. FreeSWITCH is the one that can run today.

**Skip the ESL probe on that first run.** The FreeSWITCH role ends by checking
it can reach the controller, which is not up yet — and it would fail at the end
of a ~30 minute source build:

```bash
make play PLAYBOOK=freeswitch ENV=beta EXTRA_VARS='verify_esl_target=false'
```

Turn it back on afterwards. It is what catches a stale target later, which is
the case that actually costs calls.

---

## Stopping the meter

Tear down in **reverse stage order** — later stacks read earlier ones through
remote state, and destroying `network` first leaves `sip-core` unable to plan.

```bash
make destroy STACK=test-pbx      ENV=beta   # if it still exists
make destroy STACK=livekit       ENV=beta
make destroy STACK=control-plane ENV=beta
make destroy STACK=database      ENV=beta   # set deletion_protection=false and apply first
make destroy STACK=sip-core      ENV=beta   # remove the EIP prevent_destroy first
make destroy STACK=ecr           ENV=beta
make destroy STACK=network       ENV=beta
make destroy STACK=secrets-manager ENV=beta # then force-delete, see Stage 0
```

To pause rather than destroy, stopping the EC2 instances removes the compute
charge (~$297/mo of the total) but **not** EBS, the NAT gateway, RDS, or public
IPv4 addresses — roughly $70/mo continues. Destroying stages 3-5 and keeping
0-2 costs about $44/mo and leaves the network and state intact to rebuild onto.

RDS is the one that keeps billing while "stopped": an RDS instance can be
stopped for at most 7 days before AWS restarts it automatically. To genuinely
pause it, take a final snapshot and destroy the stack.
