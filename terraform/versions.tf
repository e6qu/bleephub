terraform {
  # CI provisions exactly this Terraform minor. A wider constraint would admit
  # releases nothing in this repository ever runs.
  required_version = "~> 1.15.8"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.63.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.9.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.4.0"
    }
  }

  # Partial configuration: bucket, key, region, and KMS key come from
  # -backend-config so no environment's coordinates are baked into the module.
  # `use_lockfile` is S3-native state locking, which supersedes the DynamoDB
  # table this Terraform line deprecates.
  backend "s3" {
    encrypt      = true
    use_lockfile = true
  }
}
