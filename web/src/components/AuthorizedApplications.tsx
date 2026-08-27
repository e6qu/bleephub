import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { fetchOAuthGrants, revokeOAuthGrant } from "../api.js";
import { Box, Button, ErrorBanner } from "./ui.js";
import { confirmAction } from "./confirmAction.js";

export function AuthorizedApplications() {
  const queryClient = useQueryClient();
  const grants = useQuery({ queryKey: ["oauth-grants"], queryFn: fetchOAuthGrants });
  const revoke = useMutation({
    mutationFn: revokeOAuthGrant,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["oauth-grants"] }),
  });
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Authorized applications</span>}>
      <div style={{ padding: "1rem" }}>
        {grants.isLoading ? <Spinner label="loading authorized applications" /> : grants.isError ? (
          <InlineError title="Failed to load authorized applications" detail={String(grants.error)} />
        ) : grants.data?.length === 0 ? (
          <p style={{ color: "var(--color-fg-muted)" }}>You have not authorized any applications.</p>
        ) : (
          <div className="grid gap-3">
            {grants.data?.map((grant) => (
              <div key={grant.client_id} className="flex flex-wrap items-center justify-between gap-3 rounded border p-3">
                <div>
                  <b>{grant.name || grant.client_id}</b>
                  <div style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
                    {grant.type === "GitHubApp" ? "GitHub App" : "OAuth App"}
                    {grant.scopes.length > 0 ? ` · ${grant.scopes.join(", ")}` : ""}
                  </div>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={revoke.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Revoke authorization for ${grant.name || grant.client_id}?`)) {
                      revoke.mutate(grant.client_id);
                    }
                  }}
                >
                  Revoke
                </Button>
              </div>
            ))}
          </div>
        )}
        {revoke.error && <div className="mt-3"><ErrorBanner>{String(revoke.error)}</ErrorBanner></div>}
      </div>
    </Box>
  );
}
