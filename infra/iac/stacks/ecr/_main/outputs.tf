output "repository_urls" {
  description = "Push/pull URLs keyed by service. These are what the systemd units and CI reference."
  value       = { for k, v in aws_ecr_repository.this : k => v.repository_url }
}

output "repository_arns" {
  description = "For the app tier's instance role — ecr:BatchGetImage and friends are scoped to these rather than to *."
  value       = { for k, v in aws_ecr_repository.this : k => v.arn }
}
