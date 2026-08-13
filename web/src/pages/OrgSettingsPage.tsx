import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { OrgHeader } from "../components/Shell.js";
import { PageTitle, Box, Button, FormLabel, ErrorBanner, SectionLabel } from "../components/ui.js";
import { fetchOrgProfile, updateOrg } from "../api.js";
import {
  GearIcon,
  PeopleIcon,
  WebhookIcon,
  CommentIcon,
  TeamIcon,
} from "../components/octicons.js";

/**
 * Organization Settings landing. github.com gathers org settings under
 * /orgs/{org}/settings; bleephub previously scattered them across top-level
 * tabs. This page edits the core org profile (PATCH /orgs/{org}) and links to
 * the existing settings surfaces so there is a single Settings entry point.
 */
export function OrgSettingsPage() {
  const { org = "" } = useParams<{ org: string }>();
  const qc = useQueryClient();
  const profile = useQuery({ queryKey: ["org-profile", org], queryFn: () => fetchOrgProfile(org), enabled: !!org });

  const [form, setForm] = useState<{ name: string; description: string; billing_email: string } | null>(null);
  const current = form ?? {
    name: profile.data?.name ?? "",
    description: profile.data?.description ?? "",
    billing_email: profile.data?.email ?? "",
  };
  const set = (k: keyof typeof current, v: string) => setForm({ ...current, [k]: v });

  const saveMut = useMutation({
    mutationFn: () => updateOrg(org, { name: current.name, description: current.description, billing_email: current.billing_email }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-profile", org] }),
  });

  const base = `/ui/orgs/${org}`;
  const sections: { to: string; icon: React.ReactNode; label: string; hint: string }[] = [
    { to: `${base}/governance`, icon: <PeopleIcon size={16} />, label: "Member privileges", hint: "Base permissions, repository creation, and governance" },
    { to: `${base}/rulesets`, icon: <GearIcon size={16} />, label: "Repository rulesets", hint: "Branch and tag protection rules across repositories" },
    { to: `${base}/hooks`, icon: <WebhookIcon size={16} />, label: "Webhooks", hint: "Organization webhooks and deliveries" },
    { to: `${base}/copilot`, icon: <CommentIcon size={16} />, label: "Copilot", hint: "Copilot access and policies" },
    { to: `${base}/people`, icon: <PeopleIcon size={16} />, label: "People", hint: "Members and outside collaborators" },
    { to: `${base}/teams`, icon: <TeamIcon size={16} />, label: "Teams", hint: "Team structure and membership" },
  ];

  return (
    <div>
      <OrgHeader org={org} active="settings" />
      <PageTitle title="Settings" />
      {profile.isError ? (
        <InlineError title="Failed to load organization" detail={String(profile.error)} />
      ) : profile.isLoading ? (
        <Spinner label="loading organization" />
      ) : (
        <div className="flex flex-col gap-5" style={{ maxWidth: "48rem" }}>
          <Box header={<span style={{ fontWeight: 600 }}>Organization profile</span>}>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
              {saveMut.error && <ErrorBanner>{String(saveMut.error)}</ErrorBanner>}
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-name">Display name</FormLabel>
                <input id="org-name" type="text" value={current.name} onChange={(e) => set("name", e.target.value)} className="w-full" />
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-description">Description</FormLabel>
                <textarea id="org-description" value={current.description} rows={3} onChange={(e) => set("description", e.target.value)} className="w-full" />
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-billing-email">Billing email</FormLabel>
                <input id="org-billing-email" type="email" value={current.billing_email} onChange={(e) => set("billing_email", e.target.value)} className="w-full" />
              </div>
              <div className="flex items-center justify-end gap-3">
                {saveMut.isSuccess && <span style={{ fontSize: "0.82rem", color: "var(--gh-open)" }}>Saved.</span>}
                <Button variant="primary" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
                  {saveMut.isPending ? "Saving…" : "Save"}
                </Button>
              </div>
            </div>
          </Box>

          <section>
            <SectionLabel>Access and governance</SectionLabel>
            <Box>
              {sections.map((s, i) => (
                <Link
                  key={s.to}
                  to={s.to}
                  className="flex items-center gap-3"
                  style={{
                    padding: "0.75rem 1rem",
                    borderBottom: i < sections.length - 1 ? "1px solid var(--color-border)" : "none",
                    textDecoration: "none",
                    color: "var(--color-fg)",
                  }}
                >
                  <span style={{ color: "var(--color-fg-muted)" }}>{s.icon}</span>
                  <span className="flex-1">
                    <span style={{ fontWeight: 600, fontSize: "0.9rem", color: "var(--color-accent)" }}>{s.label}</span>
                    <span style={{ display: "block", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{s.hint}</span>
                  </span>
                </Link>
              ))}
            </Box>
          </section>
        </div>
      )}
    </div>
  );
}
