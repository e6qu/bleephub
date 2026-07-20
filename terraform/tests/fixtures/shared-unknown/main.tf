terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

resource "aws_security_group" "shared_api_gateway_vpc_link" {
  name   = "shared-api-gateway-vpc-link"
  vpc_id = "vpc-0123456789abcdef0"
}

resource "aws_apigatewayv2_vpc_link" "shared" {
  name               = "shared"
  security_group_ids = [aws_security_group.shared_api_gateway_vpc_link.id]
  subnet_ids         = ["subnet-00000000000000001", "subnet-00000000000000002"]
}

module "bleephub" {
  source = "../../.."

  name                                   = "bleephub-test"
  existing_vpc_id                        = "vpc-0123456789abcdef0"
  existing_private_subnet_ids            = ["subnet-00000000000000001", "subnet-00000000000000002"]
  existing_public_subnet_ids             = ["subnet-00000000000000003", "subnet-00000000000000004"]
  existing_ecs_cluster_arn               = "arn:aws:ecs:eu-west-1:123456789012:cluster/dev"
  create_api_gateway_vpc_link            = false
  api_gateway_vpc_link_id                = aws_apigatewayv2_vpc_link.shared.id
  api_gateway_vpc_link_security_group_id = aws_security_group.shared_api_gateway_vpc_link.id
  container_image                        = "example.invalid/bleephub:test"
  hosted_zone_id                         = "Z0123456789ABCDEFGH"
  domain_name                            = "bleephub.test.example"
  admin_token                            = "terraform-test-admin-token"
  wake_listener_zip_path                 = "${path.module}/../../../startup/index.html"
  startup_page_path                      = "${path.module}/../../../startup/index.html"
}

output "effective_vpc_link_id" {
  value = module.bleephub.api_gateway_vpc_link_id
}

output "effective_vpc_link_security_group_id" {
  value = module.bleephub.api_gateway_vpc_link_security_group_id
}
