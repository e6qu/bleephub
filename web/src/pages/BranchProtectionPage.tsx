import { useEffect, useState } from "react";
import { useParams, Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchRepoDetail,
  fetchRepoBranches,
  fetchBranchProtection,
  updateBranchProtection,
  deleteBranchProtection,
  setBranchRestrictionTeams,
  setBranchRestrictionUsers,
  ghFetch,
  ghSend,
} from "../api.js";
import type {
  GithubBranch,
  GithubBranchProtection,
  GithubBranchProtectionReviewDismissalRestrictions,
  GithubBranchProtectionRestrictions,
} from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { RepoNotFound } from "../components/RepoNotFound.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { PageTitle, Button, Box, ErrorBanner } from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";

interface FormState {
  enabled: boolean;
  requiredStatusChecks: boolean;
  strictStatusChecks: boolean;
  contexts: string;
  requirePullRequestReviews: boolean;
  requiredApprovingReviewCount: number;
  requireCodeOwnerReviews: boolean;
  dismissStaleReviews: boolean;
  requireLastPushApproval: boolean;
  restrictDismissals: boolean;
  dismissalUsers: string;
  dismissalTeams: string;
  enforceAdmins: boolean;
  allowForcePushes: boolean;
  allowDeletions: boolean;
  requiredLinearHistory: boolean;
  requiredConversationResolution: boolean;
  blockCreations: boolean;
  requiredSignatures: boolean;
  lockBranch: boolean;
  allowForkSyncing: boolean;
  restrictPushes: boolean;
  restrictedUsers: string;
  restrictedTeams: string;
}

// A web-only wildcard rule from /ui-data branch-protection-patterns. The
// protection member serializes like the REST GET protection shape but with
// omitempty members, hence Partial.
interface BranchProtectionPatternRule {
  pattern: string;
  protection: Partial<GithubBranchProtection>;
}

const patternsPath = (owner: string, repo: string) =>
  `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branch-protection-patterns`;

// Wildcard names are pattern rules (git forbids * and ? in branch names, so
// an exact branch can never be mistaken for a pattern).
const isWildcardPattern = (name: string) => /[*?]/.test(name);

// The stored dismissal-restriction team entries serialize with the slug in
// `login` (server BPActor shape) while the published type says `slug` — read both.
function teamSlug(team: unknown): string {
  const t = team as { slug?: string; login?: string };
  return t.slug ?? t.login ?? "";
}

function protectionToForm(bp: Partial<GithubBranchProtection> | null): FormState {
  if (!bp) {
    return {
      enabled: false,
      requiredStatusChecks: false,
      strictStatusChecks: false,
      contexts: "",
      requirePullRequestReviews: false,
      requiredApprovingReviewCount: 1,
      requireCodeOwnerReviews: false,
      dismissStaleReviews: false,
      requireLastPushApproval: false,
      restrictDismissals: false,
      dismissalUsers: "",
      dismissalTeams: "",
      enforceAdmins: false,
      allowForcePushes: false,
      allowDeletions: false,
      requiredLinearHistory: false,
      requiredConversationResolution: false,
      blockCreations: false,
      requiredSignatures: false,
      lockBranch: false,
      allowForkSyncing: false,
      restrictPushes: false,
      restrictedUsers: "",
      restrictedTeams: "",
    };
  }
  const dismissal = bp.required_pull_request_reviews?.dismissal_restrictions;
  return {
    enabled: true,
    requiredStatusChecks: !!bp.required_status_checks,
    strictStatusChecks: bp.required_status_checks?.strict ?? false,
    contexts: bp.required_status_checks?.contexts?.join("\n") ?? "",
    requirePullRequestReviews: !!bp.required_pull_request_reviews,
    requiredApprovingReviewCount: bp.required_pull_request_reviews?.required_approving_review_count ?? 1,
    requireCodeOwnerReviews: bp.required_pull_request_reviews?.require_code_owner_reviews ?? false,
    dismissStaleReviews: bp.required_pull_request_reviews?.dismiss_stale_reviews ?? false,
    requireLastPushApproval: bp.required_pull_request_reviews?.require_last_push_approval ?? false,
    restrictDismissals: !!dismissal,
    dismissalUsers: dismissal?.users?.map((user) => user.login).join("\n") ?? "",
    dismissalTeams: dismissal?.teams?.map(teamSlug).filter(Boolean).join("\n") ?? "",
    enforceAdmins: !!bp.enforce_admins?.enabled,
    allowForcePushes: !!bp.allow_force_pushes?.enabled,
    allowDeletions: !!bp.allow_deletions?.enabled,
    requiredLinearHistory: !!bp.required_linear_history?.enabled,
    requiredConversationResolution: !!bp.required_conversation_resolution?.enabled,
    blockCreations: !!bp.block_creations?.enabled,
    requiredSignatures: !!bp.required_signatures?.enabled,
    lockBranch: !!bp.lock_branch?.enabled,
    allowForkSyncing: !!bp.allow_fork_syncing?.enabled,
    restrictPushes: !!bp.restrictions,
    restrictedUsers: bp.restrictions?.users?.map((user) => user.login).join("\n") ?? "",
    restrictedTeams: bp.restrictions?.teams?.map(teamSlug).filter(Boolean).join("\n") ?? "",
  };
}

function splitLines(value: string): string[] {
  return value
    .split("\n")
    .map((v) => v.trim())
    .filter(Boolean);
}

// Builds the PUT protection body shared by the exact-branch REST endpoint and
// the /ui-data pattern endpoint (the pattern endpoint accepts the same
// members). Exact rules manage push-restriction actors through the dedicated
// restrictions sub-endpoints; pattern rules have no sub-endpoints, so their
// actors travel inline (the server stores actors as {login} — team slugs in
// `login`, store.BPActor has no slug field).
function formToProtectionPayload(next: FormState, inlineRestrictionActors: boolean): Partial<GithubBranchProtection> {
  const contextList = splitLines(next.contexts);
  const dismissalUsers = splitLines(next.dismissalUsers);
  const dismissalTeams = splitLines(next.dismissalTeams);
  const restrictedUsers = splitLines(next.restrictedUsers);
  const restrictedTeams = splitLines(next.restrictedTeams);
  return {
    required_status_checks: next.requiredStatusChecks
      ? {
          strict: next.strictStatusChecks,
          enforcement_level: "non_admins",
          contexts: contextList,
          checks: contextList.map((context) => ({ context, app_id: null })),
        }
      : null,
    required_pull_request_reviews: next.requirePullRequestReviews
      ? {
          required_approving_review_count: next.requiredApprovingReviewCount,
          require_code_owner_reviews: next.requireCodeOwnerReviews,
          dismiss_stale_reviews: next.dismissStaleReviews,
          require_last_push_approval: next.requireLastPushApproval,
          ...(next.restrictDismissals
            ? {
                dismissal_restrictions: {
                  users: dismissalUsers.map((login) => ({ login })),
                  teams: dismissalTeams.map((slug) => ({ login: slug })),
                } as unknown as GithubBranchProtectionReviewDismissalRestrictions,
              }
            : {}),
        }
      : null,
    restrictions: next.restrictPushes
      ? inlineRestrictionActors
        ? ({
            users: restrictedUsers.map((login) => ({ login })),
            teams: restrictedTeams.map((slug) => ({ login: slug })),
            apps: [],
          } as unknown as GithubBranchProtectionRestrictions)
        : { users: [], teams: [], apps: [] }
      : null,
    enforce_admins: { enabled: next.enforceAdmins },
    allow_force_pushes: { enabled: next.allowForcePushes },
    allow_deletions: { enabled: next.allowDeletions },
    required_linear_history: { enabled: next.requiredLinearHistory },
    required_conversation_resolution: { enabled: next.requiredConversationResolution },
    block_creations: { enabled: next.blockCreations },
    required_signatures: { enabled: next.requiredSignatures },
    lock_branch: { enabled: next.lockBranch },
    allow_fork_syncing: { enabled: next.allowForkSyncing },
  };
}

// Cap the per-branch protection fan-out when assembling the rules list: the
// REST surface is per-branch (there is no "list all rules" endpoint), so we
// probe at most this many protected branches.
const RULES_LIST_CAP = 20;

function summarizeRule(bp: Partial<GithubBranchProtection>): string {
  const parts: string[] = [];
  if (bp.required_status_checks) parts.push("status checks");
  if (bp.required_pull_request_reviews) parts.push(`${bp.required_pull_request_reviews.required_approving_review_count} review(s)`);
  if (bp.enforce_admins?.enabled) parts.push("admins included");
  if (bp.required_signatures?.enabled) parts.push("signed commits");
  if (bp.required_linear_history?.enabled) parts.push("linear history");
  if (bp.lock_branch?.enabled) parts.push("locked");
  if (bp.restrictions) parts.push("push restrictions");
  return parts.length ? parts.join(" · ") : "protected";
}

export function BranchProtectionPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const queryClient = useQueryClient();
  // Branch protection editing is admin-only: github.com 404s this URL for
  // non-admin viewers. The guard renders after the repo query settles (the
  // hook reads the same payload), so admins never see a 404 flash.
  const { isAdmin } = useRepoPermissions(owner, repo);

  const repoQuery = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: () => fetchRepoDetail(owner, repo),
    enabled: !!owner && !!repo,
  });

  const branchesQuery = useQuery({
    queryKey: ["repo-branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo,
  });

  const [branch, setBranch] = useState(repoQuery.data?.default_branch ?? "");
  const [newRuleName, setNewRuleName] = useState("");

  useEffect(() => {
    if (repoQuery.data?.default_branch && !branch) {
      setBranch(repoQuery.data.default_branch);
    }
  }, [repoQuery.data?.default_branch, branch]);

  // A wildcard target edits a /ui-data pattern rule; the REST protection
  // endpoints are exact-name only (never GET them for a wildcard — 404).
  const isPatternTarget = isWildcardPattern(branch);

  const protectionQuery = useQuery({
    queryKey: ["branch-protection", owner, repo, branch],
    queryFn: () => fetchBranchProtection(owner, repo, branch),
    enabled: !!owner && !!repo && !!branch && !isPatternTarget,
  });

  const patternsQuery = useQuery({
    queryKey: ["branch-protection-patterns", owner, repo],
    // A payload that is not the expected rule array must surface as a query
    // ERROR (banner + empty list), never crash the render mid-.map.
    queryFn: async () => {
      const raw = await ghFetch<unknown>(patternsPath(owner, repo));
      if (!Array.isArray(raw)) throw new Error("malformed branch-protection-patterns payload");
      return raw as BranchProtectionPatternRule[];
    },
    enabled: !!owner && !!repo,
  });

  // Assemble the current rules by probing protection for each protected
  // branch (capped — the REST surface has no list-all-rules endpoint).
  const protectedNames = (branchesQuery.data ?? [])
    .filter((b) => b.protected)
    .map((b) => b.name)
    .slice(0, RULES_LIST_CAP);
  const rulesQuery = useQuery({
    queryKey: ["branch-protection-rules", owner, repo, protectedNames.join(" ")],
    queryFn: async () => {
      const entries = await Promise.all(
        protectedNames.map(async (name) => {
          try {
            return { branch: name, bp: await fetchBranchProtection(owner, repo, name) };
          } catch {
            return { branch: name, bp: null };
          }
        }),
      );
      return entries.filter((e): e is { branch: string; bp: GithubBranchProtection } => e.bp !== null);
    },
    enabled: protectedNames.length > 0,
  });

  const invalidateAll = () => {
    queryClient.invalidateQueries({ queryKey: ["branch-protection", owner, repo] });
    queryClient.invalidateQueries({ queryKey: ["branch-protection-rules", owner, repo] });
    queryClient.invalidateQueries({ queryKey: ["branch-protection-patterns", owner, repo] });
    queryClient.invalidateQueries({ queryKey: ["repo-branches", owner, repo] });
    queryClient.invalidateQueries({ queryKey: ["branches", owner, repo] });
  };

  const deleteRuleMutation = useMutation({
    mutationFn: (name: string) => deleteBranchProtection(owner, repo, name),
    onSuccess: invalidateAll,
  });

  // Removing one pattern rule = PUT the remaining ordered list (the endpoint
  // replaces the whole list); DELETE clears the list when none remain.
  const deletePatternRuleMutation = useMutation({
    mutationFn: async (pattern: string) => {
      const path = patternsPath(owner, repo);
      const current = await ghFetch<BranchProtectionPatternRule[]>(path);
      const remaining = current.filter((r) => r.pattern !== pattern);
      if (remaining.length === 0) {
        await ghSend("DELETE", path);
      } else {
        await ghSend("PUT", path, remaining);
      }
    },
    onSuccess: invalidateAll,
  });

  const patternRules = patternsQuery.data ?? [];
  const activePatternRule = isPatternTarget ? patternRules.find((r) => r.pattern === branch) ?? null : null;

  const [form, setForm] = useState<FormState>(() => protectionToForm(null));
  useEffect(() => {
    setForm(protectionToForm(isPatternTarget ? activePatternRule?.protection ?? null : protectionQuery.data ?? null));
  }, [protectionQuery.data, isPatternTarget, activePatternRule]);

  const saveMutation = useMutation({
    mutationFn: async (next: FormState) => {
      if (isPatternTarget) {
        const path = patternsPath(owner, repo);
        const current = await ghFetch<BranchProtectionPatternRule[]>(path);
        if (!next.enabled) {
          const remaining = current.filter((r) => r.pattern !== branch);
          if (remaining.length !== current.length) {
            if (remaining.length === 0) {
              await ghSend("DELETE", path);
            } else {
              await ghSend("PUT", path, remaining);
            }
          }
          return null;
        }
        const entry: BranchProtectionPatternRule = { pattern: branch, protection: formToProtectionPayload(next, true) };
        const index = current.findIndex((r) => r.pattern === branch);
        const nextList = index >= 0 ? current.map((r, i) => (i === index ? entry : r)) : [...current, entry];
        await ghSend("PUT", path, nextList);
        return null;
      }
      if (!next.enabled) {
        if (protectionQuery.data) {
          await deleteBranchProtection(owner, repo, branch);
        }
        return null;
      }
      const saved = await updateBranchProtection(owner, repo, branch, formToProtectionPayload(next, false));
      if (next.restrictPushes) {
        await setBranchRestrictionUsers(owner, repo, branch, splitLines(next.restrictedUsers));
        await setBranchRestrictionTeams(owner, repo, branch, splitLines(next.restrictedTeams));
      }
      return saved;
    },
    onSuccess: invalidateAll,
  });

  if (repoQuery.isLoading || branchesQuery.isLoading) return <Spinner label={`loading ${owner}/${repo}`} />;
  if (repoQuery.isError || branchesQuery.isError)
    return <InlineError title={`Failed to load ${owner}/${repo}`} detail={String(repoQuery.error ?? branchesQuery.error)} />;
  if (!isAdmin) return <RepoNotFound />;

  const branches = branchesQuery.data ?? [];
  const branchInList = branches.some((b: GithubBranch) => b.name === branch);
  const rules = rulesQuery.data ?? [];
  const protectedCount = (branchesQuery.data ?? []).filter((b) => b.protected).length;

  const editorLoading = isPatternTarget ? patternsQuery.isLoading : protectionQuery.isLoading;
  const editorError = isPatternTarget ? patternsQuery.error : protectionQuery.error;
  const editorIsError = isPatternTarget ? patternsQuery.isError : protectionQuery.isError;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="settings" />
      <PageTitle
        title="Branch protection"
        meta={
          <Link
            to={`/ui/repos/${owner}/${repo}/settings`}
            style={{ color: "var(--color-accent)", textDecoration: "none" }}
          >
            ← Back to settings
          </Link>
        }
      />

      <Box header={<span style={{ fontWeight: 600 }}>Branch protection rules</span>} className="mb-4">
        {rulesQuery.isLoading || patternsQuery.isLoading ? (
          <div style={{ padding: "1rem" }}>
            <Spinner label="loading protection rules" />
          </div>
        ) : rules.length === 0 && patternRules.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No protected branches yet. Pick a branch below, or add a rule by name.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {rules.map((rule) => (
              <li
                key={rule.branch}
                className="flex items-center justify-between gap-3"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div className="min-w-0">
                  <span className="font-mono" style={{ fontWeight: 600, fontSize: "0.88rem" }}>{rule.branch}</span>
                  <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    {summarizeRule(rule.bp)}
                  </span>
                </div>
                <div className="flex gap-2" style={{ flexShrink: 0 }}>
                  <Button size="sm" variant="secondary" aria-label={`Edit rule for ${rule.branch}`} onClick={() => setBranch(rule.branch)}>
                    Edit
                  </Button>
                  <Button
                    size="sm"
                    variant="danger"
                    aria-label={`Delete rule for ${rule.branch}`}
                    disabled={deleteRuleMutation.isPending}
                    onClick={async () => {
                      if (
                        await confirmAction(`Delete the protection rule for "${rule.branch}"?`, {
                          title: "Delete protection rule",
                          confirmLabel: "Delete",
                        })
                      ) {
                        deleteRuleMutation.mutate(rule.branch);
                      }
                    }}
                  >
                    Delete
                  </Button>
                </div>
              </li>
            ))}
            {patternRules.map((rule) => (
              <li
                key={`pattern:${rule.pattern}`}
                className="flex items-center justify-between gap-3"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div className="min-w-0">
                  <span className="font-mono" style={{ fontWeight: 600, fontSize: "0.88rem" }}>{rule.pattern}</span>
                  <span
                    style={{
                      marginLeft: "0.5rem",
                      fontSize: "0.68rem",
                      fontWeight: 600,
                      padding: "0.05rem 0.45rem",
                      borderRadius: "999px",
                      border: "1px solid var(--color-border)",
                      color: "var(--color-fg-muted)",
                      verticalAlign: "middle",
                    }}
                  >
                    Pattern
                  </span>
                  <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    {summarizeRule(rule.protection)}
                  </span>
                </div>
                <div className="flex gap-2" style={{ flexShrink: 0 }}>
                  <Button size="sm" variant="secondary" aria-label={`Edit rule for ${rule.pattern}`} onClick={() => setBranch(rule.pattern)}>
                    Edit
                  </Button>
                  <Button
                    size="sm"
                    variant="danger"
                    aria-label={`Delete rule for ${rule.pattern}`}
                    disabled={deletePatternRuleMutation.isPending}
                    onClick={async () => {
                      if (
                        await confirmAction(`Delete the pattern rule "${rule.pattern}"?`, {
                          title: "Delete protection rule",
                          confirmLabel: "Delete",
                        })
                      ) {
                        deletePatternRuleMutation.mutate(rule.pattern);
                      }
                    }}
                  >
                    Delete
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
        {deleteRuleMutation.isError && (
          <div style={{ padding: "0.5rem 1rem" }}>
            <ErrorBanner>{String(deleteRuleMutation.error)}</ErrorBanner>
          </div>
        )}
        {deletePatternRuleMutation.isError && (
          <div style={{ padding: "0.5rem 1rem" }}>
            <ErrorBanner>{String(deletePatternRuleMutation.error)}</ErrorBanner>
          </div>
        )}
        {patternsQuery.isError && (
          <div style={{ padding: "0.5rem 1rem" }}>
            <ErrorBanner>{String(patternsQuery.error)}</ErrorBanner>
          </div>
        )}
        {protectedCount > RULES_LIST_CAP && (
          <div style={{ padding: "0.5rem 1rem", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
            Showing the first {RULES_LIST_CAP} of {protectedCount} protected branches — the API exposes
            protection per branch, so this list is assembled with a capped fan-out.
          </div>
        )}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            const name = newRuleName.trim();
            if (!name) return;
            setBranch(name);
            setNewRuleName("");
          }}
          style={{ display: "flex", gap: "0.5rem", alignItems: "flex-end", flexWrap: "wrap", padding: "0.75rem 1rem", borderTop: "1px solid var(--color-border)" }}
        >
          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem", flex: 1, minWidth: "14rem" }}>
            <span style={{ fontSize: "0.8rem", fontWeight: 500 }}>Branch name for a new rule</span>
            <input
              type="text"
              value={newRuleName}
              onChange={(e) => setNewRuleName(e.target.value)}
              placeholder="e.g. release/1.x or release/*"
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            />
          </label>
          <Button type="submit" variant="secondary" size="sm" disabled={!newRuleName.trim()}>
            Add rule
          </Button>
          <p style={{ flexBasis: "100%", margin: 0, fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
            Names with * or ? create a pattern rule: * matches within a single path segment (release/*
            does not cross /) while ** crosses segments, and exact-name rules take precedence over
            pattern rules. Exact rules for branches that do not exist yet will not appear in the list
            above until the branch is created.
          </p>
        </form>
      </Box>

      <Box header={<span style={{ fontWeight: 600 }}>Protected branch</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Branch</span>
            <select
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            >
              {!branchInList && branch && (
                <option value={branch}>{branch}{isPatternTarget ? " (pattern)" : " (new rule)"}</option>
              )}
              {branches.map((b: GithubBranch) => (
                <option key={b.name} value={b.name}>
                  {b.name}
                  {b.protected ? " (protected)" : ""}
                </option>
              ))}
            </select>
          </label>
        </div>
      </Box>

      {editorLoading && <Spinner label={`loading protection for ${branch}`} />}
      {editorIsError && (
        <div className="mt-4" style={{ color: "var(--color-danger-fg)" }}>
          {editorError instanceof Error ? editorError.message : String(editorError)}
        </div>
      )}

      {!editorLoading && !editorIsError && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            saveMutation.mutate(form);
          }}
          className="mt-4"
        >
          <Box header={<span style={{ fontWeight: 600 }}>Protection rules</span>}>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1.25rem" }}>
              <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem" }}>
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
                />
                Protect this branch
              </label>

              {form.enabled && (
                <>
                  <fieldset style={{ border: "none", padding: 0, margin: 0, display: "flex", flexDirection: "column", gap: "0.75rem" }}>
                    <legend style={{ fontSize: "0.85rem", fontWeight: 500 }}>Require status checks</legend>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.requiredStatusChecks}
                        onChange={(e) => setForm((f) => ({ ...f, requiredStatusChecks: e.target.checked }))}
                      />
                      Require status checks before merging
                    </label>
                    {form.requiredStatusChecks && (
                      <>
                        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                          <input
                            type="checkbox"
                            checked={form.strictStatusChecks}
                            onChange={(e) => setForm((f) => ({ ...f, strictStatusChecks: e.target.checked }))}
                          />
                          Require branches to be up to date before merging
                        </label>
                        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                          <span style={{ fontSize: "0.8rem" }}>Status checks (one per line)</span>
                          <textarea
                            value={form.contexts}
                            onChange={(e) => setForm((f) => ({ ...f, contexts: e.target.value }))}
                            rows={4}
                            placeholder="ci/build&#10;ci/test"
                            style={{ fontSize: "0.85rem", padding: "0.4rem 0.5rem" }}
                          />
                        </label>
                      </>
                    )}
                  </fieldset>

                  <fieldset style={{ border: "none", padding: 0, margin: 0, display: "flex", flexDirection: "column", gap: "0.75rem" }}>
                    <legend style={{ fontSize: "0.85rem", fontWeight: 500 }}>Pull request reviews</legend>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.requirePullRequestReviews}
                        onChange={(e) => setForm((f) => ({ ...f, requirePullRequestReviews: e.target.checked }))}
                      />
                      Require a pull request before merging
                    </label>
                    {form.requirePullRequestReviews && (
                      <>
                        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                          <span style={{ fontSize: "0.8rem" }}>Required approving reviews</span>
                          <input
                            type="number"
                            min={0}
                            max={6}
                            value={form.requiredApprovingReviewCount}
                            onChange={(e) =>
                              setForm((f) => ({ ...f, requiredApprovingReviewCount: parseInt(e.target.value, 10) || 0 }))
                            }
                            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem", maxWidth: "6rem" }}
                          />
                        </label>
                        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                          <input
                            type="checkbox"
                            checked={form.dismissStaleReviews}
                            onChange={(e) => setForm((f) => ({ ...f, dismissStaleReviews: e.target.checked }))}
                          />
                          Dismiss stale reviews when new commits are pushed
                        </label>
                        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                          <input
                            type="checkbox"
                            checked={form.requireCodeOwnerReviews}
                            onChange={(e) => setForm((f) => ({ ...f, requireCodeOwnerReviews: e.target.checked }))}
                          />
                          Require review from code owners
                        </label>
                        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                          <input
                            type="checkbox"
                            checked={form.requireLastPushApproval}
                            onChange={(e) => setForm((f) => ({ ...f, requireLastPushApproval: e.target.checked }))}
                          />
                          Require approval of the most recent reviewable push
                        </label>
                        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                          <input
                            type="checkbox"
                            checked={form.restrictDismissals}
                            onChange={(e) => setForm((f) => ({ ...f, restrictDismissals: e.target.checked }))}
                          />
                          Restrict who can dismiss pull request reviews
                        </label>
                        {form.restrictDismissals && (
                          <div className="grid gap-3 md:grid-cols-2">
                            <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                              <span style={{ fontSize: "0.8rem" }}>Users who can dismiss (one login per line)</span>
                              <textarea
                                value={form.dismissalUsers}
                                onChange={(e) => setForm((f) => ({ ...f, dismissalUsers: e.target.value }))}
                                rows={3}
                              />
                            </label>
                            <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                              <span style={{ fontSize: "0.8rem" }}>Teams who can dismiss (one slug per line)</span>
                              <textarea
                                value={form.dismissalTeams}
                                onChange={(e) => setForm((f) => ({ ...f, dismissalTeams: e.target.value }))}
                                rows={3}
                              />
                            </label>
                          </div>
                        )}
                      </>
                    )}
                  </fieldset>

                  <fieldset style={{ border: "none", padding: 0, margin: 0, display: "flex", flexDirection: "column", gap: "0.75rem" }}>
                    <legend style={{ fontSize: "0.85rem", fontWeight: 500 }}>Restrict pushes</legend>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.restrictPushes}
                        onChange={(e) => setForm((f) => ({ ...f, restrictPushes: e.target.checked }))}
                      />
                      Restrict who can push to this branch
                    </label>
                    {form.restrictPushes && (
                      <div className="grid gap-3 md:grid-cols-2">
                        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                          <span style={{ fontSize: "0.8rem" }}>Users (one login per line)</span>
                          <textarea
                            value={form.restrictedUsers}
                            onChange={(e) => setForm((f) => ({ ...f, restrictedUsers: e.target.value }))}
                            rows={3}
                          />
                        </label>
                        <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                          <span style={{ fontSize: "0.8rem" }}>Teams (one slug per line)</span>
                          <textarea
                            value={form.restrictedTeams}
                            onChange={(e) => setForm((f) => ({ ...f, restrictedTeams: e.target.value }))}
                            rows={3}
                          />
                        </label>
                      </div>
                    )}
                  </fieldset>

                  <fieldset style={{ border: "none", padding: 0, margin: 0, display: "flex", flexDirection: "column", gap: "0.5rem" }}>
                    <legend style={{ fontSize: "0.85rem", fontWeight: 500 }}>Miscellaneous</legend>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.enforceAdmins}
                        onChange={(e) => setForm((f) => ({ ...f, enforceAdmins: e.target.checked }))}
                      />
                      Include administrators
                    </label>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.allowForcePushes}
                        onChange={(e) => setForm((f) => ({ ...f, allowForcePushes: e.target.checked }))}
                      />
                      Allow force pushes
                    </label>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                      <input
                        type="checkbox"
                        checked={form.allowDeletions}
                        onChange={(e) => setForm((f) => ({ ...f, allowDeletions: e.target.checked }))}
                      />
                      Allow deletions
                    </label>
                    {[
                      ["requiredLinearHistory", "Require linear history"],
                      ["requiredConversationResolution", "Require conversation resolution before merging"],
                      ["blockCreations", "Block branch creation"],
                      ["requiredSignatures", "Require signed commits"],
                      ["lockBranch", "Lock branch"],
                      ["allowForkSyncing", "Allow fork syncing"],
                    ].map(([key, label]) => (
                      <label key={key} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                        <input
                          type="checkbox"
                          checked={form[key as keyof FormState] as boolean}
                          onChange={(e) => setForm((current) => ({ ...current, [key as string]: e.target.checked }))}
                        />
                        {label}
                      </label>
                    ))}
                    <p style={{ margin: "0.25rem 0 0", fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
                      Not supported by this server&apos;s branch-protection API (and therefore not shown):
                      required deployment environments.
                    </p>
                  </fieldset>
                </>
              )}

              <div className="flex justify-end" style={{ marginTop: "0.5rem" }}>
                <Button type="submit" variant="primary" disabled={saveMutation.isPending}>
                  {saveMutation.isPending ? "Saving…" : "Save changes"}
                </Button>
              </div>
            </div>
          </Box>

          {saveMutation.isError && (
            <div className="mt-4" style={{ color: "var(--color-danger-fg)" }}>
              {saveMutation.error instanceof Error ? saveMutation.error.message : String(saveMutation.error)}
            </div>
          )}
          {saveMutation.isSuccess && (
            <div className="mt-4" style={{ color: "var(--gh-open)" }}>Protection rules saved.</div>
          )}
        </form>
      )}
    </div>
  );
}
