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
} from "../api.js";
import type { GithubBranch, GithubBranchProtection } from "../types.js";
import { RepoHeader } from "../components/Shell.js";
import { PageTitle, Button, Box } from "../components/ui.js";

interface FormState {
  enabled: boolean;
  requiredStatusChecks: boolean;
  strictStatusChecks: boolean;
  contexts: string;
  requirePullRequestReviews: boolean;
  requiredApprovingReviewCount: number;
  requireCodeOwnerReviews: boolean;
  dismissStaleReviews: boolean;
  enforceAdmins: boolean;
  allowForcePushes: boolean;
  allowDeletions: boolean;
  requiredLinearHistory: boolean;
  requiredConversationResolution: boolean;
  blockCreations: boolean;
  requiredSignatures: boolean;
  restrictPushes: boolean;
  restrictedUsers: string;
  restrictedTeams: string;
}

function protectionToForm(bp: GithubBranchProtection | null): FormState {
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
      enforceAdmins: false,
      allowForcePushes: false,
      allowDeletions: false,
      requiredLinearHistory: false,
      requiredConversationResolution: false,
      blockCreations: false,
      requiredSignatures: false,
      restrictPushes: false,
      restrictedUsers: "",
      restrictedTeams: "",
    };
  }
  return {
    enabled: true,
    requiredStatusChecks: !!bp.required_status_checks,
    strictStatusChecks: bp.required_status_checks?.strict ?? false,
    contexts: bp.required_status_checks?.contexts.join("\n") ?? "",
    requirePullRequestReviews: !!bp.required_pull_request_reviews,
    requiredApprovingReviewCount: bp.required_pull_request_reviews?.required_approving_review_count ?? 1,
    requireCodeOwnerReviews: bp.required_pull_request_reviews?.require_code_owner_reviews ?? false,
    dismissStaleReviews: bp.required_pull_request_reviews?.dismiss_stale_reviews ?? false,
    enforceAdmins: !!bp.enforce_admins?.enabled,
    allowForcePushes: !!bp.allow_force_pushes?.enabled,
    allowDeletions: !!bp.allow_deletions?.enabled,
    requiredLinearHistory: !!bp.required_linear_history?.enabled,
    requiredConversationResolution: !!bp.required_conversation_resolution?.enabled,
    blockCreations: !!bp.block_creations?.enabled,
    requiredSignatures: !!bp.required_signatures?.enabled,
    restrictPushes: !!bp.restrictions,
    restrictedUsers: bp.restrictions?.users.map((user) => user.login).join("\n") ?? "",
    restrictedTeams: bp.restrictions?.teams.map((team) => team.slug).join("\n") ?? "",
  };
}

export function BranchProtectionPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const queryClient = useQueryClient();

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

  useEffect(() => {
    if (repoQuery.data?.default_branch && !branch) {
      setBranch(repoQuery.data.default_branch);
    }
  }, [repoQuery.data?.default_branch, branch]);

  const protectionQuery = useQuery({
    queryKey: ["branch-protection", owner, repo, branch],
    queryFn: () => fetchBranchProtection(owner, repo, branch),
    enabled: !!owner && !!repo && !!branch,
  });

  const [form, setForm] = useState<FormState>(() => protectionToForm(protectionQuery.data ?? null));
  useEffect(() => {
    setForm(protectionToForm(protectionQuery.data ?? null));
  }, [protectionQuery.data]);

  const saveMutation = useMutation({
    mutationFn: async (next: FormState) => {
      const contextList = next.contexts
        .split("\n")
        .map((c) => c.trim())
        .filter(Boolean);
      const restrictedUsers = next.restrictedUsers.split("\n").map((value) => value.trim()).filter(Boolean);
      const restrictedTeams = next.restrictedTeams.split("\n").map((value) => value.trim()).filter(Boolean);
      if (!next.enabled) {
        if (protectionQuery.data) {
          await deleteBranchProtection(owner, repo, branch);
        }
        return null;
      }
      const payload: Partial<GithubBranchProtection> = {
          required_status_checks: next.requiredStatusChecks
            ? {
                strict: next.strictStatusChecks,
                enforcement_level: "non_admins",
                contexts: contextList,
                checks: contextList.map((context) => ({ context, app_id: null })),
              }
            : null,
        required_pull_request_reviews: next.requirePullRequestReviews ? {
            required_approving_review_count: next.requiredApprovingReviewCount,
            require_code_owner_reviews: next.requireCodeOwnerReviews,
            dismiss_stale_reviews: next.dismissStaleReviews,
          } : null,
        restrictions: next.restrictPushes ? { users: [], teams: [], apps: [] } : null,
        enforce_admins: { enabled: next.enforceAdmins },
        allow_force_pushes: { enabled: next.allowForcePushes },
        allow_deletions: { enabled: next.allowDeletions },
        required_linear_history: { enabled: next.requiredLinearHistory },
        required_conversation_resolution: { enabled: next.requiredConversationResolution },
        block_creations: { enabled: next.blockCreations },
        required_signatures: { enabled: next.requiredSignatures },
      };
      const saved = await updateBranchProtection(owner, repo, branch, payload);
      if (next.restrictPushes) {
        await setBranchRestrictionUsers(owner, repo, branch, restrictedUsers);
        await setBranchRestrictionTeams(owner, repo, branch, restrictedTeams);
      }
      return saved;
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["branch-protection", owner, repo, branch] });
      queryClient.invalidateQueries({ queryKey: ["repo-branches", owner, repo] });
      queryClient.invalidateQueries({ queryKey: ["branches", owner, repo] });
    },
  });

  if (repoQuery.isLoading || branchesQuery.isLoading) return <Spinner label={`loading ${owner}/${repo}`} />;
  if (repoQuery.isError || branchesQuery.isError)
    return <InlineError title={`Failed to load ${owner}/${repo}`} detail={String(repoQuery.error ?? branchesQuery.error)} />;

  const branches = branchesQuery.data ?? [];

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

      <Box header={<span style={{ fontWeight: 600 }}>Protected branch</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Branch</span>
            <select
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            >
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

      {protectionQuery.isLoading && <Spinner label={`loading protection for ${branch}`} />}
      {protectionQuery.isError && (
        <div className="mt-4" style={{ color: "var(--color-danger-fg)" }}>
          {protectionQuery.error instanceof Error ? protectionQuery.error.message : String(protectionQuery.error)}
        </div>
      )}

      {!protectionQuery.isLoading && !protectionQuery.isError && (
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
                    ].map(([key, label]) => (
                      <label key={key} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.85rem" }}>
                        <input
                          type="checkbox"
                          checked={form[key as keyof FormState] as boolean}
                          onChange={(e) => setForm((current) => ({ ...current, [key]: e.target.checked }))}
                        />
                        {label}
                      </label>
                    ))}
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
