import type { ReactNode } from "react";
import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner } from "@bleephub/ui-core/components";
import { fetchFileContent, isNotFound } from "../api.js";
import type { GithubContentFile } from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { Box, PageTitle, SectionLabel, ButtonLink } from "../components/ui.js";
import { LockIcon, GraphIcon, PlayIcon, EyeIcon } from "../components/octicons.js";

// GitHub's search order for a repository security policy.
const SECURITY_POLICY_PATHS = ["SECURITY.md", ".github/SECURITY.md", "docs/SECURITY.md"];

async function findSecurityPolicy(owner: string, repo: string): Promise<GithubContentFile | null> {
  for (const path of SECURITY_POLICY_PATHS) {
    try {
      return await fetchFileContent(owner, repo, path);
    } catch (err) {
      if (isNotFound(err)) continue;
      throw err;
    }
  }
  return null;
}

export function RepoSecurityOverviewPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const counts = useOpenCounts(owner, repo);
  const base = `/ui/${owner}/${repo}`;

  const policy = useQuery({
    queryKey: ["security-policy", owner, repo],
    queryFn: () => findSecurityPolicy(owner, repo),
    retry: false,
  });

  const features: { icon: ReactNode; title: string; description: string; to: string }[] = [
    { icon: <EyeIcon size={16} />, title: "Security advisories", description: "View and manage repository security advisories.", to: `${base}/security/advisories` },
    { icon: <LockIcon size={16} />, title: "Secret scanning", description: "Detect secrets committed to the repository.", to: `${base}/security/secret-scanning` },
    { icon: <PlayIcon size={16} />, title: "Code scanning", description: "Find vulnerabilities in code with analysis alerts.", to: `${base}/security/code-scanning` },
    { icon: <GraphIcon size={16} />, title: "Dependabot", description: "Alerts for vulnerable dependencies.", to: `${base}/security/dependabot` },
  ];

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="security" {...counts} />
      <PageTitle title="Security" />

      <section aria-label="Security policy" className="mb-6">
        <SectionLabel>Security policy</SectionLabel>
        <Box>
          <div style={{ padding: "1rem", display: "flex", alignItems: "center", justifyContent: "space-between", gap: "0.75rem", flexWrap: "wrap" }}>
            {policy.isLoading ? (
              <Spinner label="checking for a security policy" />
            ) : policy.data ? (
              <>
                <span style={{ fontSize: "0.9rem" }}>
                  This repository has a security policy at <code>{policy.data.path}</code>.
                </span>
                <ButtonLink to={`${base}/blob/HEAD/${policy.data.path}`} variant="secondary" size="sm">View policy</ButtonLink>
              </>
            ) : (
              <>
                <span style={{ fontSize: "0.9rem", color: "var(--color-fg-muted)" }}>
                  No security policy found. Add a <code>SECURITY.md</code> to tell people how to report vulnerabilities.
                </span>
                <ButtonLink to={`${base}/new/HEAD?filename=SECURITY.md`} variant="secondary" size="sm">Set up a security policy</ButtonLink>
              </>
            )}
          </div>
        </Box>
      </section>

      <section aria-label="Security features">
        <SectionLabel>Security features</SectionLabel>
        <div className="grid gap-3 sm:grid-cols-2">
          {features.map((f) => (
            <Box key={f.title}>
              <div style={{ padding: "0.9rem 1rem" }}>
                <Link to={f.to} style={{ display: "inline-flex", alignItems: "center", gap: "0.4rem", color: "var(--color-accent)", fontWeight: 600, textDecoration: "none", fontSize: "0.95rem" }}>
                  <span style={{ color: "var(--color-fg-muted)" }}>{f.icon}</span>
                  {f.title}
                </Link>
                <p style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0" }}>{f.description}</p>
              </div>
            </Box>
          ))}
        </div>
      </section>
    </div>
  );
}
