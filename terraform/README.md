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

The module deliberately contains no backend or environment-specific values.
Use it through Terragrunt from the private `e6qu/infra` repository. The
production environment is `bleephub`, with `bleephub.e6qu.dev` as the public
origin and `admin.bleephub.e6qu.dev` as the administrator origin.

## Required inputs

- `name` — stable AWS resource prefix.
- `hosted_zone_id` — Route 53 hosted zone containing `bleephub.e6qu.dev`.
- `domain_name` — `bleephub.e6qu.dev`.
- `container_image` — immutable Bleephub release image coordinate.
- `admin_token` — initial administrator secret; provide it through the
  Terragrunt environment rather than committing it.
- `wake_listener_zip_path` — pre-built Linux Amazon Lambda wake-listener ZIP.
- `startup_page_path` — extracted `index.html` from the versioned startup ZIP.

To use a shared VPC, set `existing_vpc_id`,
`existing_private_subnet_ids`, `existing_public_subnet_ids`, and
`existing_ecs_cluster_arn` together. The module then creates no VPC, subnets,
route tables, fck-nat instance, Amazon Simple Storage Service endpoint, or ECS
cluster. It continues to create Bleephub-scoped security groups, EFS mount
targets, AWS Cloud Map discovery services, and Amazon ECS services in the
supplied network. HTTP traffic uses Amazon API Gateway directly through a VPC
link to AWS Cloud Map; the only Network Load Balancer is the public raw-SSH
endpoint because Amazon API Gateway does not proxy SSH/TCP.

`github_oauth_client_id` and `github_oauth_client_secret_arn` enable the
registered GitHub OAuth App. The secret ARN references an existing AWS Secrets
Manager secret so Terraform never receives the OAuth client secret value.

`shauth_oidc_issuer`, `shauth_oidc_client_id`,
`shauth_oidc_client_secret_arn`, and `shauth_oidc_post_logout_url` enroll
Bleephub with Shauth without changing its GitHub-compatible OAuth endpoints.
Set all four together. Register the exact redirect URI
`https://<domain_name>/auth/shauth/callback`, post-logout redirect URI
`https://<domain_name>/auth/signed-out` through
`shauth_oidc_post_logout_url`, and Back-Channel Logout URI
`https://<domain_name>/auth/shauth/backchannel-logout`. Bleephub uses OpenID
Connect discovery, PKCE, nonce binding, signed ID-token validation,
RP-Initiated Logout, and signed Back-Channel Logout; the client secret remains
only in AWS Secrets Manager.

`idle_shutdown_enabled` defaults to `true`. Set it to `false` for an always-on
environment; the wake controller then leaves the application and dqlite services
running while the rest of the deployment stays unchanged.

## Outputs

The module returns the public Bleephub URL, administrator URL, SSH host,
service and Amazon API Gateway identifiers, durable object-store names, and
the AWS Secrets Manager ARN holding the administrator token.

## Validation

The module's Amazon Web Services simulator apply/destroy test lives in `test/`.
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
