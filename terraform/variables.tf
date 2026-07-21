variable "name" {
  description = "Stable name for the Bleephub service and its AWS resources."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "name must start with a lowercase letter and contain only lowercase letters, digits, and hyphens."
  }
}

variable "region" {
  description = "AWS region for the Bleephub task, buckets, and supporting infrastructure."
  type        = string
  default     = "eu-west-1"
}

variable "vpc_cidr" {
  description = "IPv4 CIDR allowed by Bleephub task and dqlite security groups. It is the module-created VPC CIDR unless existing_vpc_id is supplied."
  type        = string
  default     = "10.86.0.0/16"
}

variable "existing_vpc_id" {
  description = "Existing VPC ID. Set it together with existing private/public subnets to place Bleephub in a shared network instead of creating a VPC."
  type        = string
  default     = ""
}

variable "existing_private_subnet_ids" {
  description = "Private subnet IDs for Bleephub tasks, dqlite EFS mount targets, and the Amazon API Gateway VPC link when existing_vpc_id is set."
  type        = list(string)
  default     = []
}

variable "dqlite_advertise_addresses" {
  description = "Optional durable dqlite member addresses keyed by voter index. Set only when preserving a pre-existing quorum that advertised different addresses; live transport is translated to the module's Cloud Map records."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for index, address in var.dqlite_advertise_addresses : contains(["0", "1", "2"], index) && can(regex("^[^[:space:]]+:[0-9]+$", address))])
    error_message = "dqlite_advertise_addresses keys must be 0, 1, or 2 and values must be host:port coordinates."
  }
}

variable "existing_public_subnet_ids" {
  description = "Public subnet IDs for the public SSH Network Load Balancer when existing_vpc_id is set."
  type        = list(string)
  default     = []
}

variable "existing_ecs_cluster_arn" {
  description = "Existing Amazon Elastic Container Service cluster ARN. Set it with existing_vpc_id to share an environment cluster instead of creating one."
  type        = string
  default     = ""
}

variable "create_api_gateway_vpc_link" {
  description = "Whether Bleephub creates its own Amazon API Gateway VPC Link and security group. Set false when supplying environment-owned coordinates."
  type        = bool
  default     = true
}

variable "api_gateway_vpc_link_id" {
  description = "Existing Amazon API Gateway VPC Link ID shared by this deployment when create_api_gateway_vpc_link is false."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = (
      (var.create_api_gateway_vpc_link && var.api_gateway_vpc_link_id == null && var.api_gateway_vpc_link_security_group_id == null) ||
      (!var.create_api_gateway_vpc_link && var.api_gateway_vpc_link_id != null && trimspace(var.api_gateway_vpc_link_id) != "" && var.api_gateway_vpc_link_security_group_id != null && trimspace(var.api_gateway_vpc_link_security_group_id) != "")
    )
    error_message = "Leave both shared VPC Link coordinates null when create_api_gateway_vpc_link is true, or set both to non-empty values when it is false."
  }
}

variable "api_gateway_vpc_link_security_group_id" {
  description = "Security-group ID attached to api_gateway_vpc_link_id when create_api_gateway_vpc_link is false."
  type        = string
  default     = null
  nullable    = true
}

variable "availability_zones" {
  description = "At least two Availability Zones in region used for Bleephub public, private, and EFS subnets."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b"]

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "Bleephub requires at least two Availability Zones for Amazon ECS VPC-link availability and Amazon Elastic File System."
  }
}

variable "container_image" {
  description = "Immutable Bleephub release image URI, including its registry and digest or immutable tag."
  type        = string
}

variable "hosted_zone_id" {
  description = "Route 53 public hosted-zone ID delegated for the Bleephub hostname."
  type        = string
}

variable "domain_name" {
  description = "Canonical Bleephub DNS name within hosted_zone_id, for example bleephub.e6qu.dev."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]+[a-z0-9]$", var.domain_name))
    error_message = "domain_name must be a lowercase DNS name."
  }
}

variable "admin_token" {
  description = "Initial Bleephub administrator token. Terraform stores it only in AWS Secrets Manager and never exposes it as an output."
  type        = string
  sensitive   = true
}

variable "wake_listener_zip_path" {
  description = "Path to the pre-built Linux Amazon Lambda bootstrap ZIP for the Bleephub wake listener. Build it with scripts/build-bleephub-wake.sh before apply."
  type        = string
}

variable "startup_page_path" {
  description = "Path to the extracted startup index.html from the versioned Bleephub startup bundle. Build it with scripts/build-bleephub-startup.sh before apply."
  type        = string
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth App client ID used for Bleephub browser sign-in."
  type        = string
  default     = ""
}

variable "github_oauth_client_secret_arn" {
  description = "Existing AWS Secrets Manager ARN holding the rotated GitHub OAuth App client secret. Terraform never reads or stores its value."
  type        = string
  default     = ""
}

variable "shauth_oidc_issuer" {
  description = "Shauth HTTPS OpenID Connect issuer used for Bleephub browser sign-in. Leave empty only when every Shauth coordinate is empty."
  type        = string
  default     = ""
}

variable "shauth_oidc_client_id" {
  description = "Shauth confidential OpenID Connect client ID for Bleephub browser sign-in."
  type        = string
  default     = ""
}

variable "shauth_oidc_client_secret_arn" {
  description = "Existing AWS Secrets Manager ARN holding Bleephub's Shauth confidential-client secret. Terraform never reads or stores its value."
  type        = string
  default     = ""
}

variable "shauth_oidc_post_logout_url" {
  description = "Exact Bleephub-origin HTTPS logout-completion bridge URI registered for Bleephub in Shauth. It must be https://<domain_name>/auth/shauth/logout/complete. Leave empty only when every Shauth coordinate is empty."
  type        = string
  default     = ""
}

variable "idle_shutdown_minutes" {
  description = "Number of inactive minutes before the ECS service is scaled to zero."
  type        = number
  default     = 5

  validation {
    condition     = var.idle_shutdown_minutes >= 5
    error_message = "idle_shutdown_minutes must be at least five minutes."
  }
}

variable "idle_shutdown_enabled" {
  description = "Whether the wake controller stops Bleephub after idle_shutdown_minutes without Amazon API Gateway requests. Disable for always-on environments."
  type        = bool
  default     = true
}

variable "task_cpu" {
  description = "Fargate CPU units for the single Bleephub task."
  type        = number
  default     = 1024
}

variable "task_memory" {
  description = "Fargate memory MiB for the single Bleephub task."
  type        = number
  default     = 2048
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention for Bleephub application logs."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Additional tags applied to every supported resource."
  type        = map(string)
  default     = {}
}
