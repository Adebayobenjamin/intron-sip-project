output "secret_arns" {
  description = <<-DESC
    Container ARNs keyed by component. Downstream stacks scope their instance
    IAM policies against these rather than constructing the ARN by hand.

    Construct-by-hand is the thing to avoid here: Secrets Manager appends a
    random six-character suffix to every ARN, so a literal
    "arn:...:secret:intron-sip/beta/kamailio" matches nothing and GetSecretValue
    fails with AccessDenied naming a resource that looks correct.
  DESC
  value       = { for k, v in aws_secretsmanager_secret.this : k => v.arn }
}

output "secret_names" {
  description = "Container names keyed by component — the --secret-id you pass to the CLI and to fetch-and-render-secrets.sh."
  value       = { for k, v in aws_secretsmanager_secret.this : k => v.name }
}
