output "state_bucket_name" {
  description = "Put this in every other stack's backend `bucket`."
  value       = aws_s3_bucket.tfstate.id
}

output "backend_block" {
  description = <<-DESC
    Copy-paste backend block for a new stack. Replace <stack> and <env>.

    `use_lockfile = true` needs Terraform >= 1.10. On an older CLI the block
    still parses but locking silently does nothing — two concurrent applies
    would then race and one would lose its changes. Check `terraform version`
    before trusting it.
  DESC
  value       = <<-EOT
    terraform {
      backend "s3" {
        bucket       = "${aws_s3_bucket.tfstate.id}"
        key          = "stacks/<stack>/<env>/terraform.tfstate"
        region       = "${var.aws_region}"
        encrypt      = true
        use_lockfile = true
      }
    }
  EOT
}
