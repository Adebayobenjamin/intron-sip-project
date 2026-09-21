variable "environment" {
  type        = string
  description = <<-DESC
    Environment name. Used for tagging only — the repository NAMES are not
    env-scoped.

    One repository per service across all environments, with environments
    separated by tag. The alternative, a repo per env, means the image that
    passed testing in one environment is not the image promoted to the next:
    it gets rebuilt, and a rebuild is a different artifact however identical
    the source. Promotion should be a tag, not a build.
  DESC
}

variable "release_tag_prefixes" {
  type        = list(string)
  description = <<-DESC
    Tag prefixes treated as releases by the lifecycle policy — these survive on
    a keep-last-30 rule rather than expiring after 14 days.

    Anything CI tags a deployable build with belongs here. If a prefix used in
    practice is missing from this list, those images quietly expire after two
    weeks and the rollback target you reach for during an incident is gone.
  DESC
  default     = ["v", "release-", "beta-"]
}
