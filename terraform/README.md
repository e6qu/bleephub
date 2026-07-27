# Bleephub Amazon Elastic Container Service on AWS Fargate Module

This Terraform module provisions Bleephub either with its own Amazon Web
Services network or inside an existing environment VPC and Amazon Elastic
Container Service cluster. It creates the private Amazon Elastic Container
Service on AWS Fargate services, native dqlite storage, Amazon Simple Storage
Service git/object storage, an Amazon Simple Storage Service startup document,
Amazon API Gateway wake routing, private administrator origin, SSH Git gateway,
Route 53 records, and certificate. Standalone mode also creates fck-nat and an
Amazon Simple Storage Service gateway endpoint; shared-network mode reuses the
environment's equivalents.

The module contains no environment-specific values. Use it through Terragrunt
from the private `e6qu/infra` repository. The production environment is
`bleephub`, with `bleephub.e6qu.dev` as the public origin and
`admin.bleephub.e6qu.dev` as the administrator origin.

## State backend

State holds the generated SSH host private key and the administrator token, so
it must never live on a laptop or in the repository. `versions.tf` declares an
S3 backend as a *partial* configuration — encryption and S3-native locking are
fixed there, and every coordinate is supplied at init:

```bash
terraform -chdir=terraform init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="key=bleephub/<environment>.tfstate" \
  -backend-config="region=<region>" \
  -backend-config="kms_key_id=<state-key-arn>"
```

The state bucket must have versioning and default encryption enabled and public
access fully blocked; it is not created by this module, because a module cannot
hold the state of its own backend. `use_lockfile = true` is S3-native locking,
which replaces the DynamoDB lock table on this Terraform line — no table is
needed or accepted.

Terragrunt supplies the same coordinates through its `remote_state` block; the
empty `backend "s3"` body is exactly the shape it expects. CI runs
`init -backend=false`, which skips the backend entirely, so validation and the
contract tests need no credentials.

One of the contract tests calls this root module as a child module, so every
`init`, `validate`, and `test` run prints a "Backend configuration ignored"
warning against `versions.tf`. It is expected and applies only to that nested
call; the backend still governs the real root.

`terraform fmt`, `validate`, and `test` are gated by the `terraform` job in
`.github/workflows/ci.yml` on the Terraform version pinned in `versions.tf`.
`.terraform.lock.hcl` records provider checksums for `linux_amd64`,
`linux_arm64`, `darwin_amd64`, and `darwin_arm64`; regenerate it with
`terraform providers lock -platform=…` for all four whenever a provider version
changes, or `init` re-resolves them.

## Required inputs

- `name` — stable AWS resource prefix.
- `hosted_zone_id` — Route 53 hosted zone containing `bleephub.e6qu.dev`.
- `domain_name` — `bleephub.e6qu.dev`.
- `container_image` — immutable Bleephub release image coordinate.
- `admin_token` — initial administrator secret; provide it through the
  Terragrunt environment rather than committing it.
- `ssh_ingress_cidr_blocks` — IPv4 CIDR blocks allowed to reach public SSH on
  port 22. There is no default: publishing SSH to an unstated audience is a
  decision the caller has to make explicitly, and `0.0.0.0/0` is rejected.
- `wake_listener_zip_path` — pre-built Linux Amazon Lambda wake-listener ZIP.
- `startup_page_path` — extracted `index.html` from the versioned startup ZIP.

`region` must equal the region of the AWS provider the caller passes in, and
every entry of `availability_zones` must belong to it. The module checks both
and refuses to plan otherwise rather than composing ARNs for one region while
deploying into another.

## Encryption and durability

One customer-managed KMS key, created by the module with annual rotation,
encrypts the Git bucket, the object bucket, the EFS filesystem, both AWS
Secrets Manager secrets, and the CloudWatch log group. Revoking or disabling
`alias/<name>` takes every one of them offline at once, which is the point.

The Git and object buckets are versioned and the EFS filesystem has an AWS
Backup policy, so an overwrite or a deletion has somewhere to restore from.
All three buckets block public access completely; the startup document is
served through a CloudFront distribution with an origin access control, so no
bucket policy grants anonymous reads.

An EFS filesystem cannot be re-keyed in place, so on an environment that was
deployed before the key existed the plan will stop on `prevent_destroy` rather
than replace the filesystem underneath the quorum. Migrating means restoring an
AWS Backup recovery point onto a new encrypted filesystem and moving the state
entry across; the buckets, secrets, and log group re-key in place and need no
such step.

Each of those stores also carries `prevent_destroy`, which Terraform accepts
only as a literal. `force_destroy_storage = true` empties the buckets but does
not lift the guard, so a deliberate teardown is:

```bash
terraform -chdir=terraform state rm \
  aws_kms_key.this \
  aws_s3_bucket.git aws_s3_bucket.objects \
  aws_efs_file_system.sqlite \
  aws_secretsmanager_secret.admin_token aws_secretsmanager_secret.ssh_host_key \
  aws_secretsmanager_secret.dqlite_secret
terraform -chdir=terraform destroy
```

The released resources then have to be deleted by hand, which is deliberate.

## Deployments

The application service deploys with `deployment_minimum_healthy_percent = 100`
and a container health check against `/health`, so a replacement task has to
pass before the serving one is taken away, and a deployment circuit breaker
rolls back a release that never becomes healthy. Each dqlite voter owns a
single raft directory on its own EFS access point, so those services replace
rather than overlap and rely on the circuit breaker alone.

`desired_count` is carried by `ignore_changes` on the application and dqlite
services: the wake controller owns capacity at runtime, and reconciling it from
Terraform would stop a live service mid-request. The value in the configuration
is only the count the service is created with.

## dqlite cluster secret

The dqlite wire protocol authenticates nothing, so the HTTP transport upgrade
carries a shared credential in an `X-Bleephub-Dqlite-Secret` header. Both ends
require it: the application refuses to open the database without
`BLEEPHUB_DQLITE_SECRET`, and each voter refuses connections that do not present
it. A partial wiring is worse than none — it looks configured and then fails at
boot.

The module generates the value itself (`random_password`, 64 alphanumeric
characters) and stores it in AWS Secrets Manager as `<name>/dqlite-secret`,
encrypted with the module's KMS key, alongside the administrator token and the
SSH host key. There is no input variable: nothing outside the cluster needs the
value, and asking an operator to invent one only adds a way to get it wrong. It
reaches the application task and all three voter tasks as an Amazon ECS
`secrets` entry, never as a plaintext task environment variable, so it does not
appear in `DescribeTaskDefinition` output.

It carries `prevent_destroy` like the other two secrets. Replacing it splits the
quorum, because members that restart with the new value cannot speak to members
still holding the old one — rotating it means restarting the application and all
three voters together.

To use a shared VPC, set `existing_vpc_id`,
`existing_private_subnet_ids`, `existing_public_subnet_ids`, and
`existing_ecs_cluster_arn` together. The module then creates no VPC, subnets,
route tables, fck-nat instance, Amazon Simple Storage Service endpoint, or ECS
cluster. It continues to create Bleephub-scoped security groups, EFS mount
targets, AWS Cloud Map discovery services, and Amazon ECS services in the
supplied network. HTTP traffic uses Amazon API Gateway directly through a VPC
link to AWS Cloud Map; the only Network Load Balancer is the public raw-SSH
endpoint because Amazon API Gateway does not proxy SSH/TCP.

By default, Bleephub creates a dedicated Amazon API Gateway VPC Link and its
ingress security group. Set `create_api_gateway_vpc_link = false`,
`api_gateway_vpc_link_id`, and `api_gateway_vpc_link_security_group_id` together
to reuse an environment-wide VPC Link instead. The explicit Boolean keeps
resource ownership known during planning even when the supplied IDs come from
resources created in the same Terraform plan. Shared-link mode creates neither
dedicated resource, connects the Bleephub API integration through the supplied
link, and permits application traffic only from the supplied link security
group. Supplying incomplete coordinates, or supplying shared coordinates while
dedicated mode is enabled, is invalid.

`github_oauth_client_id` and `github_oauth_client_secret_arn` enable the
registered GitHub OAuth App. The secret ARN references an existing AWS Secrets
Manager secret so Terraform never receives the OAuth client secret value.

`shauth_oidc_issuer`, `shauth_oidc_client_id`,
`shauth_oidc_client_secret_arn`, and `shauth_oidc_post_logout_url` enroll
Bleephub with Shauth without changing its GitHub-compatible OAuth endpoints.
Set all four together. Register the exact redirect URI
`https://<domain_name>/auth/shauth/callback`, post-logout redirect URI
`https://<domain_name>/auth/shauth/logout/complete` through
`shauth_oidc_post_logout_url`, Front-Channel Logout URI
`https://<domain_name>/auth/shauth/frontchannel-logout`, and Back-Channel
Logout URI `https://<domain_name>/auth/shauth/backchannel-logout`. Bleephub
uses OpenID Connect discovery, PKCE, nonce binding, signed ID-token validation,
RP-Initiated Logout, Front-Channel Logout, and signed Back-Channel Logout. The
fixed application bridge returns to Shauth's `/oauth/logout/complete` endpoint;
Shauth consumes its one-time correlation before returning to Bleephub's local
`/ui/signed-out` page. The client secret remains only in AWS Secrets Manager.

`idle_shutdown_enabled` defaults to `true`. Set it to `false` for an always-on
environment; the wake controller then leaves the application and dqlite services
running while the rest of the deployment stays unchanged.

## Outputs

The module returns the public Bleephub URL, administrator URL, SSH host,
service and Amazon API Gateway identifiers, the effective Amazon API Gateway
VPC Link and security-group identifiers, durable object-store names, the AWS
Secrets Manager ARN holding the administrator token, the KMS key ARN, and the
startup distribution's domain name.

## Validation

The module's Terraform contract tests live under `terraform/tests/`; its Amazon
Web Services simulator apply/destroy test lives in `test/`.
Build the wake-listener artifact with:

```bash
scripts/build-bleephub-wake.sh
scripts/build-bleephub-startup.sh
```

The post-merge release workflow publishes the startup ZIP and Linux ARM64
wake-listener ZIP as immutable
`ghcr.io/e6qu/bleephub-startup:<short-sha>` and
`ghcr.io/e6qu/bleephub-wake:<short-sha>` GitHub Container Registry packages. It
retains the newest 20 versions of each. Terragrunt consumes the extracted
artifacts, so the public and administrator origins can show a dehydrated startup
view and wake the service without compiling source during deployment.
