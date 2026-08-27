# Security policy

Bleephub reimplements GitHub's server-side surface. It handles personal access
tokens, GitHub App private keys, OAuth client secrets, SSH host keys, Actions
secrets, and OIDC identities, so a defect here is usually a credential defect.

## Reporting a vulnerability

Report privately through GitHub's [security advisory
form](https://github.com/e6qu/bleephub/security/advisories/new). Do not open a
public issue for anything that discloses or bypasses a credential.

Include the request or workflow that reproduces it, what you expected, and what
happened. A concrete request beats a description.

## Deployment expectations

Bleephub is a GitHub-compatible server, not a hardened multi-tenant service.
Treat an instance as trusted infrastructure:

- `BLEEPHUB_ADMIN_TOKEN` is a superuser credential with no default. Set it to a
  value that is not personal-access-token shaped, and rotate it like a root
  password.
- Serve over TLS. Set `BPH_TLS_CERT` and `BPH_TLS_KEY` together.
- Do not expose the `/_apis/` runner protocol or the `/internal/` operator
  surface to an untrusted network.
- The instance can decrypt Actions secrets. Do not put credentials in a Bleephub
  instance that you would not put on the host running it.

## Known gaps

`BUGS.md` is the current defect ledger and is deliberately public. Entries
marked `open` under the `AUTH` and `REST` sections describe known authorization
and disclosure defects being fixed; read it before exposing an instance beyond a
trusted network.
