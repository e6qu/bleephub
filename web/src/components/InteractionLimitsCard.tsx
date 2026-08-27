import { useEffect, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Box, Button } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { ghFetch, ghSend } from "../api.js";

// Temporary interaction limit. All scopes share one REST shape at `path`:
// GET `{ limit? }`, PUT `{ limit, expiry }` sets, DELETE clears. Serves the org
// and user scopes (repo has its own tab). Calls inline to keep the entry chunk flat.
export function InteractionLimitsCard({
  path,
  queryKey,
  scopeLabel,
}: {
  path: string;
  queryKey: (string | number)[];
  scopeLabel: string;
}) {
  const queryClient = useQueryClient();
  const [limit, setLimit] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);

  const limitQuery = useQuery({ queryKey, queryFn: () => ghFetch<{ limit?: string }>(path) });
  const loaded = limitQuery.data?.limit ?? null;
  useEffect(() => {
    if (!touched) setLimit(loaded);
  }, [loaded, touched]);

  const mutation = useMutation({
    mutationFn: (next: string | null) =>
      next === null ? ghSend("DELETE", path) : ghSend("PUT", path, { limit: next, expiry: "one_month" }),
    onSuccess: () => {
      setTouched(false);
      void queryClient.invalidateQueries({ queryKey });
    },
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Interaction limits</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Temporarily limit who can comment, open issues, and create pull requests {scopeLabel}. A limit
          automatically expires after one month.
        </p>
        {limitQuery.isLoading && <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>Loading…</span>}
        <select
          aria-label="Interaction limit"
          value={limit ?? ""}
          onChange={(e) => {
            setTouched(true);
            setLimit(e.target.value || null);
          }}
          disabled={mutation.isPending}
          style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem", maxWidth: "22rem" }}
        >
          <option value="">No limit</option>
          <option value="existing_users">Limit to existing users</option>
          <option value="contributors_only">Limit to prior contributors</option>
          <option value="collaborators_only">Limit to repository collaborators</option>
        </select>
        <MutationError of={mutation} />
        <div className="flex justify-end gap-2">
          <Button
            variant="ghost"
            onClick={() => {
              setTouched(true);
              setLimit(null);
              mutation.mutate(null);
            }}
            disabled={mutation.isPending}
          >
            Clear limit
          </Button>
          <Button variant="primary" onClick={() => mutation.mutate(limit)} disabled={mutation.isPending}>
            Set limit
          </Button>
        </div>
      </div>
    </Box>
  );
}
