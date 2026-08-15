import { useState } from "react";
import { useParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { fetchEnvironments, type SecretsScope } from "../api.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { RepoHeader } from "../components/PageHeader.js";
import { Tabs, SectionLabel } from "../components/ui.js";
import { SecretsSection, VariablesSection } from "../components/SecretsManager.js";

type ScopeKind = "repo" | "env" | "org";


export function RepoSecretsPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const counts = useOpenCounts(owner, repo);
  const [kind, setKind] = useState<ScopeKind>("repo");
  const [envName, setEnvName] = useState("");

  const envsQ = useQuery({
    queryKey: ["environments", owner, repo],
    queryFn: () => fetchEnvironments(owner, repo),
    enabled: kind === "env" && !!owner && !!repo,
  });
  const envs = envsQ.data ?? [];
  const effectiveEnv = envName || envs[0]?.name || "";

  const scope: SecretsScope | null =
    kind === "repo"
      ? { kind: "repo", owner, repo }
      : kind === "org"
        ? { kind: "org", org: owner }
        : effectiveEnv
          ? { kind: "env", owner, repo, env: effectiveEnv }
          : null;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="settings" {...counts} />
      <SectionLabel>Actions secrets and variables</SectionLabel>
      <p className="mb-4" style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)" }}>
        Secrets are encrypted in the browser with the scope&apos;s public key before upload and
        are never readable again from this page. Variables are stored as plain text.
      </p>
      <Tabs
        items={[
          { key: "repo", label: "Repository" },
          { key: "env", label: "Environments" },
          { key: "org", label: `Organization (${owner})` },
        ]}
        active={kind}
        onChange={setKind}
      />

      {kind === "env" && (
        <div className="mb-4 flex items-center gap-2">
          <label htmlFor="secrets-env-select" style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)" }}>
            Environment
          </label>
          {envsQ.isLoading && <Spinner label="loading environments" />}
          {envsQ.isError && (
            <InlineError inline title="Failed to load environments" detail={String(envsQ.error)} />
          )}
          {envsQ.data &&
            (envs.length === 0 ? (
              <span style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)" }}>
                This repository has no environments yet.
              </span>
            ) : (
              <select
                id="secrets-env-select"
                value={effectiveEnv}
                onChange={(e) => setEnvName(e.target.value)}
                style={{
                  padding: "0.28rem 0.55rem",
                  fontSize: "0.82rem",
                  background: "var(--color-bg-subtle)",
                  color: "var(--color-fg)",
                  border: "1px solid var(--color-border)",
                  borderRadius: "var(--radius-md)",
                }}
              >
                {envs.map((env) => (
                  <option key={env.name} value={env.name}>
                    {env.name}
                  </option>
                ))}
              </select>
            ))}
        </div>
      )}

      {scope && (
        <div className="grid gap-6 lg:grid-cols-2">
          <SecretsSection scope={scope} />
          <VariablesSection scope={scope} />
        </div>
      )}
    </div>
  );
}

