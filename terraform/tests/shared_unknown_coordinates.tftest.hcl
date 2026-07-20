mock_provider "aws" {
  mock_resource "aws_acm_certificate" {
    defaults = {
      arn = "arn:aws:acm:eu-west-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
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
  target          = module.bleephub.aws_acm_certificate.this
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

run "resource_derived_shared_coordinates_are_plan_safe" {
  command = plan

  module {
    source = "./tests/fixtures/shared-unknown"
  }
}
