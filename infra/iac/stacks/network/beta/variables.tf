variable "aws_region" {
  type        = string
  description = <<-DESC
    Region for the whole platform.

    eu-central-1 (Frankfurt), chosen because it is the region this account
    already operates in. Consistency is the reason, and it is a good one: a
    second region means a second place to look for a VPC endpoint, a second set
    of AMI IDs, and cross-region data transfer the first time anything talks to
    something that was already there.

    WHY NOT af-south-1 (Cape Town), despite being far closer to Lagos:

      1. Carrier presence. Twilio's Elastic SIP Trunking terminates in a fixed
         set of edge locations; the European ones are among them and Cape Town
         is not. A trunk homed in af-south-1 still hairpins through Europe, so
         the "closer" region adds a leg rather than removing one.
      2. Service coverage. af-south-1 does not carry every service or instance
         family, and discovering a gap mid-build is expensive.
      3. Price. af-south-1 runs materially above the European regions on both
         compute and data transfer.

    Frankfurt vs Ireland is close to a wash for this workload — both are major
    interconnect hubs with similar onward paths to West Africa, and the
    difference is a few milliseconds either way. Not worth optimising against
    the cost of splitting the account across regions.

    One-way mouth-to-ear latency from Lagos to Frankfurt lands in the 80-110ms
    range. That is inside the ITU G.114 budget for a conversation and is what
    this architecture is sized against. Revisit if Intron moves to in-country
    termination, where the calculus changes completely.
  DESC
  default     = "eu-central-1"
}

variable "vpc_cidr" {
  type        = string
  description = "See the same variable in ../_main/variables.tf — the LiveKit overlap warning is the important part."
  default     = "10.70.0.0/16"
}

variable "aws_profile" {
  type        = string
  description = <<-DESC
    Shared-config profile Terraform authenticates with, from ~/.aws/credentials.

    Named explicitly in the provider block rather than left to the default
    credential chain, so a bare `terraform plan` in this directory picks the
    same identity a `make plan` does. An auth model that only works through the
    Makefile breaks the first time someone debugs a stack directly — and it
    fails by reaching for whatever AWS_PROFILE happens to be set, which on a
    laptop with several accounts configured is how changes land in the wrong
    one.

    NOTE the two places this value also appears and why they differ:
      - backend "s3"       a LITERAL, because backend blocks cannot
                           interpolate. Override with
                           `terraform init -backend-config=profile=<other>`.
      - terraform_remote_state
                           passed through as a variable. A remote_state data
                           source configures its own backend and inherits
                           NOTHING from the provider block.

    The profile carries no region (see ~/.aws/config), which is fine for
    Terraform — region comes from var.aws_region and from the backend literal —
    but means any bare `aws` CLI call needs AWS_REGION set. The Makefile
    exports it.
  DESC
  default     = "klarity_aws"
}
