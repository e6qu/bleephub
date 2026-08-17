locals {
  common_tags = merge(var.tags, {
    component  = "bleephub"
    managed-by = "terraform"
    service    = var.name
  })
  git_bucket            = "${var.name}-git"
  object_bucket         = "${var.name}-objects"
  startup_bucket        = "${var.name}-startup"
  access_log_bucket     = "${var.name}-access-logs"
  cf_log_bucket         = "${var.name}-cf-logs"
  azs                   = { for index, az in var.availability_zones : tostring(index) => az }
  uses_existing_network = var.existing_vpc_id != ""
  dqlite_nodes          = { for index in range(3) : tostring(index) => 9000 + index }
  dqlite_data_paths = {
    "0" = "/dqlite/0"
    "1" = "/dqlite/1"
    "2" = "/dqlite/2"
  }
  dqlite_live_addresses = {
    for node in keys(local.dqlite_nodes) : node => "${aws_service_discovery_service.dqlite[node].name}.${aws_service_discovery_private_dns_namespace.this.name}:9000"
  }
  dqlite_advertise_addresses = merge(local.dqlite_live_addresses, var.dqlite_advertise_addresses)
  dqlite_address_map = join(",", [
    for node in sort(keys(local.dqlite_nodes)) : "${local.dqlite_advertise_addresses[node]}=${local.dqlite_live_addresses[node]}"
    if local.dqlite_advertise_addresses[node] != local.dqlite_live_addresses[node]
  ])
  vpc_id                           = local.uses_existing_network ? var.existing_vpc_id : aws_vpc.this[0].id
  private_subnet_ids               = local.uses_existing_network ? var.existing_private_subnet_ids : [for subnet in aws_subnet.private : subnet.id]
  public_subnet_ids                = local.uses_existing_network ? var.existing_public_subnet_ids : [for subnet in aws_subnet.public : subnet.id]
  private_subnet_map               = { for index, subnet_id in local.private_subnet_ids : tostring(index) => subnet_id }
  ecs_cluster_arn                  = local.uses_existing_network ? var.existing_ecs_cluster_arn : aws_ecs_cluster.this[0].arn
  ecs_cluster_name                 = local.uses_existing_network ? data.aws_ecs_cluster.existing[0].cluster_name : aws_ecs_cluster.this[0].name
  uses_shared_api_gateway_vpc_link = !var.create_api_gateway_vpc_link
  # Invalid shared coordinates still have to survive provider schema decoding
  # long enough for the task-definition precondition below to report the
  # module's useful paired-input error.
  api_gateway_vpc_link_id                = local.uses_shared_api_gateway_vpc_link ? coalesce(var.api_gateway_vpc_link_id, "invalid-vpc-link") : aws_apigatewayv2_vpc_link.this[0].id
  api_gateway_vpc_link_security_group_id = local.uses_shared_api_gateway_vpc_link ? coalesce(var.api_gateway_vpc_link_security_group_id, "sg-00000000000000000") : aws_security_group.api_link[0].id

  # The release image carries no HTTP client, so the probe speaks HTTP over a
  # bash TCP redirection. Without it Amazon ECS calls a task healthy the moment
  # its process starts and a rolling deployment cannot tell a broken release
  # from a working one. The start period covers a cold wake, where the listener
  # only opens once the dqlite quorum has formed.
  app_health_check = {
    command     = ["CMD", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/5555 && printf 'GET /ready HTTP/1.0\\r\\n\\r\\n' >&3 && grep -q '^HTTP/1.[01] 200' <&3"]
    interval    = 15
    timeout     = 5
    retries     = 5
    startPeriod = 180
  }
}

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

# Every durable store and every secret is encrypted with this one customer-managed
# key: annual rotation and a key policy give the operator revocation and audit
# control that the AWS-managed keys do not offer, and the deletion window plus the
# guard below keep a mistaken destroy from making Git history unreadable.
resource "aws_kms_key" "this" {
  description             = "Bleephub durable storage, secrets, and logs"
  enable_key_rotation     = true
  rotation_period_in_days = 365
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.kms.json
  tags                    = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "this" {
  name          = "alias/${var.name}"
  target_key_id = aws_kms_key.this.key_id
}

data "aws_iam_policy_document" "kms" {
  statement {
    sid       = "AccountKeyAdministration"
    effect    = "Allow"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"]
    }
  }

  # CloudWatch Logs encrypts through the service principal rather than the
  # task's own credentials, and only for this log group.
  statement {
    sid       = "CloudWatchLogsEncryption"
    effect    = "Allow"
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncryptFrom", "kms:ReEncryptTo", "kms:GenerateDataKey", "kms:GenerateDataKeyWithoutPlaintext", "kms:DescribeKey"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logs.${var.region}.amazonaws.com"]
    }
    condition {
      test     = "ArnEquals"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:aws:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:/bleephub/${var.name}"]
    }
  }

  # S3 server-access-log delivery encrypts each log object with the module key.
  # Bind the grant to this account's own source buckets.
  statement {
    sid       = "S3AccessLogDelivery"
    effect    = "Allow"
    actions   = ["kms:GenerateDataKey"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  # CloudFront decrypts the KMS-encrypted startup document through its OAC.
  # Bind that ability to this distribution rather than granting the service
  # principal access to every object encrypted by the shared key.
  statement {
    sid       = "CloudFrontStartupDecryption"
    effect    = "Allow"
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["cloudfront.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "AWS:SourceArn"
      values   = [aws_cloudfront_distribution.startup.arn]
    }
  }

  # CloudWatch alarm actions publish to the KMS-encrypted alerts SNS topic
  # through the CloudWatch service principal, so the key must let CloudWatch
  # generate a data key and decrypt when it encrypts a notification. Bound to
  # this account.
  statement {
    sid       = "CloudWatchAlarmsToEncryptedSNS"
    effect    = "Allow"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["cloudwatch.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

moved {
  from = aws_security_group.api_link
  to   = aws_security_group.api_link[0]
}

moved {
  from = aws_apigatewayv2_vpc_link.this
  to   = aws_apigatewayv2_vpc_link.this[0]
}

moved {
  from = aws_s3_bucket_policy.startup_public_read
  to   = aws_s3_bucket_policy.startup_origin_read
}

data "aws_ecs_cluster" "existing" {
  count        = local.uses_existing_network ? 1 : 0
  cluster_name = element(reverse(split("/", var.existing_ecs_cluster_arn)), 0)
}

resource "aws_vpc" "this" {
  count                = local.uses_existing_network ? 0 : 1
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags                 = merge(local.common_tags, { Name = "${var.name}-vpc" })
}

resource "aws_internet_gateway" "this" {
  count  = local.uses_existing_network ? 0 : 1
  vpc_id = aws_vpc.this[0].id
  tags   = merge(local.common_tags, { Name = "${var.name}-igw" })
}

# No auto-assigned public IPs: the only instance launched here is the fck-nat
# NAT box, whose launch template attaches its own public IP
# (associate_public_ip_address), and the network load balancer brings its own
# AWS-managed addresses. Everything else runs in the private subnets.
resource "aws_subnet" "public" {
  for_each                = local.uses_existing_network ? {} : local.azs
  vpc_id                  = aws_vpc.this[0].id
  availability_zone       = each.value
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, tonumber(each.key))
  map_public_ip_on_launch = false
  tags                    = merge(local.common_tags, { Name = "${var.name}-public-${each.value}" })
}

resource "aws_subnet" "private" {
  for_each          = local.uses_existing_network ? {} : local.azs
  vpc_id            = aws_vpc.this[0].id
  availability_zone = each.value
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, 128 + tonumber(each.key))
  tags              = merge(local.common_tags, { Name = "${var.name}-private-${each.value}" })
}

resource "aws_route_table" "public" {
  count  = local.uses_existing_network ? 0 : 1
  vpc_id = aws_vpc.this[0].id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this[0].id
  }
  tags = merge(local.common_tags, { Name = "${var.name}-public" })
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public[0].id
}

resource "aws_route_table" "private" {
  for_each = local.uses_existing_network ? {} : local.azs
  vpc_id   = aws_vpc.this[0].id
  tags     = merge(local.common_tags, { Name = "${var.name}-private-${each.value}" })
}

resource "aws_route_table_association" "private" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private[each.key].id
}

# fck-nat is the actual upstream NAT-instance implementation. It owns the
# default routes of every private subnet; this module never provisions an AWS
# managed NAT Gateway.
#
# 24/7 NAT cost (CI-037): fck-nat is a single t4g.nano NAT *instance*
# (~$3/mo + data) deliberately chosen over a managed NAT Gateway (~$32/mo +
# per-GB), so this is already the low-cost egress option. It stays up around the
# clock on purpose: even while the API service is idle-shut-down (desired_count
# 0), background reconcilers and the wake path itself need outbound reachability
# (ECR pulls, ACME renewals, the wake controller's own AWS API calls), so gating
# NAT on idle state would deadlock the very path that scales the service back up.
# ha_mode is off (a second AZ instance doubles cost for a dev/simulator
# deployment); a NAT-instance replacement is ~1 min via the ASG the module owns.
module "fck_nat" {
  count   = local.uses_existing_network ? 0 : 1
  source  = "RaJiska/fck-nat/aws"
  version = "1.6.1"

  name                 = "${var.name}-fck-nat"
  vpc_id               = aws_vpc.this[0].id
  subnet_id            = aws_subnet.public["0"].id
  update_route_tables  = true
  route_tables_ids     = { for key, route_table in aws_route_table.private : key => route_table.id }
  ha_mode              = false
  use_cloudwatch_agent = true
  tags                 = local.common_tags
}

# Access policy for the S3 gateway endpoint. It grants this instance's own
# buckets full access and leaves AWS-owned buckets (ECR image layers, etc.)
# reachable read-only, so container image pulls and other AWS service traffic
# over the endpoint keep working while the endpoint is no longer wide open for
# writes to arbitrary buckets.
data "aws_iam_policy_document" "s3_endpoint" {
  count = local.uses_existing_network ? 0 : 1

  statement {
    sid    = "AppBucketsFullAccess"
    effect = "Allow"
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:ListBucket",
      "s3:GetBucketLocation",
    ]
    resources = [
      aws_s3_bucket.git.arn,
      "${aws_s3_bucket.git.arn}/*",
      aws_s3_bucket.objects.arn,
      "${aws_s3_bucket.objects.arn}/*",
    ]
  }

  statement {
    sid    = "AwsServiceBucketReads"
    effect = "Allow"
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions   = ["s3:GetObject"]
    resources = ["*"]
  }
}

resource "aws_vpc_endpoint" "s3" {
  count             = local.uses_existing_network ? 0 : 1
  vpc_id            = aws_vpc.this[0].id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [for route_table in aws_route_table.private : route_table.id]
  policy            = data.aws_iam_policy_document.s3_endpoint[0].json
  tags              = merge(local.common_tags, { Name = "${var.name}-s3" })
}

# Every Git ref and pack lives here; server-access logs go to the dedicated
# access-log bucket (see aws_s3_bucket_logging.git).
resource "aws_s3_bucket" "git" {
  bucket        = local.git_bucket
  force_destroy = var.force_destroy_storage
  tags          = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

# Every Git ref and pack lives here and nowhere else, so an overwrite or a
# delete needs a previous version to restore from.
resource "aws_s3_bucket_versioning" "git" {
  bucket = aws_s3_bucket.git.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "git" {
  bucket = aws_s3_bucket.git.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "git" {
  bucket                  = aws_s3_bucket.git.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "git" {
  bucket     = aws_s3_bucket.git.id
  policy     = data.aws_iam_policy_document.git_transport.json
  depends_on = [aws_s3_bucket_public_access_block.git]
}

data "aws_iam_policy_document" "git_transport" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.git.arn, "${aws_s3_bucket.git.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "git" {
  bucket = aws_s3_bucket.git.id
  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
    # Versioning is enabled on this bucket; expire superseded copies so
    # noncurrent versions do not accumulate storage cost forever (CI-037).
    noncurrent_version_expiration { noncurrent_days = 30 }
  }
}

# Object storage; server-access logs go to the dedicated access-log bucket
# (see aws_s3_bucket_logging.objects).
resource "aws_s3_bucket" "objects" {
  bucket        = local.object_bucket
  force_destroy = var.force_destroy_storage
  tags          = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "objects" {
  bucket = aws_s3_bucket.objects.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "objects" {
  bucket = aws_s3_bucket.objects.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "objects" {
  bucket                  = aws_s3_bucket.objects.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "objects" {
  bucket     = aws_s3_bucket.objects.id
  policy     = data.aws_iam_policy_document.objects_transport.json
  depends_on = [aws_s3_bucket_public_access_block.objects]
}

data "aws_iam_policy_document" "objects_transport" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.objects.arn, "${aws_s3_bucket.objects.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "objects" {
  bucket = aws_s3_bucket.objects.id
  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
    # Versioning is enabled on this bucket; expire superseded copies so
    # noncurrent versions do not accumulate storage cost forever (CI-037).
    noncurrent_version_expiration { noncurrent_days = 30 }
  }
}

# Server-access logs for every durable bucket land here. The bucket logs its
# own access into itself (the standard non-recursive self-logging pattern), so
# it needs no external target, and S3 log delivery writes under bucket-owner-
# enforced ownership through the policy below rather than a legacy ACL.
resource "aws_s3_bucket" "access_logs" {
  bucket        = local.access_log_bucket
  force_destroy = var.force_destroy_storage
  tags          = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this.arn
    }
    bucket_key_enabled = true
  }
}

# Versioned for tamper-evidence on the security access trail (an overwrite keeps
# the prior copy); noncurrent versions are expired quickly by the lifecycle rule
# below so the retained history stays cheap.
resource "aws_s3_bucket_versioning" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_public_access_block" "access_logs" {
  bucket                  = aws_s3_bucket.access_logs.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = true
  restrict_public_buckets = true
}

# Access logs are a re-derivable audit trail; expire current objects after a
# year and drop superseded versions quickly so versioning stays cheap.
resource "aws_s3_bucket_lifecycle_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  rule {
    id     = "expire-access-logs"
    status = "Enabled"
    filter {}
    expiration { days = 365 }
    noncurrent_version_expiration { noncurrent_days = 30 }
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
  }
}

data "aws_iam_policy_document" "access_logs" {
  statement {
    sid       = "S3ServerAccessLogsDelivery"
    effect    = "Allow"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.access_logs.arn}/*"]
    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values = [
        aws_s3_bucket.git.arn,
        aws_s3_bucket.objects.arn,
        aws_s3_bucket.startup.arn,
        aws_s3_bucket.access_logs.arn,
      ]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.access_logs.arn, "${aws_s3_bucket.access_logs.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "access_logs" {
  bucket     = aws_s3_bucket.access_logs.id
  policy     = data.aws_iam_policy_document.access_logs.json
  depends_on = [aws_s3_bucket_public_access_block.access_logs]
}

resource "aws_s3_bucket_logging" "git" {
  bucket        = aws_s3_bucket.git.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "git/"
}

resource "aws_s3_bucket_logging" "objects" {
  bucket        = aws_s3_bucket.objects.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "objects/"
}

resource "aws_s3_bucket_logging" "startup" {
  bucket        = aws_s3_bucket.startup.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "startup/"
}

resource "aws_s3_bucket_logging" "access_logs" {
  bucket        = aws_s3_bucket.access_logs.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "access-logs/"
}

# This contains exactly one non-sensitive document. It makes the scale-to-zero
# transition visible without starting an ECS task merely to render a loading
# screen; no application, Git, object, or status data is readable from it.
resource "aws_s3_bucket" "startup" {
  bucket        = local.startup_bucket
  force_destroy = var.force_destroy_storage
  tags          = local.common_tags
}

resource "aws_s3_bucket_versioning" "startup" {
  bucket = aws_s3_bucket.startup.id
  versioning_configuration { status = "Enabled" }
}

# Versioning is enabled above; without this the startup bucket's superseded
# object versions would accumulate forever with no expiration (CI-037).
resource "aws_s3_bucket_lifecycle_configuration" "startup" {
  bucket = aws_s3_bucket.startup.id
  rule {
    id     = "expire-noncurrent-and-abort-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
    noncurrent_version_expiration { noncurrent_days = 30 }
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "startup" {
  bucket = aws_s3_bucket.startup.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "startup" {
  bucket                  = aws_s3_bucket.startup.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = true
  restrict_public_buckets = true
}

# Amazon API Gateway cannot sign an S3 origin request, so the startup document
# reaches the browser through CloudFront: origin access control signs the fetch
# and the bucket itself stays entirely private.
resource "aws_cloudfront_origin_access_control" "startup" {
  name                              = "${var.name}-startup"
  description                       = "Signs Bleephub startup-document reads"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

data "aws_cloudfront_cache_policy" "caching_disabled" {
  name = "Managed-CachingDisabled"
}

# The origin is one fixed, non-sensitive startup document; API Gateway access
# logs cover every viewer request, while a WAF or a second CloudFront log bucket
# would add cost and attack surface without protecting a dynamic application.
#trivy:ignore:AWS-0010:exp:2027-01-28 trivy:ignore:AWS-0011:exp:2027-01-28
resource "aws_cloudfront_distribution" "startup" {
  enabled = true
  comment = "${var.name} startup document"

  origin {
    domain_name              = aws_s3_bucket.startup.bucket_regional_domain_name
    origin_id                = "startup"
    origin_access_control_id = aws_cloudfront_origin_access_control.startup.id
  }

  default_cache_behavior {
    target_origin_id       = "startup"
    viewer_protocol_policy = "https-only"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]
    # The document reports live wake progress and must never be served stale.
    cache_policy_id = data.aws_cloudfront_cache_policy.caching_disabled.id
  }

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  viewer_certificate {
    cloudfront_default_certificate = true
  }

  tags = local.common_tags
}

data "aws_iam_policy_document" "startup_origin_read" {
  statement {
    sid       = "ReadStartupDocumentFromDistribution"
    effect    = "Allow"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.startup.arn}/startup/index.html"]
    principals {
      type        = "Service"
      identifiers = ["cloudfront.amazonaws.com"]
    }
    condition {
      test     = "ArnEquals"
      variable = "AWS:SourceArn"
      values   = [aws_cloudfront_distribution.startup.arn]
    }
  }

  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.startup.arn, "${aws_s3_bucket.startup.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "startup_origin_read" {
  bucket     = aws_s3_bucket.startup.id
  policy     = data.aws_iam_policy_document.startup_origin_read.json
  depends_on = [aws_s3_bucket_public_access_block.startup]
}

resource "aws_s3_object" "startup_page" {
  bucket        = aws_s3_bucket.startup.id
  key           = "startup/index.html"
  source        = var.startup_page_path
  etag          = filemd5(var.startup_page_path)
  content_type  = "text/html; charset=utf-8"
  cache_control = "no-store, max-age=0"
  tags          = local.common_tags
  depends_on = [
    aws_s3_bucket_server_side_encryption_configuration.startup,
    aws_s3_bucket_versioning.startup,
  ]
}

resource "aws_secretsmanager_secret" "admin_token" {
  name                    = "${var.name}/admin-token"
  recovery_window_in_days = 7
  kms_key_id              = aws_kms_key.this.arn
  tags                    = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_secretsmanager_secret_version" "admin_token" {
  secret_id     = aws_secretsmanager_secret.admin_token.id
  secret_string = var.admin_token
}

resource "tls_private_key" "ssh_host" {
  algorithm = "ED25519"
}

resource "aws_secretsmanager_secret" "ssh_host_key" {
  name                    = "${var.name}/ssh-host-key"
  recovery_window_in_days = 7
  kms_key_id              = aws_kms_key.this.arn
  tags                    = local.common_tags

  # Replacing this rotates the host key every client has pinned.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_secretsmanager_secret_version" "ssh_host_key" {
  secret_id     = aws_secretsmanager_secret.ssh_host_key.id
  secret_string = tls_private_key.ssh_host.private_key_openssh
}

# The dqlite transport upgrade carries this in an HTTP header, so the alphabet
# stays alphanumeric. No human ever needs the value: it only has to be identical
# across the application task and all three voters.
resource "random_password" "dqlite_secret" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "dqlite_secret" {
  name                    = "${var.name}/dqlite-secret"
  recovery_window_in_days = 7
  kms_key_id              = aws_kms_key.this.arn
  tags                    = local.common_tags

  # Replacing this splits the quorum: the members that restart with the new
  # value cannot speak to the members still holding the old one.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_secretsmanager_secret_version" "dqlite_secret" {
  secret_id     = aws_secretsmanager_secret.dqlite_secret.id
  secret_string = random_password.dqlite_secret.result
}

# Application-level authenticated encryption keeps Actions, Codespaces, OAuth,
# App, and browser-session credentials opaque even in a raw dqlite backup.
# This key must remain stable for the lifetime of those rows.
resource "random_id" "persistence_encryption_key" {
  byte_length = 32
}

resource "aws_secretsmanager_secret" "persistence_encryption_key" {
  name                    = "${var.name}/persistence-encryption-key"
  recovery_window_in_days = 7
  kms_key_id              = aws_kms_key.this.arn
  tags                    = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_secretsmanager_secret_version" "persistence_encryption_key" {
  secret_id     = aws_secretsmanager_secret.persistence_encryption_key.id
  secret_string = random_id.persistence_encryption_key.b64_std
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/bleephub/${var.name}"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.this.arn
  tags              = local.common_tags
}

resource "aws_security_group" "api_link" {
  count       = local.uses_shared_api_gateway_vpc_link ? 0 : 1
  name_prefix = "${var.name}-api-link-"
  description = "API Gateway VPC link to Bleephub application tasks"
  vpc_id      = local.vpc_id
  tags        = local.common_tags
}

# The link only needs the application tasks on the single HTTP API port. A
# standalone rule (rather than an inline egress block referencing the task
# group) keeps the two security groups free of a definition cycle.
resource "aws_vpc_security_group_egress_rule" "api_link_to_tasks" {
  count                        = local.uses_shared_api_gateway_vpc_link ? 0 : 1
  security_group_id            = aws_security_group.api_link[0].id
  description                  = "Reach application tasks selected by service discovery"
  referenced_security_group_id = aws_security_group.task.id
  ip_protocol                  = "tcp"
  from_port                    = 5555
  to_port                      = 5555
}

resource "aws_security_group" "task" {
  name_prefix = "${var.name}-task-"
  description = "Bleephub application task ingress and outbound integrations"
  vpc_id      = local.vpc_id
  ingress {
    description     = "HTTP API traffic from the API Gateway VPC link"
    protocol        = "tcp"
    from_port       = 5555
    to_port         = 5555
    security_groups = [local.api_gateway_vpc_link_security_group_id]
  }
  ingress {
    description     = "Git SSH traffic from the SSH gateway"
    protocol        = "tcp"
    from_port       = 2222
    to_port         = 2222
    security_groups = [aws_security_group.ssh_gateway.id]
  }
  # Git remotes, webhooks, package registries, and OIDC providers are
  # user-configurable internet destinations and cannot be reduced to CIDRs.
  #trivy:ignore:AWS-0104:exp:2027-01-28
  egress {
    description = "User-configured GitHub-compatible outbound integrations"
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = local.common_tags
}

resource "aws_security_group" "ssh" {
  name_prefix = "${var.name}-ssh-"
  description = "Public SSH Network Load Balancer ingress"
  vpc_id      = local.vpc_id
  ingress {
    description = "Operator-configured public Git SSH ingress"
    protocol    = "tcp"
    from_port   = 22
    to_port     = 22
    cidr_blocks = var.ssh_ingress_cidr_blocks
  }
  # The Network Load Balancer preserves client IPs and its nodes require
  # return traffic to arbitrary clients.
  #trivy:ignore:AWS-0104:exp:2027-01-28
  egress {
    description = "Return traffic to Git SSH clients"
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = local.common_tags
}

resource "aws_security_group" "ssh_gateway" {
  name_prefix = "${var.name}-ssh-gateway-"
  description = "SSH gateway traffic between the NLB and application tasks"
  vpc_id      = local.vpc_id
  ingress {
    description     = "Git SSH traffic from the public NLB"
    protocol        = "tcp"
    from_port       = 2222
    to_port         = 2222
    security_groups = [aws_security_group.ssh.id]
  }
  # The gateway wakes API Gateway and resolves dynamic Cloud Map task targets.
  #trivy:ignore:AWS-0104:exp:2027-01-28
  egress {
    description = "Wake endpoint and dynamic application task targets"
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = local.common_tags
}

resource "aws_security_group" "efs" {
  name_prefix = "${var.name}-efs-"
  description = "Encrypted EFS access from application and dqlite tasks"
  vpc_id      = local.vpc_id
  ingress {
    description     = "NFS from application and dqlite tasks"
    protocol        = "tcp"
    from_port       = 2049
    to_port         = 2049
    security_groups = [aws_security_group.task.id, aws_security_group.dqlite.id]
  }
  tags = local.common_tags
}

resource "aws_security_group" "dqlite" {
  name_prefix = "${var.name}-dqlite-"
  description = "Private dqlite quorum traffic"
  vpc_id      = local.vpc_id
  ingress {
    description     = "dqlite client traffic from application tasks"
    protocol        = "tcp"
    from_port       = 9000
    to_port         = 9000
    security_groups = [aws_security_group.task.id]
  }
  ingress {
    description = "dqlite raft traffic between quorum members"
    protocol    = "tcp"
    from_port   = 9000
    to_port     = 9000
    self        = true
  }
  # Quorum nodes require EFS, CloudWatch, Secrets Manager, and peer discovery;
  # endpoint addresses vary by VPC and account.
  #trivy:ignore:AWS-0104:exp:2027-01-28
  egress {
    description = "AWS services, EFS, and dqlite quorum peers"
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = local.common_tags
}

resource "aws_efs_file_system" "sqlite" {
  encrypted  = true
  kms_key_id = aws_kms_key.this.arn
  tags       = merge(local.common_tags, { Name = "${var.name}-sqlite" })

  lifecycle {
    prevent_destroy = true
    # An EFS filesystem's KMS key is create-time-only on AWS: it cannot be
    # changed in place, so expressing a key change forces a replacement that
    # prevent_destroy correctly refuses — which wedged every already-deployed
    # plan when the customer-managed key above was introduced (CI-061). New
    # deployments still encrypt with the customer-managed key at creation;
    # an existing filesystem keeps the key it was created with and its plans
    # apply cleanly. Migrating an existing filesystem to the customer-managed
    # key is an operational procedure, not an in-place apply: AWS Backup
    # restore into a new filesystem encrypted with the target key, then
    # point this resource at it (terraform state rm + import).
    ignore_changes = [kms_key_id]
  }
}

# The dqlite raft logs and release assets exist only on this filesystem, so
# without AWS Backup a deleted or corrupted directory has no restore path.
resource "aws_efs_backup_policy" "sqlite" {
  file_system_id = aws_efs_file_system.sqlite.id
  backup_policy { status = "ENABLED" }
}

resource "aws_efs_access_point" "sqlite" {
  file_system_id = aws_efs_file_system.sqlite.id
  posix_user {
    uid = 0
    gid = 0
  }
  root_directory {
    path = "/bleephub"
    creation_info {
      owner_uid   = 0
      owner_gid   = 0
      permissions = "0700"
    }
  }
  tags = local.common_tags
}

resource "aws_efs_access_point" "dqlite" {
  for_each       = local.dqlite_nodes
  file_system_id = aws_efs_file_system.sqlite.id
  posix_user {
    uid = 0
    gid = 0
  }
  root_directory {
    path = local.dqlite_data_paths[each.key]
    creation_info {
      owner_uid   = 0
      owner_gid   = 0
      permissions = "0700"
    }
  }
  tags = merge(local.common_tags, { Name = "${var.name}-dqlite-${each.key}" })
}

resource "aws_efs_mount_target" "sqlite" {
  for_each        = local.private_subnet_map
  file_system_id  = aws_efs_file_system.sqlite.id
  subnet_id       = each.value
  security_groups = [aws_security_group.efs.id]
}

data "aws_iam_policy_document" "assume_ecs" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "assume_lambda" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "assume_scheduler" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name_prefix        = "${var.name}-execution-"
  assume_role_policy = data.aws_iam_policy_document.assume_ecs.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "execution_secret" {
  name = "read-admin-token"
  role = aws_iam_role.execution.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = compact([aws_secretsmanager_secret.admin_token.arn, aws_secretsmanager_secret.ssh_host_key.arn, aws_secretsmanager_secret.dqlite_secret.arn, aws_secretsmanager_secret.persistence_encryption_key.arn, var.github_oauth_client_secret_arn, var.shauth_oidc_client_secret_arn]) },
    { Effect = "Allow", Action = ["kms:Decrypt"], Resource = aws_kms_key.this.arn }
  ] })
}

resource "aws_iam_role" "task" {
  name_prefix        = "${var.name}-task-"
  assume_role_policy = data.aws_iam_policy_document.assume_ecs.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy" "task_storage" {
  name = "bleephub-durable-storage"
  role = aws_iam_role.task.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["s3:ListBucket"], Resource = [aws_s3_bucket.git.arn, aws_s3_bucket.objects.arn] },
    { Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], Resource = ["${aws_s3_bucket.git.arn}/*", "${aws_s3_bucket.objects.arn}/*"] },
    { Effect = "Allow", Action = ["kms:Decrypt", "kms:GenerateDataKey"], Resource = aws_kms_key.this.arn }
  ] })
}

resource "aws_iam_role" "ssh_gateway" {
  name_prefix        = "${var.name}-ssh-gateway-"
  assume_role_policy = data.aws_iam_policy_document.assume_ecs.json
  tags               = local.common_tags
}

resource "aws_iam_role" "wake" {
  name_prefix        = "${var.name}-wake-"
  assume_role_policy = data.aws_iam_policy_document.assume_lambda.json
  tags               = local.common_tags
}

resource "aws_iam_role" "idle_arm_scheduler" {
  name_prefix        = "${var.name}-idle-arm-"
  assume_role_policy = data.aws_iam_policy_document.assume_scheduler.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy" "idle_arm_scheduler" {
  name = "invoke-idle-arm"
  role = aws_iam_role.idle_arm_scheduler.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = aws_lambda_function.idle_arm.arn }]
  })
}

resource "aws_iam_role_policy_attachment" "wake_logs" {
  role       = aws_iam_role.wake.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy" "wake_service" {
  name = "wake-bleephub-service"
  role = aws_iam_role.wake.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["ecs:DescribeServices", "ecs:UpdateService"], Resource = concat([aws_ecs_service.this.id], [for service in aws_ecs_service.dqlite : service.id]) },
      # ListTasks/DescribeTasks/StopTask do not take a task-ARN resource the
      # policy can pin (the task IDs are dynamic and ListTasks has no
      # resource-level permission), so scope them to this deployment's cluster
      # with the ecs:cluster condition key instead of letting the wake Lambda
      # stop any ECS task in the account.
      { Effect = "Allow", Action = ["ecs:ListTasks", "ecs:DescribeTasks", "ecs:StopTask"], Resource = "*", Condition = { ArnEquals = { "ecs:cluster" = local.ecs_cluster_arn } } },
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = aws_secretsmanager_secret.admin_token.arn },
      { Effect = "Allow", Action = ["kms:Decrypt"], Resource = aws_kms_key.this.arn },
      { Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" },
      { Effect = "Allow", Action = ["apigateway:GET", "apigateway:PATCH"], Resource = "arn:aws:apigateway:${var.region}::/apis/${aws_apigatewayv2_api.this.id}/*" },
      { Effect = "Allow", Action = ["cloudwatch:SetAlarmState", "cloudwatch:DisableAlarmActions", "cloudwatch:EnableAlarmActions"], Resource = aws_cloudwatch_metric_alarm.idle_shutdown.arn },
      { Effect = "Allow", Action = ["cloudwatch:DescribeAlarms"], Resource = "*" },
      { Effect = "Allow", Action = ["scheduler:CreateSchedule", "scheduler:UpdateSchedule"], Resource = "arn:aws:scheduler:${var.region}:${data.aws_caller_identity.current.account_id}:schedule/default/${var.name}-idle-arm" },
      { Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = aws_lambda_function.idle_shutdown.arn },
      { Effect = "Allow", Action = ["iam:PassRole"], Resource = aws_iam_role.idle_arm_scheduler.arn, Condition = { StringEquals = { "iam:PassedToService" = "scheduler.amazonaws.com" } } }
    ]
  })
}

resource "aws_ecs_cluster" "this" {
  count = local.uses_existing_network ? 0 : 1
  name  = var.name

  # Enable CloudWatch Container Insights so task/container CPU, memory, and
  # network metrics are captured without instrumenting the workload.
  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = local.common_tags
}

resource "aws_acm_certificate" "this" {
  domain_name               = var.domain_name
  subject_alternative_names = ["admin.${var.domain_name}"]
  validation_method         = "DNS"
  tags                      = local.common_tags
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "certificate" {
  for_each = { for option in aws_acm_certificate.this.domain_validation_options : option.domain_name => option }
  zone_id  = var.hosted_zone_id
  name     = each.value.resource_record_name
  type     = each.value.resource_record_type
  records  = [each.value.resource_record_value]
  ttl      = 60
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for record in aws_route53_record.certificate : record.fqdn]
}

resource "aws_service_discovery_private_dns_namespace" "this" {
  name = "${var.name}.internal"
  vpc  = local.vpc_id
  tags = local.common_tags
}

# Amazon API Gateway resolves this SRV record through its VPC link. The
# application task registers and deregisters its actual ENI address directly,
# eliminating the private Network Load Balancer hop.
resource "aws_service_discovery_service" "app" {
  name = "app"
  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.this.id
    routing_policy = "MULTIVALUE"
    dns_records {
      ttl  = 10
      type = "SRV"
    }
  }
  tags = local.common_tags
}

# Every durable dqlite voter has a separate stable discovery name. It resolves
# directly to that voter's ECS task IP, allowing voters to form a quorum without
# a Network Load Balancer and without routing through an unhealthy-leader gate.
resource "aws_service_discovery_service" "dqlite" {
  for_each = local.dqlite_nodes
  name     = "dqlite-${each.key}"
  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.this.id
    routing_policy = "MULTIVALUE"
    dns_records {
      ttl  = 10
      type = "A"
    }
  }
  tags = merge(local.common_tags, { Name = "${var.name}-dqlite-${each.key}" })
}

# Public reachability is the purpose of the Git SSH endpoint; ingress CIDRs are
# independently operator-restricted on aws_security_group.ssh.
#trivy:ignore:AWS-0053:exp:2027-01-28
resource "aws_lb" "ssh" {
  name               = "${var.name}-ssh"
  internal           = false
  load_balancer_type = "network"
  security_groups    = [aws_security_group.ssh.id]
  subnets            = local.public_subnet_ids
  tags               = local.common_tags
}

resource "aws_lb_target_group" "ssh" {
  name        = "${var.name}-ssh"
  port        = 2222
  protocol    = "TCP"
  target_type = "ip"
  vpc_id      = local.vpc_id
  health_check {
    protocol = "TCP"
    matcher  = null
  }
  tags = local.common_tags
}

resource "aws_lb_listener" "ssh" {
  load_balancer_arn = aws_lb.ssh.arn
  port              = 22
  protocol          = "TCP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.ssh.arn
  }
}

resource "aws_apigatewayv2_vpc_link" "this" {
  count              = local.uses_shared_api_gateway_vpc_link ? 0 : 1
  name               = var.name
  security_group_ids = [aws_security_group.api_link[0].id]
  subnet_ids         = local.private_subnet_ids
  tags               = local.common_tags
}

resource "aws_apigatewayv2_api" "this" {
  name          = var.name
  protocol_type = "HTTP"
  tags          = local.common_tags
}

resource "aws_apigatewayv2_integration" "service" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "HTTP_PROXY"
  integration_uri        = aws_service_discovery_service.app.arn
  integration_method     = "ANY"
  connection_type        = "VPC_LINK"
  connection_id          = local.api_gateway_vpc_link_id
  payload_format_version = "1.0"
}

resource "aws_apigatewayv2_integration" "wake" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.wake.invoke_arn
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_integration" "startup_page" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "HTTP_PROXY"
  integration_uri        = "https://${aws_cloudfront_distribution.startup.domain_name}/startup/index.html"
  integration_method     = "GET"
  payload_format_version = "1.0"
}

resource "aws_apigatewayv2_route" "startup_page" {
  api_id    = aws_apigatewayv2_api.this.id
  route_key = "GET /__startup"
  target    = "integrations/${aws_apigatewayv2_integration.startup_page.id}"
}

resource "aws_apigatewayv2_route" "startup_status" {
  api_id    = aws_apigatewayv2_api.this.id
  route_key = "GET /__startup/status"
  target    = "integrations/${aws_apigatewayv2_integration.wake.id}"
}

resource "aws_apigatewayv2_route" "default" {
  api_id    = aws_apigatewayv2_api.this.id
  route_key = "$default"
  target    = "integrations/${aws_apigatewayv2_integration.wake.id}"
  lifecycle { ignore_changes = [target] }
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.this.id
  name        = "$default"
  auto_deploy = true
  default_route_settings {
    throttling_burst_limit = 100
    throttling_rate_limit  = 50
  }
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.this.arn
    format = jsonencode({
      requestId      = "$context.requestId"
      ip             = "$context.identity.sourceIp"
      requestTime    = "$context.requestTime"
      httpMethod     = "$context.httpMethod"
      routeKey       = "$context.routeKey"
      status         = "$context.status"
      protocol       = "$context.protocol"
      responseLength = "$context.responseLength"
    })
  }
  tags = local.common_tags
}

resource "aws_lambda_permission" "wake_api" {
  statement_id  = "allow-api-gateway-wake"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.wake.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.this.execution_arn}/*/*"
}

resource "aws_apigatewayv2_domain_name" "service" {
  domain_name = var.domain_name
  domain_name_configuration {
    certificate_arn = aws_acm_certificate_validation.this.certificate_arn
    endpoint_type   = "REGIONAL"
    security_policy = "TLS_1_2"
  }
}

resource "aws_apigatewayv2_domain_name" "admin" {
  domain_name = "admin.${var.domain_name}"
  domain_name_configuration {
    certificate_arn = aws_acm_certificate_validation.this.certificate_arn
    endpoint_type   = "REGIONAL"
    security_policy = "TLS_1_2"
  }
}

resource "aws_apigatewayv2_api_mapping" "service" {
  api_id      = aws_apigatewayv2_api.this.id
  domain_name = aws_apigatewayv2_domain_name.service.id
  stage       = aws_apigatewayv2_stage.default.id
}

resource "aws_apigatewayv2_api_mapping" "admin" {
  api_id      = aws_apigatewayv2_api.this.id
  domain_name = aws_apigatewayv2_domain_name.admin.id
  stage       = aws_apigatewayv2_stage.default.id
}

resource "aws_route53_record" "service" {
  zone_id = var.hosted_zone_id
  name    = var.domain_name
  type    = "A"
  alias {
    name                   = aws_apigatewayv2_domain_name.service.domain_name_configuration[0].target_domain_name
    zone_id                = aws_apigatewayv2_domain_name.service.domain_name_configuration[0].hosted_zone_id
    evaluate_target_health = false
  }
}

resource "aws_route53_record" "admin" {
  zone_id = var.hosted_zone_id
  name    = "admin.${var.domain_name}"
  type    = "A"
  alias {
    name                   = aws_apigatewayv2_domain_name.admin.domain_name_configuration[0].target_domain_name
    zone_id                = aws_apigatewayv2_domain_name.admin.domain_name_configuration[0].hosted_zone_id
    evaluate_target_health = false
  }
}

resource "aws_route53_record" "ssh" {
  zone_id = var.hosted_zone_id
  name    = "ssh.${var.domain_name}"
  type    = "A"
  alias {
    name                   = aws_lb.ssh.dns_name
    zone_id                = aws_lb.ssh.zone_id
    evaluate_target_health = true
  }
}

resource "aws_ecs_task_definition" "this" {
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.task_cpu)
  memory                   = tostring(var.task_memory)
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }
  execution_role_arn = aws_iam_role.execution.arn
  task_role_arn      = aws_iam_role.task.arn
  volume {
    name = "sqlite"
    efs_volume_configuration {
      file_system_id     = aws_efs_file_system.sqlite.id
      transit_encryption = "ENABLED"
      authorization_config {
        access_point_id = aws_efs_access_point.sqlite.id
        iam             = "DISABLED"
      }
    }
  }
  container_definitions = jsonencode([{ name = "bleephub", image = var.container_image, essential = true, portMappings = [{ containerPort = 5555, protocol = "tcp" }, { containerPort = 2222, protocol = "tcp" }], healthCheck = local.app_health_check, mountPoints = [{ sourceVolume = "sqlite", containerPath = "/var/lib/bleephub", readOnly = false }], environment = concat([{ name = "BLEEPHUB_PERSIST", value = "true" }, { name = "BLEEPHUB_DATA_DIR", value = "/var/lib/bleephub" }, { name = "BLEEPHUB_DQLITE_SERVERS", value = join(",", [for node in sort(keys(local.dqlite_nodes)) : local.dqlite_live_addresses[node]]) }, { name = "BLEEPHUB_S3_BUCKET", value = aws_s3_bucket.git.bucket }, { name = "BLEEPHUB_S3_PREFIX", value = "git" }, { name = "BLEEPHUB_OBJECT_S3_BUCKET", value = aws_s3_bucket.objects.bucket }, { name = "BLEEPHUB_OBJECT_S3_PREFIX", value = "objects" }, { name = "BLEEPHUB_S3_REGION", value = var.region }, { name = "BLEEPHUB_EXTERNAL_URL", value = "https://${var.domain_name}" }, { name = "BLEEPHUB_ADMIN_HOST", value = "admin.${var.domain_name}" }, { name = "BLEEPHUB_SSH_ADDR", value = ":2222" }, { name = "BLEEPHUB_SSH_HOST", value = "ssh.${var.domain_name}" }], local.dqlite_address_map == "" ? [] : [{ name = "BLEEPHUB_DQLITE_ADDRESS_MAP", value = local.dqlite_address_map }], var.github_oauth_client_id == "" ? [] : [{ name = "BLEEPHUB_GITHUB_OAUTH_CLIENT_ID", value = var.github_oauth_client_id }], var.shauth_oidc_issuer == "" ? [] : [{ name = "BLEEPHUB_SHAUTH_ISSUER", value = var.shauth_oidc_issuer }, { name = "BLEEPHUB_SHAUTH_CLIENT_ID", value = var.shauth_oidc_client_id }, { name = "BLEEPHUB_SHAUTH_POST_LOGOUT_URL", value = var.shauth_oidc_post_logout_url }], var.otel_exporter_otlp_endpoint == "" ? [] : [{ name = "OTEL_EXPORTER_OTLP_ENDPOINT", value = var.otel_exporter_otlp_endpoint }, { name = "OTEL_SERVICE_NAME", value = var.otel_service_name }]), secrets = concat([{ name = "BLEEPHUB_ADMIN_TOKEN", valueFrom = aws_secretsmanager_secret.admin_token.arn }, { name = "BLEEPHUB_SSH_HOST_KEY", valueFrom = aws_secretsmanager_secret.ssh_host_key.arn }, { name = "BLEEPHUB_DQLITE_SECRET", valueFrom = aws_secretsmanager_secret.dqlite_secret.arn }, { name = "BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY", valueFrom = aws_secretsmanager_secret.persistence_encryption_key.arn }], var.github_oauth_client_secret_arn == "" ? [] : [{ name = "BLEEPHUB_GITHUB_OAUTH_CLIENT_SECRET", valueFrom = var.github_oauth_client_secret_arn }], var.shauth_oidc_client_secret_arn == "" ? [] : [{ name = "BLEEPHUB_SHAUTH_CLIENT_SECRET", valueFrom = var.shauth_oidc_client_secret_arn }]), logConfiguration = { logDriver = "awslogs", options = { awslogs-group = aws_cloudwatch_log_group.this.name, awslogs-region = var.region, awslogs-stream-prefix = "service" } } }])
  tags                  = local.common_tags

  lifecycle {
    # The caller owns the provider, so region is a second writer for every ARN,
    # endpoint, and container environment value the module composes by hand.
    precondition {
      condition     = var.region == data.aws_region.current.region
      error_message = "region must equal the region of the AWS provider passed to this module."
    }
    # Keep relationships between inputs out of variable validation blocks.
    # Terraform module registries and input-form parsers commonly implement
    # the legacy rule that a variable may validate only itself.
    precondition {
      condition     = alltrue([for zone in var.availability_zones : startswith(zone, var.region)])
      error_message = "Every entry of availability_zones must be an Availability Zone of region; the two defaults drift apart otherwise."
    }
    precondition {
      condition = (
        (var.create_api_gateway_vpc_link && var.api_gateway_vpc_link_id == null && var.api_gateway_vpc_link_security_group_id == null) ||
        (!var.create_api_gateway_vpc_link && var.api_gateway_vpc_link_id != null && trimspace(var.api_gateway_vpc_link_id) != "" && var.api_gateway_vpc_link_security_group_id != null && trimspace(var.api_gateway_vpc_link_security_group_id) != "")
      )
      error_message = "Leave both shared VPC Link coordinates null when create_api_gateway_vpc_link is true, or set both to non-empty values when it is false."
    }
    precondition {
      condition = (
        (var.existing_vpc_id == "" && length(var.existing_private_subnet_ids) == 0 && length(var.existing_public_subnet_ids) == 0 && var.existing_ecs_cluster_arn == "") ||
        (var.existing_vpc_id != "" && length(var.existing_private_subnet_ids) >= 2 && length(var.existing_public_subnet_ids) >= 2 && var.existing_ecs_cluster_arn != "")
      )
      error_message = "Configure all existing-network inputs together: VPC ID, at least two private subnets, at least two public subnets, and an ECS cluster ARN."
    }
    precondition {
      condition     = (var.shauth_oidc_issuer == "" && var.shauth_oidc_client_id == "" && var.shauth_oidc_client_secret_arn == "" && var.shauth_oidc_post_logout_url == "") || (var.shauth_oidc_issuer != "" && var.shauth_oidc_client_id != "" && var.shauth_oidc_client_secret_arn != "" && var.shauth_oidc_post_logout_url != "")
      error_message = "shauth_oidc_issuer, shauth_oidc_client_id, shauth_oidc_client_secret_arn, and shauth_oidc_post_logout_url must be configured together."
    }
    precondition {
      condition     = var.shauth_oidc_post_logout_url == "" || var.shauth_oidc_post_logout_url == "https://${var.domain_name}/auth/shauth/logout/complete"
      error_message = "shauth_oidc_post_logout_url must be the Bleephub-origin logout-completion bridge https://<domain_name>/auth/shauth/logout/complete."
    }
  }
}

resource "aws_ecs_task_definition" "ssh_gateway" {
  family                   = "${var.name}-ssh-gateway"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }
  execution_role_arn = aws_iam_role.execution.arn
  task_role_arn      = aws_iam_role.ssh_gateway.arn
  container_definitions = jsonencode([{
    name         = "ssh-gateway"
    image        = var.container_image
    essential    = true
    entryPoint   = ["/usr/local/bin/bleephub-ssh-gateway"]
    portMappings = [{ containerPort = 2222, protocol = "tcp" }]
    environment = [
      { name = "BLEEPHUB_WAKE_URL", value = "https://${var.domain_name}/health" },
      { name = "BLEEPHUB_INTERNAL_SSH_TARGET", value = "${aws_service_discovery_service.app.name}.${aws_service_discovery_private_dns_namespace.this.name}" }
    ]
    logConfiguration = { logDriver = "awslogs", options = { awslogs-group = aws_cloudwatch_log_group.this.name, awslogs-region = var.region, awslogs-stream-prefix = "ssh-gateway" } }
  }])
  tags = local.common_tags
}

resource "aws_ecs_task_definition" "dqlite" {
  for_each                 = local.dqlite_nodes
  family                   = "${var.name}-dqlite-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "512"
  memory                   = "1024"
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }
  execution_role_arn = aws_iam_role.execution.arn
  task_role_arn      = aws_iam_role.task.arn
  volume {
    name = "dqlite"
    efs_volume_configuration {
      file_system_id     = aws_efs_file_system.sqlite.id
      transit_encryption = "ENABLED"
      authorization_config {
        access_point_id = aws_efs_access_point.dqlite[each.key].id
        iam             = "DISABLED"
      }
    }
  }
  container_definitions = jsonencode([{
    name         = "dqlite"
    image        = var.container_image
    essential    = true
    entryPoint   = ["/usr/local/bin/bleephub-dqlite-node"]
    portMappings = [{ containerPort = 9000, protocol = "tcp" }]
    mountPoints  = [{ sourceVolume = "dqlite", containerPath = "/var/lib/dqlite", readOnly = false }]
    environment = concat([
      { name = "BLEEPHUB_DQLITE_DATA_DIR", value = "/var/lib/dqlite" },
      { name = "BLEEPHUB_DQLITE_ADVERTISE_ADDR", value = local.dqlite_advertise_addresses[each.key] }
    ], local.dqlite_address_map == "" ? [] : [{ name = "BLEEPHUB_DQLITE_ADDRESS_MAP", value = local.dqlite_address_map }], each.key == "0" ? [] : [{ name = "BLEEPHUB_DQLITE_JOIN", value = local.dqlite_live_addresses["0"] }])
    secrets          = [{ name = "BLEEPHUB_DQLITE_SECRET", valueFrom = aws_secretsmanager_secret.dqlite_secret.arn }]
    logConfiguration = { logDriver = "awslogs", options = { awslogs-group = aws_cloudwatch_log_group.this.name, awslogs-region = var.region, awslogs-stream-prefix = "dqlite-${each.key}" } }
  }])
  tags = merge(local.common_tags, { Name = "${var.name}-dqlite-${each.key}" })
}

resource "aws_ecs_service" "this" {
  name            = var.name
  cluster         = local.ecs_cluster_arn
  task_definition = aws_ecs_task_definition.this.arn

  # SINGLE-WRITER CEILING (CI-036): desired_count is 0 (idle) or exactly 1, and
  # there is deliberately NO aws_appautoscaling_target/policy for this service.
  # bleephub's application state is an in-memory store guarded by a process-local
  # mutex; a second serving task would own a divergent copy of that state, so the
  # API is architecturally capped at one running task. Durability is provided out
  # of band by the dqlite quorum (its own service below), not by API replicas.
  # Horizontal scaling of the API is therefore not a Terraform knob — it requires
  # the multi-writer refactor tracked as ARCH-001. The wake controller owns the
  # 0↔1 transition at runtime (see the ignore_changes below).
  desired_count = var.idle_shutdown_enabled ? 0 : 1
  launch_type   = "FARGATE"

  # A replacement task must pass its health check before the serving one is
  # taken away. Metadata is served by the independent dqlite quorum, and the
  # two tasks that briefly overlap write disjoint files under the EFS mount.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  availability_zone_rebalancing      = "DISABLED"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = local.private_subnet_ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = false
  }
  service_registries {
    registry_arn   = aws_service_discovery_service.app.arn
    container_name = "bleephub"
    container_port = 5555
  }
  depends_on = [aws_efs_mount_target.sqlite]
  tags       = local.common_tags

  # The wake controller owns capacity once the service exists; desired_count
  # here is only the initial value. Reconciling it would stop a live service.
  lifecycle {
    ignore_changes = [desired_count]
  }
}

resource "aws_ecs_service" "dqlite" {
  for_each                      = local.dqlite_nodes
  name                          = "${var.name}-dqlite-${each.key}"
  cluster                       = local.ecs_cluster_arn
  task_definition               = aws_ecs_task_definition.dqlite[each.key].arn
  desired_count                 = var.idle_shutdown_enabled ? 0 : 1
  launch_type                   = "FARGATE"
  availability_zone_rebalancing = "DISABLED"

  # A voter owns one raft directory on one EFS access point. Two tasks of the
  # same voter would write the same log, so this one replaces rather than
  # overlaps; the circuit breaker is what stops a bad release looping.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = local.private_subnet_ids
    security_groups  = [aws_security_group.dqlite.id]
    assign_public_ip = false
  }
  service_registries {
    # An awsvpc Amazon ECS service backed by an A record registers its task
    # ENI address; Amazon ECS only accepts a port coordinate for SRV records.
    registry_arn = aws_service_discovery_service.dqlite[each.key].arn
  }
  depends_on = [aws_efs_mount_target.sqlite]
  tags       = merge(local.common_tags, { Name = "${var.name}-dqlite-${each.key}" })

  # The wake controller scales the quorum with the application service.
  lifecycle {
    ignore_changes = [desired_count]
  }
}

resource "aws_ecs_service" "ssh_gateway" {
  name            = "${var.name}-ssh-gateway"
  cluster         = local.ecs_cluster_arn
  task_definition = aws_ecs_task_definition.ssh_gateway.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = local.private_subnet_ids
    security_groups  = [aws_security_group.ssh_gateway.id]
    assign_public_ip = false
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.ssh.arn
    container_name   = "ssh-gateway"
    container_port   = 2222
  }
  depends_on = [aws_lb_listener.ssh]
  tags       = local.common_tags
}

resource "aws_lambda_function" "wake" {
  filename         = var.wake_listener_zip_path
  function_name    = "${var.name}-wake"
  role             = aws_iam_role.wake.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  source_code_hash = filebase64sha256(var.wake_listener_zip_path)
  timeout          = 120
  tracing_config { mode = "Active" }

  environment {
    variables = {
      ECS_CLUSTER                 = local.ecs_cluster_name
      ECS_SERVICE                 = aws_ecs_service.this.name
      API_ID                      = aws_apigatewayv2_api.this.id
      SERVICE_INTEGRATION_ID      = aws_apigatewayv2_integration.service.id
      DQLITE_SERVICES             = join(",", [for node in sort(keys(local.dqlite_nodes)) : aws_ecs_service.dqlite[node].name])
      IDLE_ALARM_NAME             = aws_cloudwatch_metric_alarm.idle_shutdown.alarm_name
      IDLE_ARM_FUNCTION_ARN       = aws_lambda_function.idle_arm.arn
      IDLE_ARM_SCHEDULER_ROLE_ARN = aws_iam_role.idle_arm_scheduler.arn
      IDLE_ARM_SCHEDULE_NAME      = "${var.name}-idle-arm"
      IDLE_SHUTDOWN_ENABLED       = tostring(var.idle_shutdown_enabled)
      IDLE_SHUTDOWN_MINUTES       = tostring(var.idle_shutdown_minutes)
      ADMIN_TOKEN_SECRET_ARN      = aws_secretsmanager_secret.admin_token.arn
    }
  }

  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "idle_shutdown" {
  alarm_name          = "${var.name}-five-minute-idle"
  alarm_description   = var.idle_shutdown_enabled ? "Stops Bleephub after ${var.idle_shutdown_minutes} minutes without Amazon API Gateway requests." : "Automatic Bleephub idle shutdown is disabled."
  comparison_operator = "LessThanOrEqualToThreshold"
  evaluation_periods  = var.idle_shutdown_minutes
  metric_name         = "Count"
  namespace           = "AWS/ApiGateway"
  period              = 60
  statistic           = "Sum"
  threshold           = 0
  treat_missing_data  = "breaching"
  # The wake controller enables this only after the API Gateway request that
  # woke the stack is visible to CloudWatch, avoiding a stale zero window.
  actions_enabled = false
  alarm_actions   = [aws_lambda_function.idle_shutdown.arn]

  lifecycle {
    ignore_changes = [actions_enabled]
  }

  dimensions = {
    ApiId = aws_apigatewayv2_api.this.id
    Stage = aws_apigatewayv2_stage.default.name
  }

  tags = local.common_tags
}

resource "aws_lambda_function" "idle_shutdown" {
  filename         = var.wake_listener_zip_path
  function_name    = "${var.name}-idle-shutdown"
  role             = aws_iam_role.wake.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  source_code_hash = filebase64sha256(var.wake_listener_zip_path)
  timeout          = 300
  tracing_config { mode = "Active" }

  environment {
    variables = {
      ECS_CLUSTER            = local.ecs_cluster_name
      ECS_SERVICE            = aws_ecs_service.this.name
      IDLE_SHUTDOWN          = "true"
      IDLE_ALARM_NAME        = "${var.name}-five-minute-idle"
      API_ID                 = aws_apigatewayv2_api.this.id
      SERVICE_INTEGRATION_ID = aws_apigatewayv2_integration.service.id
      DQLITE_SERVICES        = join(",", [for node in sort(keys(local.dqlite_nodes)) : aws_ecs_service.dqlite[node].name])
    }
  }

  tags = local.common_tags
}

resource "aws_lambda_function" "idle_arm" {
  filename         = var.wake_listener_zip_path
  function_name    = "${var.name}-idle-arm"
  role             = aws_iam_role.wake.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  source_code_hash = filebase64sha256(var.wake_listener_zip_path)
  timeout          = 30
  tracing_config { mode = "Active" }

  environment {
    variables = {
      IDLE_ARM                   = "true"
      IDLE_ALARM_NAME            = aws_cloudwatch_metric_alarm.idle_shutdown.alarm_name
      IDLE_SHUTDOWN_FUNCTION_ARN = aws_lambda_function.idle_shutdown.arn
    }
  }

  tags = local.common_tags
}

resource "aws_lambda_permission" "idle_shutdown_alarm" {
  statement_id  = "allow-cloudwatch-idle-shutdown"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.idle_shutdown.function_name
  principal     = "lambda.alarms.cloudwatch.amazonaws.com"
  source_arn    = aws_cloudwatch_metric_alarm.idle_shutdown.arn
}

# ─── Cost guardrails (CI-037) ────────────────────────────────────────────────
# A monthly cost budget and account-wide cost-anomaly detection. Both always
# exist so spend is visible in the console; email notifications are wired only
# when var.alert_email is set (Budgets and Cost Explorer are global services and
# use the default provider).

resource "aws_budgets_budget" "monthly" {
  name         = "${var.name}-monthly"
  budget_type  = "COST"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  dynamic "notification" {
    for_each = var.alert_email != "" ? {
      forecasted = { type = "FORECASTED", threshold = 80 }
      actual     = { type = "ACTUAL", threshold = 100 }
    } : {}
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value.threshold
      threshold_type             = "PERCENTAGE"
      notification_type          = notification.value.type
      subscriber_email_addresses = [var.alert_email]
    }
  }
}

resource "aws_ce_anomaly_monitor" "service" {
  name              = "${var.name}-anomaly"
  monitor_type      = "DIMENSIONAL"
  monitor_dimension = "SERVICE"
  tags              = local.common_tags
}

resource "aws_ce_anomaly_subscription" "default" {
  count            = var.alert_email != "" ? 1 : 0
  name             = "${var.name}-anomaly-sub"
  frequency        = "DAILY"
  monitor_arn_list = [aws_ce_anomaly_monitor.service.arn]

  subscriber {
    type    = "EMAIL"
    address = var.alert_email
  }

  threshold_expression {
    dimension {
      key           = "ANOMALY_TOTAL_IMPACT_ABSOLUTE"
      match_options = ["GREATER_THAN_OR_EQUAL"]
      values        = ["25"]
    }
  }

  tags = local.common_tags
}

# ─── Operational alerting (CI-024) ───────────────────────────────────────────
# The idle_shutdown alarm above turns the service OFF; it is a control signal,
# not an error signal. This SNS topic and the error alarms below surface actual
# failures. CloudWatch alarm actions require an ARN, so email delivery is via an
# SNS subscription wired only when var.alert_email is set.

resource "aws_sns_topic" "alerts" {
  name = "${var.name}-alerts"
  # Encrypt with the module's customer-managed key (not the AWS-managed
  # alias/aws/sns) so the topic meets the same CMK bar as every other durable
  # store here; the KMS key policy grants CloudWatch use of the key below.
  kms_master_key_id = aws_kms_key.this.arn
  tags              = local.common_tags
}

resource "aws_sns_topic_subscription" "alerts_email" {
  count     = var.alert_email != "" ? 1 : 0
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

# A sustained spike of API Gateway server errors (5xx) means the app is failing
# requests — distinct from the idle-shutdown control alarm.
resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${var.name}-api-5xx"
  namespace           = "AWS/ApiGateway"
  metric_name         = "5xx"
  dimensions          = { ApiId = aws_apigatewayv2_api.this.id }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
  tags                = local.common_tags
}

# A failing wake/idle controller Lambda can strand the service off or fail to
# scale it down, so any invocation error pages.
resource "aws_cloudwatch_metric_alarm" "lambda_errors" {
  for_each = {
    wake          = aws_lambda_function.wake.function_name
    idle_shutdown = aws_lambda_function.idle_shutdown.function_name
    idle_arm      = aws_lambda_function.idle_arm.function_name
  }
  alarm_name          = "${var.name}-lambda-errors-${each.key}"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  tags                = local.common_tags
}
