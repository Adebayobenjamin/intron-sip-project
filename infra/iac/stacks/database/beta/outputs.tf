output "db_endpoint" {
  value = module.database.db_endpoint
}

output "db_address" {
  value = module.database.db_address
}

output "db_port" {
  value = module.database.db_port
}

output "db_name" {
  value = module.database.db_name
}

output "master_username" {
  value = module.database.master_username
}

output "master_user_secret_arn" {
  description = "RDS-managed secret holding the master password. Terraform never sees the value."
  value       = module.database.master_user_secret_arn
}

output "db_security_group_id" {
  value = module.database.db_security_group_id
}

output "dsn_command" {
  value = module.database.dsn_command
}
