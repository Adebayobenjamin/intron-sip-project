########################################################
# Re-export the module's outputs at the stack's top level.
#
# terraform_remote_state reads the ROOT module's outputs — anything left
# unexported here is invisible to sip-core and control-plane, however clearly
# it is defined in _main. This file is the actual contract between stacks.
########################################################

output "vpc_id" {
  value = module.network.vpc_id
}

output "vpc_cidr" {
  value = module.network.vpc_cidr
}

output "availability_zones" {
  value = module.network.availability_zones
}

output "subnet_ids" {
  value = module.network.subnet_ids
}

output "subnet_cidrs" {
  value = module.network.subnet_cidrs
}

output "edge_subnet_ids" {
  value = module.network.edge_subnet_ids
}

output "app_subnet_ids" {
  value = module.network.app_subnet_ids
}

output "sip_subnet_ids" {
  value = module.network.sip_subnet_ids
}

output "data_subnet_ids" {
  value = module.network.data_subnet_ids
}

output "nat_public_ips" {
  value = module.network.nat_public_ips
}

output "private_route_table_ids" {
  value = module.network.private_route_table_ids
}
