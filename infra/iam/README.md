# IAM access to deploy and operate the Intron SIP platform

Two customer-managed policies for the person deploying this repo. **Attach
both.** They are split only because one would exceed AWS's 6,144-character
limit for a managed policy.

| Policy | Covers |
|---|---|
| `intron-sip-deployer-infra.json` | What Terraform builds: VPC, EC2, IAM instance roles, RDS, Route53, and the guardrails |
| `intron-sip-deployer-ops.json` | Running it: Terraform state, Ansible's transfer bucket, secrets, container images, SSM shell access |

Both pass IAM Access Analyzer's `validate-policy` with no findings. They contain
no account ID, so they can be created in any account unchanged.

---

## What the repo needs

Derived from the Terraform stacks under `infra/iac/stacks/`, the scripts in
`infra/bin/`, and Ansible, which connects to instances over SSM (no SSH):

| Service | Used for |
|---|---|
| EC2 / VPC | VPC, subnets, NAT gateway, Elastic IPs, security groups, 4–5 instances, an EBS volume |
| IAM | One role and instance profile per server tier; nothing for people |
| RDS | One PostgreSQL instance, its subnet and parameter groups, a final snapshot on delete |
| Route53 | A private zone (`beta.intron.internal`) and a public zone for LiveKit |
| S3 | The Terraform state bucket, and Ansible's file-transfer bucket |
| Secrets Manager | `intron-sip/*` containers, plus RDS's own managed master password |
| ECR | `intron-sip/*` image repositories: create, push, pull |
| SSM | Session Manager shells, port forwarding (to reach the control-plane API, which listens only inside the VPC), and `send-command`, to this project's instances only |

---

## Guardrails built in

- **Region.** Every regional action is denied outside **eu-west-1**. Only
  IAM, Route53 and STS (global services) are exempt.
- **Instances and volumes.** Stop, start, reboot, terminate, modify, and
  volume attach/detach/delete are **denied** on anything not tagged
  `project=intron-sip`. Every stack applies that tag to everything it creates.
- **Shell access.** `ssm:StartSession` and `ssm:SendCommand` work only on
  instances tagged `project=intron-sip`.
- **Names.** IAM roles and instance profiles are limited to `*-intron-*`,
  secrets to `intron-sip/*`, ECR to `intron-sip/*`, RDS to `*-intron-sip-*`, and
  S3 to the two project buckets.
- **Managed policies.** The deployer can attach only
  `AmazonSSMManagedInstanceCore` and `CloudWatchAgentServerPolicy` to roles,
  and can pass roles only to EC2.
- **RDS passwords.** Only the master-password secret belonging to this
  project's database can be read, matched on the tag RDS puts on that secret.

---

## Known gaps, stated plainly

**1. Creating IAM roles is close to admin rights.** The deployer can create an
`*-intron-*` role, give it any inline policy (`iam:PutRolePolicy`) and a trust
policy naming themselves, then use it. That is a privilege-escalation path,
and name-scoping doesn't close it. The standard fix is a **permissions
boundary**: you create a boundary policy, these policies require it on every
role the deployer creates, and the Terraform sets `permissions_boundary` on its
four `aws_iam_role` resources. That's a small code change on our side; ask and
we'll add it.

**2. Network changes aren't tag-limited.** VPC, subnet, route and
security-group actions are limited to eu-west-1 but not to this project's
resources. If you run anything else in eu-west-1, the deployer could modify
it.

**3. Route53 isn't limited to our zones.** A new zone can't be scoped in
advance. Once both zones exist, you can restrict `ChangeResourceRecordSets` and
`DeleteHostedZone` to their two zone ARNs.

**The cleanest answer to all three is a dedicated AWS account** (for example a
member account in your Organization). Inside its own account, the deployer
can't affect anything else of yours, and these policies become a safety net
rather than the only line of defence.

---

## Not covered by IAM

- **DNS delegation.** The LiveKit hostname needs NS records added wherever the
  parent domain is hosted (currently Cloudflare for `quiverlearn.com`).
- **Carrier configuration.** Twilio's Origination URI and Termination IP ACL
  must point at the new edge Elastic IP.
- **Access to Intron's GPU box.** The agent worker is installed over SSH
  (`infra/bin/intron-agent-bridge.sh`) and needs no AWS permissions.

## Changing region

Replace `eu-west-1` in `DenyOutsideDeployRegion` (in the infra policy). The
Terraform also hard-codes eu-west-1 in its backends and bucket names, so a
region change is a repo change as well.
