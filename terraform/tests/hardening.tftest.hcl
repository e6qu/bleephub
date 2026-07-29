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

override_resource {
  target          = aws_cloudwatch_log_group.this
  override_during = plan
  values = {
    arn = "arn:aws:logs:eu-west-1:123456789012:log-group:/ecs/bleephub-test"
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

  override_resource {
    target          = aws_secretsmanager_secret.dqlite_secret
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/dqlite-secret"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/dqlite-secret"
    }
  }

  override_resource {
    target          = aws_secretsmanager_secret.persistence_encryption_key
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/persistence-encryption-key"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/persistence-encryption-key"
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

# Both ends of the dqlite transport refuse to start without this secret, and a
# mismatch is only visible at boot, so every leg is asserted separately.
run "the_dqlite_cluster_secret_reaches_both_ends" {
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

  override_resource {
    target          = aws_secretsmanager_secret.dqlite_secret
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/dqlite-secret"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/dqlite-secret"
    }
  }

  override_resource {
    target          = aws_secretsmanager_secret.persistence_encryption_key
    override_during = plan
    values = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/persistence-encryption-key"
      id  = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:bleephub-test/persistence-encryption-key"
    }
  }

  assert {
    condition     = aws_secretsmanager_secret.dqlite_secret.name == "bleephub-test/dqlite-secret" && aws_secretsmanager_secret.dqlite_secret.kms_key_id == aws_kms_key.this.arn
    error_message = "the dqlite cluster secret must live in Secrets Manager under the module's own key"
  }

  assert {
    condition     = aws_secretsmanager_secret_version.dqlite_secret.secret_id == aws_secretsmanager_secret.dqlite_secret.id
    error_message = "the generated dqlite cluster secret must be stored as a version of its own secret"
  }

  # A plaintext environment entry would leak the cluster credential into every
  # DescribeTaskDefinition call.
  assert {
    condition = alltrue(concat(
      [for entry in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].environment : entry.name != "BLEEPHUB_DQLITE_SECRET"],
      flatten([for definition in aws_ecs_task_definition.dqlite : [for entry in jsondecode(definition.container_definitions)[0].environment : entry.name != "BLEEPHUB_DQLITE_SECRET"]]),
    ))
    error_message = "the dqlite cluster secret must never appear as a plaintext task environment entry"
  }

  assert {
    condition     = one([for entry in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].secrets : entry.valueFrom if entry.name == "BLEEPHUB_DQLITE_SECRET"]) == aws_secretsmanager_secret.dqlite_secret.arn
    error_message = "the application task must receive the dqlite cluster secret"
  }

  assert {
    condition = alltrue([
      for definition in aws_ecs_task_definition.dqlite :
      one([for entry in jsondecode(definition.container_definitions)[0].secrets : entry.valueFrom if entry.name == "BLEEPHUB_DQLITE_SECRET"]) == aws_secretsmanager_secret.dqlite_secret.arn
    ])
    error_message = "every dqlite voter task must receive the same dqlite cluster secret"
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_role_policy.execution_secret.policy).Statement :
      contains(statement.Resource, aws_secretsmanager_secret.dqlite_secret.arn)
      if contains(statement.Action, "secretsmanager:GetSecretValue")
    ])
    error_message = "the execution role must be able to read the dqlite cluster secret"
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_role_policy.execution_secret.policy).Statement :
      statement.Resource == aws_kms_key.this.arn
      if contains(statement.Action, "kms:Decrypt")
    ])
    error_message = "the execution role must be able to decrypt the secret under the module's key"
  }

  assert {
    condition     = aws_secretsmanager_secret.persistence_encryption_key.name == "bleephub-test/persistence-encryption-key" && aws_secretsmanager_secret.persistence_encryption_key.kms_key_id == aws_kms_key.this.arn
    error_message = "the persistence encryption key must live in Secrets Manager under the module's own KMS key"
  }

  assert {
    condition     = aws_secretsmanager_secret_version.persistence_encryption_key.secret_id == aws_secretsmanager_secret.persistence_encryption_key.id
    error_message = "the generated persistence encryption key must be stored as a version of its own secret"
  }

  assert {
    condition     = one([for entry in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].secrets : entry.valueFrom if entry.name == "BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY"]) == aws_secretsmanager_secret.persistence_encryption_key.arn
    error_message = "the application task must receive the persistence encryption key as an ECS secret"
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_role_policy.execution_secret.policy).Statement :
      contains(statement.Resource, aws_secretsmanager_secret.persistence_encryption_key.arn)
      if contains(statement.Action, "secretsmanager:GetSecretValue")
    ])
    error_message = "the execution role must be able to read the persistence encryption key"
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
    condition     = aws_s3_bucket_versioning.startup.versioning_configuration[0].status == "Enabled"
    error_message = "the startup document must be versioned so a bad rollout is recoverable"
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
      one(aws_s3_bucket_server_side_encryption_configuration.startup.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "aws:kms",
    ])
    error_message = "every bucket must be encrypted with a KMS key, not the S3-managed one"
  }

  assert {
    condition = alltrue([
      one(aws_s3_bucket_server_side_encryption_configuration.git.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == aws_kms_key.this.arn,
      one(aws_s3_bucket_server_side_encryption_configuration.objects.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == aws_kms_key.this.arn,
      one(aws_s3_bucket_server_side_encryption_configuration.startup.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == aws_kms_key.this.arn,
      aws_efs_file_system.sqlite.kms_key_id == aws_kms_key.this.arn,
      aws_cloudwatch_log_group.this.kms_key_id == aws_kms_key.this.arn,
      aws_secretsmanager_secret.admin_token.kms_key_id == aws_kms_key.this.arn,
      aws_secretsmanager_secret.ssh_host_key.kms_key_id == aws_kms_key.this.arn,
      aws_secretsmanager_secret.persistence_encryption_key.kms_key_id == aws_kms_key.this.arn,
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

run "the_public_edge_and_wake_functions_emit_audit_telemetry" {
  command = plan

  assert {
    condition     = one(aws_apigatewayv2_stage.default.access_log_settings).destination_arn == aws_cloudwatch_log_group.this.arn
    error_message = "the public HTTP API must write structured access logs"
  }

  assert {
    condition = alltrue([
      one(aws_lambda_function.wake.tracing_config).mode == "Active",
      one(aws_lambda_function.idle_shutdown.tracing_config).mode == "Active",
      one(aws_lambda_function.idle_arm.tracing_config).mode == "Active",
    ])
    error_message = "every wake-controller Lambda must emit active X-Ray traces"
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

  expect_failures = [aws_ecs_task_definition.this]
}

run "a_region_disagreeing_with_the_provider_is_rejected" {
  command = plan

  variables {
    region             = "us-east-1"
    availability_zones = ["us-east-1a", "us-east-1b"]
  }

  expect_failures = [aws_ecs_task_definition.this]
}
