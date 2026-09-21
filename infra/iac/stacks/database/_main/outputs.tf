########################################################
# NOTE WHAT IS NOT HERE: the password, and any DSN containing it.
#
# A Terraform output is stored in state in cleartext, so emitting a composed
# DATABASE_URL would put the credential in the state bucket permanently and
# undo the whole point of manage_master_user_password.
#
# The pieces are exposed instead, and bin/db-dsn-to-sm assembles them with the
# password read straight from the RDS-managed secret, writing the result into
# intron-sip/<env>/control-plane. Nothing in between touches disk.
########################################################

output "db_endpoint" {
  description = "host:port. Private only - resolvable and reachable from the app subnets, nowhere else."
  value       = aws_db_instance.main.endpoint
}

output "db_address" {
  description = "Hostname without the port."
  value       = aws_db_instance.main.address
}

output "db_port" {
  value = aws_db_instance.main.port
}

output "db_name" {
  value = aws_db_instance.main.db_name
}

output "master_username" {
  description = "Master user. The application should use a least-privilege role created in the first migration, not this."
  value       = aws_db_instance.main.username
}

output "master_user_secret_arn" {
  description = <<-DESC
    The RDS-managed Secrets Manager secret holding the master password.

    Terraform never sees the value - RDS generates and stores it. Read by
    bin/db-dsn-to-sm when composing the DSN, and by an operator who needs a
    psql session.

    The APPLICATION does not read this. It reads DATABASE_URL from
    intron-sip/<env>/control-plane, so the app tier needs no extra IAM grant
    and no second code path.
  DESC
  value       = try(aws_db_instance.main.master_user_secret[0].secret_arn, null)
}

output "db_security_group_id" {
  value = aws_security_group.db.id
}

output "dsn_command" {
  description = <<-DESC
    Copy-paste command to write DATABASE_URL into the control-plane secret
    container. Run after the first apply, and again if the password is rotated.
  DESC
  value       = "../../../bin/db-dsn-to-sm ${var.environment}"
}
