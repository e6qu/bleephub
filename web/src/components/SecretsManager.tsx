import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchScopedSecrets,
  fetchScopedPublicKey,
  putScopedSecret,
  deleteScopedSecret,
  fetchScopedVariables,
  createScopedVariable,
  updateScopedVariable,
  deleteScopedVariable,
  type SecretsScope,
} from "../api.js";
import type { GithubOrgVisibility, GithubVariable } from "../types.js";
import { sealSecret } from "../utils/sealedBox.js";
import { confirmAction } from "./confirmAction.js";
import {
  Box,
  Blankslate,
  Button,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
  SectionLabel,
} from "./ui.js";
import { LockIcon, KeyIcon } from "./octicons.js";

// Actions secrets + variables manager for any scope (repo / environment / org).
// Secrets are sealed-box encrypted in the browser against the scope's public
// key before upload.
function scopeKey(scope: SecretsScope): string {
  switch (scope.kind) {
    case "repo":
      return `repo:${scope.owner}/${scope.repo}`;
    case "env":
      return `env:${scope.owner}/${scope.repo}/${scope.env}`;
    case "org":
      return `org:${scope.org}`;
  }
}

// ─── Secrets ─────────────────────────────────────────────────────────────

export function SecretsSection({ scope }: { scope: SecretsScope }) {
  const qc = useQueryClient();
  const key = scopeKey(scope);
  const [editing, setEditing] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const secretsQ = useQuery({
    queryKey: ["actions-secrets", key],
    queryFn: () => fetchScopedSecrets(scope),
  });

  const deleteMutation = useMutation({
    mutationFn: (name: string) => deleteScopedSecret(scope, name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["actions-secrets", key] }),
  });

  return (
    <section aria-label="Secrets">
      <div className="mb-2 flex items-center justify-between">
        <SectionLabel>Secrets</SectionLabel>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New secret
        </Button>
      </div>
      {secretsQ.isLoading && <Spinner label="loading secrets" />}
      {secretsQ.isError && (
        <InlineError title="Failed to load secrets" detail={String(secretsQ.error)} />
      )}
      {deleteMutation.isError && <ErrorBanner>{String(deleteMutation.error)}</ErrorBanner>}
      {secretsQ.data &&
        (secretsQ.data.items.length === 0 ? (
          <Blankslate icon={<LockIcon size={24} />} title="No secrets" />
        ) : (
          <Box>
            {secretsQ.data.items.map((s, i) => (
              <div
                key={s.name}
                className="flex items-center gap-2"
                style={{
                  padding: "0.55rem 1rem",
                  borderBottom:
                    i < secretsQ.data.items.length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <LockIcon size={14} style={{ color: "var(--color-fg-muted)" }} />
                <span className="min-w-0 flex-1 truncate font-mono" style={{ fontSize: "0.84rem", color: "var(--color-fg)" }}>
                  {s.name}
                </span>
                {s.visibility && (
                  <span style={{ fontSize: "0.72rem", color: "var(--color-fg-subtle)" }}>
                    {s.visibility}
                  </span>
                )}
                <span style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)" }}>
                  updated {new Date(s.updated_at).toLocaleDateString()}
                </span>
                <Button variant="ghost" size="sm" onClick={() => setEditing(s.name)}>
                  Update
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  disabled={deleteMutation.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Delete secret ${s.name}?`)) deleteMutation.mutate(s.name);
                  }}
                >
                  Delete
                </Button>
              </div>
            ))}
          </Box>
        ))}
      {(creating || editing !== null) && (
        <SecretModal
          scope={scope}
          existingName={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
    </section>
  );
}

function SecretModal({
  scope,
  existingName,
  onClose,
}: {
  scope: SecretsScope;
  existingName: string | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(existingName ?? "");
  const [value, setValue] = useState("");
  const [visibility, setVisibility] = useState<GithubOrgVisibility>("all");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: async () => {
      const secretName = name.trim();
      if (!secretName) throw new Error("Name is required");
      if (!value) throw new Error("Value is required");
      // Sealed-box encrypt; the PUT body carries ciphertext + key_id only.
      const pk = await fetchScopedPublicKey(scope);
      const encrypted = await sealSecret(value, pk.key);
      await putScopedSecret(scope, secretName, {
        encrypted_value: encrypted,
        key_id: pk.key_id,
        ...(scope.kind === "org" ? { visibility } : {}),
      });
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["actions-secrets", scopeKey(scope)] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={existingName ? `Update secret ${existingName}` : "New secret"} onClose={onClose}>
      {existingName === null && (
        <>
          <FormLabel id="secret-name">Name</FormLabel>
          <input
            id="secret-name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="YOUR_SECRET_NAME"
            className="mb-4 w-full font-mono"
          />
        </>
      )}
      <FormLabel id="secret-value">Value</FormLabel>
      <textarea
        id="secret-value"
        rows={4}
        value={value}
        onChange={(e) => setValue(e.target.value)}
        className="mb-4 w-full font-mono"
        style={{ resize: "vertical" }}
      />
      {scope.kind === "org" && (
        <>
          <FormLabel id="secret-visibility">Repository access</FormLabel>
          <select
            id="secret-visibility"
            value={visibility}
            onChange={(e) => setVisibility(e.target.value as GithubOrgVisibility)}
            className="mb-4 w-full"
          >
            <option value="all">All repositories</option>
            <option value="private">Private repositories</option>
            <option value="selected">Selected repositories</option>
          </select>
        </>
      )}
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button onClick={onClose} disabled={mutation.isPending} variant="ghost">
          Cancel
        </Button>
        <Button
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
          variant="primary"
        >
          {mutation.isPending ? "Encrypting…" : existingName ? "Update secret" : "Add secret"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

// ─── Variables ───────────────────────────────────────────────────────────

export function VariablesSection({ scope }: { scope: SecretsScope }) {
  const qc = useQueryClient();
  const key = scopeKey(scope);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<GithubVariable | null>(null);

  const variablesQ = useQuery({
    queryKey: ["actions-variables", key],
    queryFn: () => fetchScopedVariables(scope),
  });

  const deleteMutation = useMutation({
    mutationFn: (name: string) => deleteScopedVariable(scope, name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["actions-variables", key] }),
  });

  return (
    <section aria-label="Variables">
      <div className="mb-2 flex items-center justify-between">
        <SectionLabel>Variables</SectionLabel>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New variable
        </Button>
      </div>
      {variablesQ.isLoading && <Spinner label="loading variables" />}
      {variablesQ.isError && (
        <InlineError title="Failed to load variables" detail={String(variablesQ.error)} />
      )}
      {deleteMutation.isError && <ErrorBanner>{String(deleteMutation.error)}</ErrorBanner>}
      {variablesQ.data &&
        (variablesQ.data.items.length === 0 ? (
          <Blankslate icon={<KeyIcon size={24} />} title="No variables" />
        ) : (
          <Box>
            {variablesQ.data.items.map((v, i) => (
              <div
                key={v.name}
                className="flex items-center gap-2"
                style={{
                  padding: "0.55rem 1rem",
                  borderBottom:
                    i < variablesQ.data.items.length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <span className="min-w-0 flex-1 truncate font-mono" style={{ fontSize: "0.84rem", color: "var(--color-fg)" }}>
                  {v.name}
                </span>
                <span
                  className="min-w-0 flex-1 truncate font-mono"
                  style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}
                >
                  {v.value}
                </span>
                {v.visibility && (
                  <span style={{ fontSize: "0.72rem", color: "var(--color-fg-subtle)" }}>
                    {v.visibility}
                  </span>
                )}
                <Button variant="ghost" size="sm" onClick={() => setEditing(v)}>
                  Edit
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  disabled={deleteMutation.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Delete variable ${v.name}?`)) deleteMutation.mutate(v.name);
                  }}
                >
                  Delete
                </Button>
              </div>
            ))}
          </Box>
        ))}
      {(creating || editing !== null) && (
        <VariableModal
          scope={scope}
          existing={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
    </section>
  );
}

function VariableModal({
  scope,
  existing,
  onClose,
}: {
  scope: SecretsScope;
  existing: GithubVariable | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(existing?.name ?? "");
  const [value, setValue] = useState(existing?.value ?? "");
  const [visibility, setVisibility] = useState<GithubOrgVisibility>(existing?.visibility ?? "all");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: async () => {
      const varName = name.trim();
      if (!varName) throw new Error("Name is required");
      if (existing) {
        await updateScopedVariable(scope, existing.name, { name: varName, value });
      } else {
        await createScopedVariable(scope, {
          name: varName,
          value,
          ...(scope.kind === "org" ? { visibility } : {}),
        });
      }
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["actions-variables", scopeKey(scope)] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={existing ? `Edit variable ${existing.name}` : "New variable"} onClose={onClose}>
      <FormLabel id="variable-name">Name</FormLabel>
      <input
        id="variable-name"
        type="text"
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="YOUR_VARIABLE_NAME"
        className="mb-4 w-full font-mono"
      />
      <FormLabel id="variable-value">Value</FormLabel>
      <textarea
        id="variable-value"
        rows={3}
        value={value}
        onChange={(e) => setValue(e.target.value)}
        className="mb-4 w-full font-mono"
        style={{ resize: "vertical" }}
      />
      {scope.kind === "org" && !existing && (
        <>
          <FormLabel id="variable-visibility">Repository access</FormLabel>
          <select
            id="variable-visibility"
            value={visibility}
            onChange={(e) => setVisibility(e.target.value as GithubOrgVisibility)}
            className="mb-4 w-full"
          >
            <option value="all">All repositories</option>
            <option value="private">Private repositories</option>
            <option value="selected">Selected repositories</option>
          </select>
        </>
      )}
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button onClick={onClose} disabled={mutation.isPending} variant="ghost">
          Cancel
        </Button>
        <Button
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
          variant="primary"
        >
          {mutation.isPending ? "Saving…" : existing ? "Update variable" : "Add variable"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
