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

variables {
  name                        = "bleephub-test"
  existing_vpc_id             = "vpc-0123456789abcdef0"
  existing_private_subnet_ids = ["subnet-00000000000000001", "subnet-00000000000000002"]
  existing_public_subnet_ids  = ["subnet-00000000000000003", "subnet-00000000000000004"]
  existing_ecs_cluster_arn    = "arn:aws:ecs:eu-west-1:123456789012:cluster/dev"
  container_image             = "example.invalid/bleephub:test"
  hosted_zone_id              = "Z0123456789ABCDEFGH"
  domain_name                 = "bleephub.test.example"
  admin_token                 = "terraform-test-admin-token"
  wake_listener_zip_path      = "startup/index.html"
  startup_page_path           = "startup/index.html"
}

run "dedicated_vpc_link_is_the_default" {
  command = plan

  override_resource {
    target          = aws_apigatewayv2_vpc_link.this[0]
    override_during = plan
    values = {
      id = "dedicated-link"
    }
  }

  override_resource {
    target          = aws_security_group.api_link[0]
    override_during = plan
    values = {
      id = "sg-dedicated"
    }
  }

  assert {
    condition     = length(aws_apigatewayv2_vpc_link.this) == 1
    error_message = "default mode must create one dedicated Amazon API Gateway VPC Link"
  }

  assert {
    condition     = length(aws_security_group.api_link) == 1
    error_message = "default mode must create one dedicated VPC Link security group"
  }

  assert {
    condition     = aws_apigatewayv2_integration.service.connection_id == aws_apigatewayv2_vpc_link.this[0].id
    error_message = "the Bleephub API integration must use the dedicated VPC Link"
  }

  assert {
    condition     = one([for rule in aws_security_group.task.ingress : rule.security_groups if rule.from_port == 5555]) == toset(["sg-dedicated"])
    error_message = "the Bleephub task must authorize the dedicated VPC Link security group"
  }
}

run "shared_vpc_link_creates_no_dedicated_resources" {
  command = plan

  variables {
    create_api_gateway_vpc_link            = false
    api_gateway_vpc_link_id                = "ynul34"
    api_gateway_vpc_link_security_group_id = "sg-0123456789abcdef0"
  }

  assert {
    condition     = length(aws_apigatewayv2_vpc_link.this) == 0
    error_message = "shared-link mode must not create a dedicated Amazon API Gateway VPC Link"
  }

  assert {
    condition     = length(aws_security_group.api_link) == 0
    error_message = "shared-link mode must not create a dedicated VPC Link security group"
  }

  assert {
    condition     = aws_apigatewayv2_integration.service.connection_id == "ynul34"
    error_message = "the Bleephub API integration must use the supplied shared VPC Link"
  }

  assert {
    condition     = one([for rule in aws_security_group.task.ingress : rule.security_groups if rule.from_port == 5555]) == toset(["sg-0123456789abcdef0"])
    error_message = "the Bleephub task must authorize the supplied shared VPC Link security group"
  }

  assert {
    condition     = output.api_gateway_vpc_link_id == "ynul34" && output.api_gateway_vpc_link_security_group_id == "sg-0123456789abcdef0"
    error_message = "the module must output the effective shared VPC Link coordinates"
  }
}

run "vpc_link_without_security_group_is_rejected" {
  command = plan

  variables {
    create_api_gateway_vpc_link = false
    api_gateway_vpc_link_id     = "ynul34"
  }

  expect_failures = [var.api_gateway_vpc_link_id]
}

run "security_group_without_vpc_link_is_rejected" {
  command = plan

  variables {
    create_api_gateway_vpc_link            = false
    api_gateway_vpc_link_security_group_id = "sg-0123456789abcdef0"
  }

  expect_failures = [var.api_gateway_vpc_link_id]
}

run "empty_shared_coordinates_are_rejected" {
  command = plan

  variables {
    create_api_gateway_vpc_link            = false
    api_gateway_vpc_link_id                = ""
    api_gateway_vpc_link_security_group_id = ""
  }

  expect_failures = [var.api_gateway_vpc_link_id]
}

run "dedicated_mode_rejects_shared_coordinates" {
  command = plan

  variables {
    api_gateway_vpc_link_id                = "ynul34"
    api_gateway_vpc_link_security_group_id = "sg-0123456789abcdef0"
  }

  expect_failures = [var.api_gateway_vpc_link_id]
}
