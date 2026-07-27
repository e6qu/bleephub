mock_provider "aws" {
  mock_resource "aws_acm_certificate" {
    defaults = {
      arn = "arn:aws:acm:eu-west-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
      domain_validation_options = [{
        domain_name           = "bleephub.test.example"
        resource_record_name  = "_validation.bleephub.test.example"
        resource_record_type  = "CNAME"
        resource_record_value = "_validation.acm-validations.aws"
      }]
    }
  }

  mock_resource "aws_acm_certificate_validation" {
    defaults = {
      certificate_arn = "arn:aws:acm:eu-west-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:root"
    }
  }

  mock_data "aws_ecs_cluster" {
    defaults = {
      cluster_name = "dev"
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "eu-west-1"
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}

mock_provider "tls" {}

override_resource {
  target          = aws_acm_certificate.this
  override_during = plan
  values = {
    arn = "arn:aws:acm:eu-west-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
    domain_validation_options = [{
      domain_name           = "bleephub.test.example"
      resource_record_name  = "_validation.bleephub.test.example"
      resource_record_type  = "CNAME"
      resource_record_value = "_validation.acm-validations.aws"
    }]
  }
}

# A plan leaves computed identifiers unknown, and an assertion cannot compare
# unknowns. These pin the two identifiers the wiring assertions read back.
override_resource {
  target          = aws_kms_key.this
  override_during = plan
  values = {
    arn    = "arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000000"
    key_id = "00000000-0000-0000-0000-000000000000"
  }
}

override_resource {
  target          = aws_cloudfront_distribution.startup
  override_during = plan
  values = {
    arn         = "arn:aws:cloudfront::123456789012:distribution/E000000000000"
    domain_name = "d000000000000.cloudfront.net"
  }
}

variables {
  name                        = "bleephub-test"
  existing_vpc_id             = "vpc-0123456789abcdef0"
  existing_private_subnet_ids = ["subnet-00000000000000001", "subnet-00000000000000002"]
  existing_public_subnet_ids  = ["subnet-00000000000000003", "subnet-00000000000000004"]
  existing_ecs_cluster_arn    = "arn:aws:ecs:eu-west-1:123456789012:cluster/dev"
  container_image             = "example.invalid/bleephub:test"
  ssh_ingress_cidr_blocks     = ["203.0.113.0/24"]
  hosted_zone_id              = "Z0123456789ABCDEFGH"
  domain_name                 = "bleephub.test.example"
  admin_token                 = "terraform-test-admin-token"
  wake_listener_zip_path      = "startup/index.html"
  startup_page_path           = "startup/index.html"
}

run "a_deployment_replaces_the_task_without_dropping_service" {
  command = plan

  assert {
    condition     = aws_ecs_service.this.deployment_minimum_healthy_percent == 100
    error_message = "the application service must keep a healthy task for the whole deployment"
  }

  assert {
    condition     = aws_ecs_service.this.deployment_maximum_percent == 200
    error_message = "the application service needs replacement headroom to deploy without an outage"
  }

  assert {
    condition     = aws_ecs_service.this.deployment_circuit_breaker[0].enable && aws_ecs_service.this.deployment_circuit_breaker[0].rollback
    error_message = "the application service must abort and roll back a failing deployment"
  }

  assert {
    condition     = alltrue([for service in aws_ecs_service.dqlite : service.deployment_circuit_breaker[0].enable && service.deployment_circuit_breaker[0].rollback])
    error_message = "every dqlite voter service must abort and roll back a failing deployment"
  }

  assert {
    condition     = aws_ecs_service.ssh_gateway.deployment_circuit_breaker[0].enable && aws_ecs_service.ssh_gateway.deployment_circuit_breaker[0].rollback
    error_message = "the SSH gateway service must abort and roll back a failing deployment"
  }
}

run "the_application_container_reports_its_own_health" {
  command = plan

  override_resource {
    target          = aws_secretsmanager_secret.admin_token
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/admin-token"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/admin-token"
    }
  }

  override_resource {
    target          = aws_secretsmanager_secret.ssh_host_key
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/ssh-host-key"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/ssh-host-key"
    }
  }

  assert {
    condition     = can(jsondecode(aws_ecs_task_definition.this.container_definitions)[0].healthCheck.command)
    error_message = "the application container must declare a health check"
  }

  assert {
    condition     = anytrue([for word in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].healthCheck.command : strcontains(word, "/health")])
    error_message = "the container health check must probe the application health endpoint"
  }

  assert {
    condition     = jsondecode(aws_ecs_task_definition.this.container_definitions)[0].healthCheck.retries >= 2
    error_message = "the container health check must tolerate a single lost probe"
  }
}

run "both_durable_stores_have_a_restore_path" {
  command = plan

  assert {
    condition     = aws_s3_bucket_versioning.git.versioning_configuration[0].status == "Enabled"
    error_message = "the Git bucket holds the only copy of every ref and pack and must be versioned"
  }

  assert {
    condition     = aws_s3_bucket_versioning.objects.versioning_configuration[0].status == "Enabled"
    error_message = "the object bucket must be versioned"
  }

  assert {
    condition     = aws_efs_backup_policy.sqlite.backup_policy[0].status == "ENABLED"
    error_message = "the EFS filesystem must have AWS Backup enabled"
  }
}

run "durable_data_is_encrypted_with_a_rotating_customer_managed_key" {
  command = plan

  assert {
    condition     = aws_kms_key.this.enable_key_rotation && aws_kms_key.this.rotation_period_in_days == 365
    error_message = "the customer-managed key must rotate on a stated schedule"
  }

  assert {
    condition = alltrue([
      one(aws_s3_bucket_server_side_encryption_configuration.git.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "aws:kms",
      one(aws_s3_bucket_server_side_encryption_configuration.objects.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "aws:kms",
    ])
    error_message = "the Git and object buckets must be encrypted with a KMS key, not the S3-managed one"
  }

  assert {
    condition = alltrue([
      one(aws_s3_bucket_server_side_encryption_configuration.git.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == aws_kms_key.this.arn,
      one(aws_s3_bucket_server_side_encryption_configuration.objects.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == aws_kms_key.this.arn,
      aws_efs_file_system.sqlite.kms_key_id == aws_kms_key.this.arn,
      aws_cloudwatch_log_group.this.kms_key_id == aws_kms_key.this.arn,
      aws_secretsmanager_secret.admin_token.kms_key_id == aws_kms_key.this.arn,
      aws_secretsmanager_secret.ssh_host_key.kms_key_id == aws_kms_key.this.arn,
    ])
    error_message = "every durable store, secret, and log group must use the module's own key"
  }
}

run "the_startup_bucket_is_never_public" {
  command = plan

  assert {
    condition = alltrue([
      aws_s3_bucket_public_access_block.startup.block_public_acls,
      aws_s3_bucket_public_access_block.startup.ignore_public_acls,
      aws_s3_bucket_public_access_block.startup.block_public_policy,
      aws_s3_bucket_public_access_block.startup.restrict_public_buckets,
    ])
    error_message = "the startup bucket must keep every public-access block switched on"
  }

  assert {
    condition = alltrue([
      aws_s3_bucket_public_access_block.git.block_public_policy,
      aws_s3_bucket_public_access_block.git.restrict_public_buckets,
      aws_s3_bucket_public_access_block.objects.block_public_policy,
      aws_s3_bucket_public_access_block.objects.restrict_public_buckets,
    ])
    error_message = "the durable buckets must keep every public-access block switched on"
  }

  assert {
    condition     = aws_cloudfront_origin_access_control.startup.signing_behavior == "always"
    error_message = "the startup origin must be read through a signed origin access control"
  }

  assert {
    condition     = aws_apigatewayv2_integration.startup_page.integration_uri == "https://d000000000000.cloudfront.net/startup/index.html"
    error_message = "the startup route must be served through the distribution, not the bucket"
  }
}

run "the_named_ssh_audience_reaches_the_load_balancer" {
  command = plan

  variables {
    ssh_ingress_cidr_blocks = ["203.0.113.0/24", "198.51.100.7/32"]
  }

  assert {
    condition     = toset(one(aws_security_group.ssh.ingress).cidr_blocks) == toset(["203.0.113.0/24", "198.51.100.7/32"])
    error_message = "public SSH must be restricted to the supplied CIDR blocks"
  }
}

run "internet_wide_ssh_is_rejected" {
  command = plan

  variables {
    ssh_ingress_cidr_blocks = ["0.0.0.0/0"]
  }

  expect_failures = [var.ssh_ingress_cidr_blocks]
}

run "a_near_internet_wide_ssh_range_is_rejected" {
  command = plan

  variables {
    ssh_ingress_cidr_blocks = ["10.0.0.0/4"]
  }

  expect_failures = [var.ssh_ingress_cidr_blocks]
}

run "an_empty_ssh_audience_is_rejected" {
  command = plan

  variables {
    ssh_ingress_cidr_blocks = []
  }

  expect_failures = [var.ssh_ingress_cidr_blocks]
}

run "a_malformed_ssh_cidr_is_rejected" {
  command = plan

  variables {
    ssh_ingress_cidr_blocks = ["203.0.113.1"]
  }

  expect_failures = [var.ssh_ingress_cidr_blocks]
}

run "availability_zones_outside_the_region_are_rejected" {
  command = plan

  variables {
    availability_zones = ["us-east-1a", "us-east-1b"]
  }

  expect_failures = [var.availability_zones]
}

run "a_region_disagreeing_with_the_provider_is_rejected" {
  command = plan

  variables {
    region             = "us-east-1"
    availability_zones = ["us-east-1a", "us-east-1b"]
  }

  expect_failures = [aws_ecs_task_definition.this]
}
