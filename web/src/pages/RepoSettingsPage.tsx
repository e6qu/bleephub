import { useEffect, useState, type CSSProperties, type FormEvent } from "react";
import { useParams, useNavigate, Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";
import { RulesetEditor, type RulesetRuleConfig } from "../components/RulesetEditor.js";
import {
  ghFetch,
  ghPostJSON,
  ghSend,
  isNotFound,
  fetchOrgCustomProperties,
  fetchActionsPermissions,
  updateActionsPermissions,
  fetchWorkflowPermissions,
  updateWorkflowPermissions,
  fetchRepoRulesets,
  createRepoRuleset,
  deleteRepoRuleset,
  fetchEnvironments,
  fetchEnvironmentsDetail,
  createEnvironment,
  putEnvironment,
  deleteEnvironment,
  fetchEnvVariables,
  createEnvVariable,
  deleteEnvVariable,
  fetchEnvSecrets,
  deleteEnvSecret,
  addRepoDeployKey,
  createRepoAutolink,
  fetchWebhooks,
  updateRepoHook,
  deleteRepoHook,
  pingRepoHook,
  deleteRepoAutolink,
  fetchRepoAutolinks,
  createPagesSite,
  deletePagesSite,
  cancelRepoInvitation,
  deleteRepo,
  deleteRepoDeployKey,
  fetchPagesBuilds,
  fetchPagesDeploymentStatus,
  fetchPagesHealth,
  fetchPagesSite,
  fetchRepoAutomatedSecurityFixes,
  fetchRepoBranches,
  fetchRepoCollaborators,
  fetchRepoDeployKeys,
  fetchRepoDetail,
  fetchRepoInteractionLimit,
  fetchRepoPrivateVulnerabilityReporting,
  fetchRepoVulnerabilityAlerts,
  fetchRepoInvitations,
  fetchRepoTopics,
  fetchOrgTeams,
  inviteRepoCollaborator,
  removeRepoCollaborator,
  renameBranch,
  requestPagesBuild,
  setRepoFlag,
  setRepoInteractionLimit,
  transferRepo,
  updatePagesSite,
  updateRepo,
  updateRepoTopics,
} from "../api.js";
import type {
  GithubCustomProperty,
  GithubRuleset,
  GithubRulesetTarget,
  GithubRulesetEnforcement,
  GithubEnvironment,
  GithubActionsVariable,
  GithubActionsPermissions,
  GithubWorkflowPermissions,
  GithubSecret,
  BleephubRepo,
  GithubAutolink,
  GithubWebhook,
  GithubCollaborator,
  GithubDeployKey,
  GithubPagesBuild,
  GithubPagesSite,
  GithubRepoInvitation,
} from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { RepoNotFound } from "../components/RepoNotFound.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { SettingsLayout, type SettingsNavSection } from "../components/SettingsLayout.js";
import { PageTitle, Button, ButtonLink, Box, FormLabel, ErrorBanner, Modal, DialogActions } from "../components/ui.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { WebhookForm, type WebhookFormValues } from "../components/WebhookForm.js";

// Repo PATCH fields the server accepts (internal/server/gh_repos_rest.go) that
// predate the BleephubRepo shape. Kept page-local so types.ts and api.ts stay
// untouched; updateRepo's Partial<BleephubRepo> payload is widened via RepoPatch.
interface SecurityAnalysisStatus { status: "enabled" | "disabled" }
interface RepoSettingsExtras {
  has_discussions?: boolean;
  security_and_analysis?: {
    advanced_security?: SecurityAnalysisStatus;
    secret_scanning?: SecurityAnalysisStatus;
    secret_scanning_push_protection?: SecurityAnalysisStatus;
    secret_scanning_non_provider_patterns?: SecurityAnalysisStatus;
    /** Read-only in the PATCH payload — toggled via PUT/DELETE /automated-security-fixes. */
    dependabot_security_updates?: SecurityAnalysisStatus;
  };
}
type RepoPatch = Partial<BleephubRepo> & RepoSettingsExtras & { name?: string };
const patchRepo = (owner: string, repo: string, payload: RepoPatch) =>
  updateRepo(owner, repo, payload as Partial<BleephubRepo>);

type SettingsTab = "general" | "collaborators" | "deploy-keys" | "pages" | "security" | "interaction" | "transfer" | "rename" | "autolinks" | "webhooks" | "actions" | "environments" | "rulesets" | "custom-properties" | "branches" | "secrets" | "tags";

const SETTINGS_NAV: SettingsNavSection<SettingsTab>[] = [
  { items: [{ key: "general", label: "General" }] },
  { title: "Access", items: [{ key: "collaborators", label: "Collaborators" }] },
  {
    title: "Code and automation",
    items: [
      { key: "branches", label: "Branches" },
      { key: "tags", label: "Tags" },
      { key: "actions", label: "Actions" },
      { key: "rulesets", label: "Rulesets" },
      { key: "environments", label: "Environments" },
      { key: "pages", label: "Pages" },
      { key: "webhooks", label: "Webhooks" },
      { key: "rename", label: "Rename branch" },
      { key: "autolinks", label: "Autolinks" },
      { key: "custom-properties", label: "Custom properties" },
    ],
  },
  {
    title: "Security",
    items: [
      { key: "deploy-keys", label: "Deploy keys" },
      { key: "secrets", label: "Secrets and variables" },
      { key: "security", label: "Code security and analysis" },
      { key: "interaction", label: "Interaction limits" },
    ],
  },
  { title: "Danger zone", items: [{ key: "transfer", label: "Transfer" }] },
];

export function RepoSettingsPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const [tab, setTab] = useState<SettingsTab>("general");

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: () => fetchRepoDetail(owner, repo),
    enabled: !!owner && !!repo,
  });
  // Settings is admin-only: github.com answers this URL with a 404 for
  // non-admin viewers rather than explaining what it is. The repo query
  // above is the same payload the hook reads, so once it has resolved the
  // decision is final — admins never see a 404 flash.
  const { isAdmin } = useRepoPermissions(owner, repo);

  if (isLoading) return <Spinner label={`loading ${owner}/${repo}`} />;
  if (isError || !data)
    return <InlineError title={`Failed to load ${owner}/${repo}`} detail={String(error)} />;
  if (!isAdmin) return <RepoNotFound />;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="settings" />
      <PageTitle title="Settings" />
      <SettingsLayout sections={SETTINGS_NAV} active={tab} onSelect={setTab}>
        {tab === "general" && <GeneralSettingsTab owner={owner} repo={repo} repoData={data} />}
        {tab === "collaborators" && <CollaboratorsTab owner={owner} repo={repo} />}
        {tab === "deploy-keys" && <DeployKeysTab owner={owner} repo={repo} />}
        {tab === "pages" && <PagesTab owner={owner} repo={repo} />}
        {tab === "security" && <SecurityTab owner={owner} repo={repo} repoData={data} />}
        {tab === "interaction" && <InteractionTab owner={owner} repo={repo} />}
        {tab === "autolinks" && <AutolinksTab owner={owner} repo={repo} />}
        {tab === "webhooks" && <WebhooksTab owner={owner} repo={repo} />}
        {tab === "actions" && <ActionsSettingsTab owner={owner} repo={repo} />}
        {tab === "rulesets" && <RepoRulesetsTab owner={owner} repo={repo} />}
        {tab === "custom-properties" && <CustomPropertiesTab owner={owner} repo={repo} />}
        {tab === "environments" && <EnvironmentsTab owner={owner} repo={repo} />}
        {tab === "branches" && <BranchesSettingsTab owner={owner} repo={repo} />}
        {tab === "secrets" && <SecretsAndVariablesCard owner={owner} repo={repo} />}
        {tab === "tags" && <TagsSettingsTab owner={owner} repo={repo} />}
        {tab === "transfer" && (
          <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
            <ChangeVisibilityCard owner={owner} repo={repo} repoData={data} />
            <TransferTab owner={owner} repo={repo} />
            <ArchiveRepoCard owner={owner} repo={repo} repoData={data} />
            <DeleteRepoCard owner={owner} repo={repo} />
          </div>
        )}
        {tab === "rename" && <RenameBranchTab owner={owner} repo={repo} />}
      </SettingsLayout>
    </div>
  );
}

function GeneralSettingsTab({ owner, repo, repoData }: { owner: string; repo: string; repoData: BleephubRepo }) {
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: (payload: RepoPatch) => patchRepo(owner, repo, payload),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repo", owner, repo] });
    },
  });

  const topicsQuery = useQuery({
    queryKey: ["repo-topics", owner, repo],
    queryFn: () => fetchRepoTopics(owner, repo),
    enabled: !!owner && !!repo,
  });

  const topicsMutation = useMutation({
    mutationFn: (names: string[]) => updateRepoTopics(owner, repo, names),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repo-topics", owner, repo] });
      queryClient.invalidateQueries({ queryKey: ["repo", owner, repo] });
    },
  });

  return (
    <>
      <RenameRepoCard owner={owner} repo={repo} />
      <RepoSettingsForm repo={repoData} onSave={(payload) => mutation.mutate(payload)} />
      <RepoTopicsForm
        topics={topicsQuery.data?.names ?? []}
        isLoading={topicsQuery.isLoading}
        onSave={(names) => topicsMutation.mutate(names)}
      />
      {mutation.isError && (
        <div className="mt-4" style={{ color: "var(--color-danger-fg)" }}>
          {mutation.error instanceof Error ? mutation.error.message : String(mutation.error)}
        </div>
      )}
      {mutation.isSuccess && (
        <div className="mt-4" style={{ color: "var(--gh-open)" }}>Settings saved.</div>
      )}
      {topicsMutation.isError && (
        <div className="mt-4" style={{ color: "var(--color-danger-fg)" }}>
          {topicsMutation.error instanceof Error ? topicsMutation.error.message : String(topicsMutation.error)}
        </div>
      )}
      {topicsMutation.isSuccess && (
        <div className="mt-4" style={{ color: "var(--gh-open)" }}>Topics saved.</div>
      )}
    </>
  );
}

function RepoTopicsForm({
  topics,
  isLoading,
  onSave,
}: {
  topics: string[];
  isLoading: boolean;
  onSave: (names: string[]) => void;
}) {
  const [value, setValue] = useState(topics.join(", "));
  useEffect(() => {
    setValue(topics.join(", "));
  }, [topics]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const names = value
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t.length > 0 && t.length <= 50 && !t.includes(" ") && !t.includes("/") && !t.includes("\\") && !t.includes(":"));
    onSave(names.slice(0, 20));
  };

  return (
    <form onSubmit={handleSubmit} className="mt-4">
      <Box header={<span style={{ fontWeight: 600 }}>Topics</span>}>
        <div style={{ display: "flex", flexDirection: "column", gap: "1rem", padding: "1rem" }}>
          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Topics (comma separated)</span>
            <input
              type="text"
              value={value}
              disabled={isLoading}
              onChange={(e) => setValue(e.target.value)}
              placeholder="e.g. go, ci, bleephub"
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            />
            <span style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
              Up to 20 topics, max 50 chars, no spaces or / \ :.
            </span>
          </label>
          <div className="flex justify-end">
            <Button type="submit" variant="primary">Save topics</Button>
          </div>
        </div>
      </Box>
    </form>
  );
}

// GitHub's default-commit-message enums, mirroring the repo PATCH schema.
const SQUASH_TITLE_OPTIONS = [
  { value: "COMMIT_OR_PR_TITLE", label: "Commit title (single commit) or PR title" },
  { value: "PR_TITLE", label: "Pull request title" },
] as const;
const SQUASH_MESSAGE_OPTIONS = [
  { value: "COMMIT_MESSAGES", label: "Commit messages" },
  { value: "PR_BODY", label: "Pull request body" },
  { value: "BLANK", label: "Blank" },
] as const;
const MERGE_TITLE_OPTIONS = [
  { value: "MERGE_MESSAGE", label: "Default merge message" },
  { value: "PR_TITLE", label: "Pull request title" },
] as const;
const MERGE_MESSAGE_OPTIONS = [
  { value: "PR_TITLE", label: "Pull request title" },
  { value: "PR_BODY", label: "Pull request body" },
  { value: "BLANK", label: "Blank" },
] as const;

function RenameRepoCard({ owner, repo }: { owner: string; repo: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [name, setName] = useState(repo);
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => patchRepo(owner, repo, { name: name.trim() }),
    onSuccess: (updated) => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["repos"] });
      // The PATCH response carries the new full_name — navigate to the renamed repo.
      const [newOwner, newName] = (updated.full_name ?? `${owner}/${name.trim()}`).split("/");
      navigate(`/ui/repos/${newOwner}/${newName}/settings`);
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Repository name</span>} className="mb-4">
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <div style={{ display: "flex", gap: "0.6rem", alignItems: "center", flexWrap: "wrap" }}>
          <input
            type="text"
            aria-label="Repository name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem", flex: 1, minWidth: "12rem" }}
          />
          <Button
            variant="secondary"
            onClick={() => {
              setError(null);
              mutation.mutate();
            }}
            disabled={mutation.isPending || !name.trim() || name.trim() === repo}
          >
            Rename
          </Button>
        </div>
      </div>
    </Box>
  );
}

function RepoSettingsForm({
  repo,
  onSave,
}: {
  repo: BleephubRepo;
  onSave: (payload: RepoPatch) => void;
}) {
  const extras = repo as BleephubRepo & RepoSettingsExtras;
  const [description, setDescription] = useState(repo.description ?? "");
  const [homepage, setHomepage] = useState(repo.homepage ?? "");
  const [defaultBranch, setDefaultBranch] = useState(repo.default_branch);
  const [hasIssues, setHasIssues] = useState(repo.has_issues);
  const [hasProjects, setHasProjects] = useState(repo.has_projects);
  const [hasWiki, setHasWiki] = useState(repo.has_wiki);
  const [hasDiscussions, setHasDiscussions] = useState(extras.has_discussions ?? false);
  const [hasPullRequests, setHasPullRequests] = useState(repo.has_pull_requests);
  const [isTemplate, setIsTemplate] = useState(repo.is_template);
  const [signoffRequired, setSignoffRequired] = useState(repo.web_commit_signoff_required ?? false);
  const [allowSquashMerge, setAllowSquashMerge] = useState(repo.allow_squash_merge);
  const [allowMergeCommit, setAllowMergeCommit] = useState(repo.allow_merge_commit);
  const [allowRebaseMerge, setAllowRebaseMerge] = useState(repo.allow_rebase_merge);
  const [allowAutoMerge, setAllowAutoMerge] = useState(repo.allow_auto_merge);
  const [allowUpdateBranch, setAllowUpdateBranch] = useState(repo.allow_update_branch ?? false);
  const [deleteBranchOnMerge, setDeleteBranchOnMerge] = useState(repo.delete_branch_on_merge);
  const [squashTitle, setSquashTitle] = useState(repo.squash_merge_commit_title || "COMMIT_OR_PR_TITLE");
  const [squashMessage, setSquashMessage] = useState(repo.squash_merge_commit_message || "COMMIT_MESSAGES");
  const [mergeTitle, setMergeTitle] = useState(repo.merge_commit_title || "MERGE_MESSAGE");
  const [mergeMessage, setMergeMessage] = useState(repo.merge_commit_message || "PR_TITLE");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    onSave({
      description: description.trim(),
      homepage: homepage.trim() || null,
      default_branch: defaultBranch.trim(),
      has_issues: hasIssues,
      has_projects: hasProjects,
      has_wiki: hasWiki,
      has_discussions: hasDiscussions,
      has_pull_requests: hasPullRequests,
      is_template: isTemplate,
      web_commit_signoff_required: signoffRequired,
      allow_squash_merge: allowSquashMerge,
      allow_merge_commit: allowMergeCommit,
      allow_rebase_merge: allowRebaseMerge,
      allow_auto_merge: allowAutoMerge,
      allow_update_branch: allowUpdateBranch,
      delete_branch_on_merge: deleteBranchOnMerge,
      squash_merge_commit_title: squashTitle,
      squash_merge_commit_message: squashMessage,
      merge_commit_title: mergeTitle,
      merge_commit_message: mergeMessage,
    });
  };

  return (
    <form onSubmit={handleSubmit}>
      <Box
        header={<span style={{ fontWeight: 600 }}>Repository settings</span>
        }
      >
        <div style={{ display: "flex", flexDirection: "column", gap: "1rem", padding: "1rem" }}>
          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Description</span>
            <input
              type="text"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Short description of this repository"
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            />
          </label>

          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Website</span>
            <input
              type="text"
              value={homepage}
              onChange={(e) => setHomepage(e.target.value)}
              placeholder="https://example.com"
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            />
          </label>

          <label style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
            <span style={{ fontSize: "0.85rem", fontWeight: 500 }}>Default branch</span>
            <input
              type="text"
              value={defaultBranch}
              onChange={(e) => setDefaultBranch(e.target.value)}
              style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
            />
          </label>

          <fieldset style={{ border: "none", padding: 0, margin: 0 }}>
            <legend style={{ fontSize: "0.85rem", fontWeight: 500, marginBottom: "0.5rem" }}>Features</legend>
            <div style={{ display: "flex", flexDirection: "column", gap: "0.4rem" }}>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={hasIssues} onChange={(e) => setHasIssues(e.target.checked)} />
                Issues
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={hasProjects} onChange={(e) => setHasProjects(e.target.checked)} />
                Projects
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={hasWiki} onChange={(e) => setHasWiki(e.target.checked)} />
                Wiki
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={hasDiscussions} onChange={(e) => setHasDiscussions(e.target.checked)} />
                Discussions
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={hasPullRequests} onChange={(e) => setHasPullRequests(e.target.checked)} />
                Pull requests
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={isTemplate} onChange={(e) => setIsTemplate(e.target.checked)} />
                Template repository
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={signoffRequired} onChange={(e) => setSignoffRequired(e.target.checked)} />
                Require contributors to sign off on web-based commits
              </label>
            </div>
          </fieldset>

          <fieldset style={{ border: "none", padding: 0, margin: 0 }}>
            <legend style={{ fontSize: "0.85rem", fontWeight: 500, marginBottom: "0.5rem" }}>Merge button</legend>
            <div style={{ display: "flex", flexDirection: "column", gap: "0.4rem" }}>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={allowSquashMerge} onChange={(e) => setAllowSquashMerge(e.target.checked)} />
                Allow squash merging
              </label>
              {allowSquashMerge && (
                <div style={{ display: "flex", gap: "0.6rem", flexWrap: "wrap", paddingLeft: "1.5rem" }}>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.78rem" }}>
                    Default squash commit title
                    <select aria-label="Default squash commit title" value={squashTitle} onChange={(e) => setSquashTitle(e.target.value)} style={{ fontSize: "0.85rem", padding: "0.3rem 0.4rem" }}>
                      {SQUASH_TITLE_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  </label>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.78rem" }}>
                    Default squash commit message
                    <select aria-label="Default squash commit message" value={squashMessage} onChange={(e) => setSquashMessage(e.target.value)} style={{ fontSize: "0.85rem", padding: "0.3rem 0.4rem" }}>
                      {SQUASH_MESSAGE_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  </label>
                </div>
              )}
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={allowMergeCommit} onChange={(e) => setAllowMergeCommit(e.target.checked)} />
                Allow merge commits
              </label>
              {allowMergeCommit && (
                <div style={{ display: "flex", gap: "0.6rem", flexWrap: "wrap", paddingLeft: "1.5rem" }}>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.78rem" }}>
                    Default merge commit title
                    <select aria-label="Default merge commit title" value={mergeTitle} onChange={(e) => setMergeTitle(e.target.value)} style={{ fontSize: "0.85rem", padding: "0.3rem 0.4rem" }}>
                      {MERGE_TITLE_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  </label>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.78rem" }}>
                    Default merge commit message
                    <select aria-label="Default merge commit message" value={mergeMessage} onChange={(e) => setMergeMessage(e.target.value)} style={{ fontSize: "0.85rem", padding: "0.3rem 0.4rem" }}>
                      {MERGE_MESSAGE_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                    </select>
                  </label>
                </div>
              )}
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={allowRebaseMerge} onChange={(e) => setAllowRebaseMerge(e.target.checked)} />
                Allow rebase merging
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={allowAutoMerge} onChange={(e) => setAllowAutoMerge(e.target.checked)} />
                Allow auto-merge
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={allowUpdateBranch} onChange={(e) => setAllowUpdateBranch(e.target.checked)} />
                Always suggest updating pull request branches
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
                <input type="checkbox" checked={deleteBranchOnMerge} onChange={(e) => setDeleteBranchOnMerge(e.target.checked)} />
                Automatically delete head branches
              </label>
            </div>
          </fieldset>

          <div className="flex justify-end">
            <Button type="submit" variant="primary">Save changes</Button>
          </div>
        </div>
      </Box>
    </form>
  );
}

function BranchesSettingsTab({ owner, repo }: { owner: string; repo: string }) {
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Branch protection</span>}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "1rem", padding: "1rem" }}>
        <span style={{ fontSize: "0.9rem" }}>Define merge constraints and required status checks per branch.</span>
        <ButtonLink to={`/ui/repos/${owner}/${repo}/settings/branch-protection`} variant="secondary" size="sm">
          Manage branch protection
        </ButtonLink>
      </div>
    </Box>
  );
}

function SecretsAndVariablesCard({ owner, repo }: { owner: string; repo: string }) {
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Secrets and variables</span>}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "1rem", padding: "1rem" }}>
        <span style={{ fontSize: "0.9rem" }}>Manage Actions secrets and variables across repository, environment, and organization scopes.</span>
        <ButtonLink to={`/ui/repos/${owner}/${repo}/settings/secrets`} variant="secondary" size="sm">
          Manage secrets and variables
        </ButtonLink>
      </div>
    </Box>
  );
}

function TagsSettingsTab({ owner, repo }: { owner: string; repo: string }) {
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Protected tags</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
          This server does not expose the (deprecated) tag-protection API. To restrict who can
          create or delete tags, create a <strong>tag-targeted ruleset</strong> under
          Rulesets instead — it supports tag name patterns and bypass lists.
        </p>
        <div className="flex gap-2">
          <ButtonLink to={`/ui/repos/${owner}/${repo}/tags`} variant="secondary" size="sm">
            View tags
          </ButtonLink>
        </div>
      </div>
    </Box>
  );
}

function CollaboratorsTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [username, setUsername] = useState("");
  const [permission, setPermission] = useState("push");

  const collaboratorsQuery = useQuery({
    queryKey: ["repo-collaborators", owner, repo],
    queryFn: () => fetchRepoCollaborators(owner, repo),
    enabled: !!owner && !!repo,
  });
  const invitationsQuery = useQuery({
    queryKey: ["repo-invitations", owner, repo],
    queryFn: () => fetchRepoInvitations(owner, repo),
    enabled: !!owner && !!repo,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["repo-collaborators", owner, repo] });
    queryClient.invalidateQueries({ queryKey: ["repo-invitations", owner, repo] });
  };

  const inviteMut = useMutation({
    mutationFn: () => inviteRepoCollaborator(owner, repo, username.trim(), permission),
    onSuccess: (invitation) => {
      setError(null);
      setNotice(
        invitation
          ? `Invitation sent to ${invitation.invitee?.login ?? username.trim()}.`
          : `Updated ${username.trim()}'s permission.`,
      );
      setUsername("");
      invalidate();
    },
    onError: (err: Error) => {
      setNotice(null);
      setError(err.message);
    },
  });

  const removeMut = useMutation({
    mutationFn: (login: string) => removeRepoCollaborator(owner, repo, login),
    onSuccess: () => {
      setError(null);
      setNotice(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  const cancelMut = useMutation({
    mutationFn: (invitationId: number) => cancelRepoInvitation(owner, repo, invitationId),
    onSuccess: () => {
      setError(null);
      setNotice(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  if (collaboratorsQuery.isLoading || invitationsQuery.isLoading)
    return <Spinner label="loading collaborators" />;
  if (collaboratorsQuery.isError)
    return <InlineError title="Failed to load collaborators" detail={String(collaboratorsQuery.error)} />;
  if (invitationsQuery.isError)
    return <InlineError title="Failed to load invitations" detail={String(invitationsQuery.error)} />;

  const collaborators = collaboratorsQuery.data ?? [];
  const invitations = invitationsQuery.data ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {notice && <div style={{ color: "var(--gh-open)", fontSize: "0.85rem" }}>{notice}</div>}
      <Box header={<span style={{ fontWeight: 600 }}>Invite a collaborator</span>}>
        <div style={{ padding: "1rem", display: "flex", flexWrap: "wrap", gap: "0.75rem", alignItems: "center" }}>
          <input
            type="text"
            aria-label="Username to invite"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="username"
            className="flex-1"
            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem", minWidth: "12rem" }}
          />
          <select
            aria-label="Role"
            value={permission}
            onChange={(e) => setPermission(e.target.value)}
            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
          >
            <option value="pull">Read</option>
            <option value="push">Write</option>
            <option value="admin">Admin</option>
          </select>
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              setNotice(null);
              inviteMut.mutate();
            }}
            disabled={inviteMut.isPending || !username.trim()}
          >
            Invite
          </Button>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Collaborators</span>}>
        {collaborators.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No collaborators.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {collaborators.map((c: GithubCollaborator) => (
              <li
                key={c.login}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div>
                  <span style={{ fontWeight: 500 }}>{c.login}</span>
                  <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    {c.role_name}
                  </span>
                </div>
                {c.login !== owner && (
                  <Button
                    size="sm"
                    variant="danger"
                    onClick={async () => {
                      if (await confirmAction(`Remove ${c.login} from ${owner}/${repo}?`)) {
                        removeMut.mutate(c.login);
                      }
                    }}
                    disabled={removeMut.isPending}
                  >
                    remove
                  </Button>
                )}
              </li>
            ))}
          </ul>
        )}
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Pending invitations</span>}>
        {invitations.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No pending invitations.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {invitations.map((inv: GithubRepoInvitation) => (
              <li
                key={inv.id}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div>
                  <span style={{ fontWeight: 500 }}>{inv.invitee?.login ?? "unknown"}</span>
                  <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    {inv.permissions} · invited by {inv.inviter?.login ?? "unknown"}{" "}
                    <RelativeTime iso={inv.created_at} />
                  </span>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  onClick={() => cancelMut.mutate(inv.id)}
                  disabled={cancelMut.isPending}
                >
                  cancel
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Box>
    </div>
  );
}

function DeployKeysTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [key, setKey] = useState("");
  const [readOnly, setReadOnly] = useState(false);

  const query = useQuery({
    queryKey: ["repo-deploy-keys", owner, repo],
    queryFn: () => fetchRepoDeployKeys(owner, repo),
    enabled: !!owner && !!repo,
  });

  const addMut = useMutation({
    mutationFn: () => addRepoDeployKey(owner, repo, title.trim(), key.trim(), readOnly),
    onSuccess: () => {
      setError(null);
      setTitle("");
      setKey("");
      setReadOnly(false);
      queryClient.invalidateQueries({ queryKey: ["repo-deploy-keys", owner, repo] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (keyId: number) => deleteRepoDeployKey(owner, repo, keyId),
    onSuccess: () => {
      setError(null);
      queryClient.invalidateQueries({ queryKey: ["repo-deploy-keys", owner, repo] });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label="loading deploy keys" />;
  if (query.isError) return <InlineError title="Failed to load deploy keys" />;

  const keys = query.data ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Add deploy key</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
          <FormLabel id="deploy-key-title">Title</FormLabel>
          <input
            id="deploy-key-title"
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            className="w-full"
          />
          <FormLabel id="deploy-key-key">Key</FormLabel>
          <textarea
            id="deploy-key-key"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            rows={4}
            className="w-full"
            style={{ fontFamily: "var(--font-mono)", fontSize: "0.8rem" }}
          />
          <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
            <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} />
            Read-only
          </label>
          <div className="flex justify-end">
            <Button
              variant="primary"
              onClick={() => {
                setError(null);
                addMut.mutate();
              }}
              disabled={addMut.isPending || !title.trim() || !key.trim()}
            >
              Add key
            </Button>
          </div>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Deploy keys</span>}>
        <div style={{ padding: "0" }}>
          {keys.length === 0 ? (
            <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
              No deploy keys.
            </div>
          ) : (
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {keys.map((k: GithubDeployKey) => (
                <li
                  key={k.id}
                  style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    padding: "0.6rem 1rem",
                    borderBottom: "1px solid var(--color-border)",
                    gap: "1rem",
                  }}
                >
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontWeight: 500 }}>{k.title}</div>
                    <div
                      style={{
                        color: "var(--color-fg-muted)",
                        fontSize: "0.8rem",
                        fontFamily: "var(--font-mono)",
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                      }}
                    >
                      {k.key}
                    </div>
                    <div style={{ color: "var(--color-fg-muted)", fontSize: "0.75rem" }}>
                      {k.read_only ? "read-only" : "read/write"} · {k.verified ? "verified" : "unverified"}
                    </div>
                  </div>
                  <Button
                    size="sm"
                    variant="danger"
                    onClick={async () => {
                      if (await confirmAction(`Delete deploy key "${k.title}"?`)) {
                        deleteMut.mutate(k.id);
                      }
                    }}
                    disabled={deleteMut.isPending}
                  >
                    delete
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      </Box>
    </div>
  );
}

// The four security_and_analysis toggles the repo PATCH accepts
// (internal/server/gh_repos_rest.go). dependabot_security_updates is read-only
// in that payload — it goes through PUT/DELETE /automated-security-fixes.
const SECURITY_ANALYSIS_TOGGLES: { key: keyof NonNullable<RepoSettingsExtras["security_and_analysis"]>; label: string; description: string }[] = [
  { key: "advanced_security", label: "GitHub Advanced Security", description: "Enable code security features for this repository." },
  { key: "secret_scanning", label: "Secret scanning", description: "Scan the repository for known secret formats." },
  { key: "secret_scanning_push_protection", label: "Push protection", description: "Block pushes that contain detected secrets." },
  { key: "secret_scanning_non_provider_patterns", label: "Scan for non-provider patterns", description: "Also scan for generic secret patterns such as private keys." },
];

function SecurityAnalysisSection({ owner, repo, repoData }: { owner: string; repo: string; repoData: BleephubRepo }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const current = (repoData as BleephubRepo & RepoSettingsExtras).security_and_analysis ?? {};

  const mutation = useMutation({
    mutationFn: (patch: NonNullable<RepoSettingsExtras["security_and_analysis"]>) =>
      patchRepo(owner, repo, { security_and_analysis: patch }),
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["repo", owner, repo] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Code security and analysis</span>}>
      <div style={{ display: "flex", flexDirection: "column" }}>
        {error && <div style={{ padding: "0.75rem 1rem 0" }}><ErrorBanner>{error}</ErrorBanner></div>}
        {SECURITY_ANALYSIS_TOGGLES.map((t, i) => {
          const enabled = current[t.key]?.status === "enabled";
          return (
            <div
              key={t.key}
              style={{
                display: "flex",
                alignItems: "center",
                justifyContent: "space-between",
                gap: "1rem",
                padding: "0.75rem 1rem",
                borderBottom: i < SECURITY_ANALYSIS_TOGGLES.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <div>
                <div style={{ fontSize: "0.9rem", fontWeight: 500 }}>{t.label}</div>
                <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{t.description}</div>
              </div>
              <Button
                size="sm"
                variant={enabled ? "danger" : "secondary"}
                aria-label={`${enabled ? "Disable" : "Enable"} ${t.label}`}
                disabled={mutation.isPending}
                onClick={() => mutation.mutate({ [t.key]: { status: enabled ? "disabled" : "enabled" } })}
              >
                {enabled ? "Disable" : "Enable"}
              </Button>
            </div>
          );
        })}
      </div>
    </Box>
  );
}

function SecurityTab({ owner, repo, repoData }: { owner: string; repo: string; repoData: BleephubRepo }) {
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  type FlagKey = "automated_security_fixes" | "private_vulnerability_reporting" | "vulnerability_alerts";
  const [flags, setFlags] = useState<Record<FlagKey, boolean>>({
    automated_security_fixes: false,
    private_vulnerability_reporting: false,
    vulnerability_alerts: false,
  });

  // Seed each checkbox from its own dedicated status endpoint. The repo object
  // carries no `security_and_analysis` block (the server never emits it), so
  // reading the flags off the repo detail left every toggle stuck unchecked —
  // these are the same endpoints setRepoFlag writes to.
  const securityQuery = useQuery({
    queryKey: ["repo-security-flags", owner, repo],
    queryFn: async () => {
      const [automated_security_fixes, private_vulnerability_reporting, vulnerability_alerts] =
        await Promise.all([
          fetchRepoAutomatedSecurityFixes(owner, repo),
          fetchRepoPrivateVulnerabilityReporting(owner, repo),
          fetchRepoVulnerabilityAlerts(owner, repo),
        ]);
      return { automated_security_fixes, private_vulnerability_reporting, vulnerability_alerts };
    },
    enabled: !!owner && !!repo,
  });
  useEffect(() => {
    if (securityQuery.data) setFlags(securityQuery.data);
  }, [securityQuery.data]);

  const mutation = useMutation({
    mutationFn: ({ flag, enabled }: { flag: FlagKey; enabled: boolean }) => setRepoFlag(owner, repo, flag, enabled),
    onSuccess: (_, vars) => {
      setError(null);
      setSuccess(`Updated ${vars.flag.replace(/_/g, " ")}.`);
      setFlags((prev) => ({ ...prev, [vars.flag]: vars.enabled }));
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  const toggle = (flag: FlagKey) => {
    const enabled = !flags[flag];
    setError(null);
    setSuccess(null);
    mutation.mutate({ flag, enabled });
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <SecurityAnalysisSection owner={owner} repo={repo} repoData={repoData} />
      <Box header={<span style={{ fontWeight: 600 }}>Dependabot and vulnerability reporting</span>}>
        {securityQuery.isLoading ? (
          <div style={{ padding: "1rem" }}>
            <Spinner label="loading security settings" />
          </div>
        ) : (
        <div style={{ display: "flex", flexDirection: "column" }}>
          <div style={{ padding: "0.75rem 1rem 0", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
            {error && <ErrorBanner>{error}</ErrorBanner>}
            {success && <div style={{ color: "var(--gh-open)", fontSize: "0.85rem" }}>{success}</div>}
          </div>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              justifyContent: "space-between",
              gap: "1rem",
              padding: "0.75rem 1rem",
              borderBottom: "1px solid var(--color-border)",
            }}
          >
            <div>
              <div style={{ fontSize: "0.9rem", fontWeight: 500 }}>Dependabot security updates</div>
              <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                Automatically open pull requests that fix vulnerable dependencies.
              </div>
            </div>
            <Button
              size="sm"
              variant={flags.automated_security_fixes ? "danger" : "secondary"}
              aria-label={`${flags.automated_security_fixes ? "Disable" : "Enable"} Dependabot security updates`}
              disabled={mutation.isPending}
              onClick={() => toggle("automated_security_fixes")}
            >
              {flags.automated_security_fixes ? "Disable" : "Enable"}
            </Button>
          </div>
          <div style={{ padding: "0.75rem 1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
            <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
              <input
                type="checkbox"
                checked={flags.private_vulnerability_reporting}
                onChange={() => toggle("private_vulnerability_reporting")}
                disabled={mutation.isPending}
              />
              Private vulnerability reporting
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
              <input
                type="checkbox"
                checked={flags.vulnerability_alerts}
                onChange={() => toggle("vulnerability_alerts")}
                disabled={mutation.isPending}
              />
              Vulnerability alerts
            </label>
          </div>
        </div>
        )}
      </Box>
    </div>
  );
}

function InteractionTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [limit, setLimit] = useState<string | null>(null);

  // Load the repo's current interaction limit so the select reflects server
  // state; this is also the reader that makes the onSuccess invalidation live.
  const limitQuery = useQuery({
    queryKey: ["repo-interaction-limit", owner, repo],
    queryFn: () => fetchRepoInteractionLimit(owner, repo),
    enabled: !!owner && !!repo,
  });
  const loadedLimit = limitQuery.data?.limit;
  useEffect(() => {
    setLimit(loadedLimit ?? null);
  }, [loadedLimit]);

  const mutation = useMutation({
    mutationFn: () => setRepoInteractionLimit(owner, repo, limit),
    onSuccess: () => {
      setError(null);
      setSuccess(limit === null ? "Interaction limit cleared." : `Interaction limit set to ${limit}.`);
      queryClient.invalidateQueries({ queryKey: ["repo-interaction-limit", owner, repo] });
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Interaction limits</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {success && <div style={{ color: "var(--gh-open)" }}>{success}</div>}
        <FormLabel id="interaction-limit">Limit</FormLabel>
        <select
          id="interaction-limit"
          value={limit ?? ""}
          onChange={(e) => setLimit(e.target.value || null)}
          className="w-full"
        >
          <option value="">No limit</option>
          <option value="existing_users">Existing users</option>
          <option value="contributors_only">Contributors only</option>
          <option value="collaborators_only">Collaborators only</option>
        </select>
        <div className="flex justify-end gap-2">
          <Button
            variant="ghost"
            onClick={() => {
              setError(null);
              setSuccess(null);
              setLimit(null);
              mutation.mutate();
            }}
            disabled={mutation.isPending}
          >
            Clear limit
          </Button>
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              setSuccess(null);
              mutation.mutate();
            }}
            disabled={mutation.isPending}
          >
            Set limit
          </Button>
        </div>
      </div>
    </Box>
  );
}

function ChangeVisibilityCard({ owner, repo, repoData }: { owner: string; repo: string; repoData: BleephubRepo }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const isPrivate = repoData.private;

  const mutation = useMutation({
    mutationFn: () =>
      patchRepo(owner, repo, { private: !isPrivate, visibility: isPrivate ? "public" : "private" }),
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["repo", owner, repo] });
      void queryClient.invalidateQueries({ queryKey: ["repos"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const handleChange = async () => {
    const target = isPrivate ? "public" : "private";
    const confirmed = await confirmAction(
      isPrivate
        ? `Making ${owner}/${repo} public exposes its code, issues, and history to everyone.`
        : `Making ${owner}/${repo} private hides it from everyone without explicit access.`,
      {
        title: `Change visibility to ${target}?`,
        confirmLabel: `Make ${target}`,
        expectedText: `${owner}/${repo}`,
      },
    );
    if (confirmed) mutation.mutate();
  };

  return (
    <Box header={<span style={{ fontWeight: 600, color: "var(--gh-danger, var(--color-danger-fg))" }}>Change repository visibility</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          This repository is currently <strong>{isPrivate ? "private" : "public"}</strong>.
        </p>
        <div className="flex justify-end">
          <Button variant="danger" onClick={handleChange} disabled={mutation.isPending}>
            {isPrivate ? "Make public" : "Make private"}
          </Button>
        </div>
      </div>
    </Box>
  );
}

function TransferTab({ owner, repo }: { owner: string; repo: string }) {
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [newOwner, setNewOwner] = useState("");

  const mutation = useMutation({
    mutationFn: () => transferRepo(owner, repo, newOwner.trim()),
    onSuccess: () => {
      setError(null);
      setSuccess(`Repository transferred to ${newOwner.trim()}.`);
      setNewOwner("");
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  const handleTransfer = async () => {
    const confirmed = await confirmAction(
      `Transferring ${owner}/${repo} to ${newOwner.trim()} removes you as owner. You may lose admin access to it.`,
      { title: "Transfer this repository?", confirmLabel: "Transfer", expectedText: `${owner}/${repo}` },
    );
    if (!confirmed) return;
    setError(null);
    setSuccess(null);
    mutation.mutate();
  };

  return (
    <Box header={<span style={{ fontWeight: 600, color: "var(--gh-danger, var(--color-danger-fg))" }}>Transfer repository</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {success && <div style={{ color: "var(--gh-open)" }}>{success}</div>}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Transferring removes you as owner and moves the repository to the target owner or organization.
        </p>
        <FormLabel id="transfer-owner">New owner login</FormLabel>
        <input
          id="transfer-owner"
          type="text"
          value={newOwner}
          onChange={(e) => setNewOwner(e.target.value)}
          placeholder="owner or org"
          className="w-full"
        />
        <div className="flex justify-end">
          <Button
            variant="danger"
            onClick={() => void handleTransfer()}
            disabled={mutation.isPending || !newOwner.trim()}
          >
            Transfer
          </Button>
        </div>
      </div>
    </Box>
  );
}

function AutolinksTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const [keyPrefix, setKeyPrefix] = useState("");
  const [urlTemplate, setUrlTemplate] = useState("");
  const listQ = useQuery({
    queryKey: ["repo-autolinks", owner, repo],
    queryFn: () => fetchRepoAutolinks(owner, repo),
    enabled: !!owner && !!repo,
  });
  const invalidate = () =>
    void queryClient.invalidateQueries({ queryKey: ["repo-autolinks", owner, repo] });
  const createMut = useMutation({
    mutationFn: () =>
      createRepoAutolink(owner, repo, { key_prefix: keyPrefix.trim(), url_template: urlTemplate.trim() }),
    onSuccess: () => {
      invalidate();
      setKeyPrefix("");
      setUrlTemplate("");
    },
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteRepoAutolink(owner, repo, id),
    onSuccess: invalidate,
  });

  const autolinks: GithubAutolink[] = listQ.data ?? [];
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <Box header={<span style={{ fontWeight: 600 }}>Add autolink reference</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
          {createMut.error && <ErrorBanner>{String(createMut.error)}</ErrorBanner>}
          <FormLabel id="autolink-prefix">Reference prefix</FormLabel>
          <input
            id="autolink-prefix"
            type="text"
            value={keyPrefix}
            onChange={(e) => setKeyPrefix(e.target.value)}
            placeholder="TICKET-"
            className="w-full"
          />
          <FormLabel id="autolink-url">Target URL</FormLabel>
          <input
            id="autolink-url"
            type="text"
            value={urlTemplate}
            onChange={(e) => setUrlTemplate(e.target.value)}
            placeholder="https://example.com/TICKET?query=<num>"
            className="w-full"
          />
          <div className="flex justify-end">
            <Button
              variant="primary"
              disabled={createMut.isPending || !keyPrefix.trim() || !urlTemplate.trim()}
              onClick={() => createMut.mutate()}
            >
              Add autolink
            </Button>
          </div>
        </div>
      </Box>
      {deleteMut.error && <ErrorBanner>{String(deleteMut.error)}</ErrorBanner>}
      {listQ.isLoading ? (
        <Spinner label="loading autolinks" />
      ) : listQ.isError ? (
        <InlineError title="Failed to load autolinks" detail={String(listQ.error)} />
      ) : autolinks.length === 0 ? null : (
        <Box>
          {autolinks.map((a, i) => (
            <div
              key={a.id}
              className="flex items-center gap-3"
              style={{ padding: "0.65rem 1rem", borderBottom: i === autolinks.length - 1 ? "none" : "1px solid var(--color-border)" }}
            >
              <div className="min-w-0 flex-1">
                <div className="font-mono" style={{ fontWeight: 500 }}>{a.key_prefix}&lt;num&gt;</div>
                <div style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", wordBreak: "break-all" }}>
                  {a.url_template}
                </div>
              </div>
              <Button
                size="sm"
                variant="danger"
                aria-label={`Delete autolink ${a.key_prefix}`}
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete autolink ${a.key_prefix}?`)) deleteMut.mutate(a.id);
                }}
              >
                Delete
              </Button>
            </div>
          ))}
        </Box>
      )}
    </div>
  );
}

// Create/PATCH a repo hook with a full config (url/content_type/secret/
// insecure_ssl) — the entry-resident createRepoHook/updateRepoHook wrappers'
// config types carry neither secret nor insecure_ssl, so the webhooks tab
// posts/patches through these lazy-page fetchers instead of widening api.ts.
// A blank secret is omitted: the server keeps the stored secret when
// config.secret is absent/empty (internal/server/gh_hooks_rest.go).
type RepoHookConfigBody = { url: string; content_type: string; insecure_ssl: string; secret?: string };
const createRepoHookFull = (
  owner: string,
  repo: string,
  body: { name: "web"; active: boolean; events: string[]; config: RepoHookConfigBody },
) => ghPostJSON<GithubWebhook>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/hooks`, body);
const patchRepoHookFull = (
  owner: string,
  repo: string,
  id: number,
  body: { active: boolean; events: string[]; config: RepoHookConfigBody },
) => ghSend("PATCH", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/hooks/${id}`, body);

function WebhooksTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<GithubWebhook | null>(null);

  const listQ = useQuery({
    queryKey: ["repo-hooks", owner, repo],
    queryFn: () => fetchWebhooks(owner, repo),
    enabled: !!owner && !!repo,
  });
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ["repo-hooks", owner, repo] });

  const createMut = useMutation({
    mutationFn: (values: WebhookFormValues) =>
      createRepoHookFull(owner, repo, {
        name: "web",
        active: values.active,
        events: values.events,
        config: {
          url: values.url,
          content_type: values.contentType,
          insecure_ssl: values.insecureSsl,
          ...(values.secret ? { secret: values.secret } : {}),
        },
      }),
    onSuccess: invalidate,
  });
  const editMut = useMutation({
    mutationFn: ({ id, values }: { id: number; values: WebhookFormValues }) =>
      patchRepoHookFull(owner, repo, id, {
        active: values.active,
        events: values.events,
        config: {
          url: values.url,
          content_type: values.contentType,
          insecure_ssl: values.insecureSsl,
          ...(values.secret ? { secret: values.secret } : {}),
        },
      }),
    onSuccess: () => {
      invalidate();
      setEditing(null);
    },
  });
  const toggleMut = useMutation({
    mutationFn: (h: GithubWebhook) => updateRepoHook(owner, repo, h.id, { active: !h.active }),
    onSuccess: invalidate,
  });
  const pingMut = useMutation({
    mutationFn: (id: number) => pingRepoHook(owner, repo, id),
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteRepoHook(owner, repo, id),
    onSuccess: invalidate,
  });

  const hooks: GithubWebhook[] = listQ.data ?? [];
  const busy = toggleMut.isPending || pingMut.isPending || deleteMut.isPending;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <Box header={<span style={{ fontWeight: 600 }}>Add webhook</span>}>
        <div style={{ padding: "1rem" }}>
          {createMut.error && <div className="mb-2"><ErrorBanner>{String(createMut.error)}</ErrorBanner></div>}
          <WebhookForm
            submitLabel="Add webhook"
            pendingLabel="Adding…"
            pending={createMut.isPending}
            onSubmit={(values) => createMut.mutate(values)}
          />
        </div>
      </Box>

      {editing && (
        <Modal title={`Edit webhook #${editing.id}`} onClose={() => setEditing(null)}>
          {editMut.error && <div className="mb-2"><ErrorBanner>{String(editMut.error)}</ErrorBanner></div>}
          <WebhookForm
            initial={{
              url: editing.config.url,
              contentType: editing.config.content_type,
              // The GET config carries insecure_ssl; GithubWebhook.config's
              // inline type omits it (types.ts is entry-resident), so read it
              // through a local widening.
              insecureSsl:
                (editing.config as GithubWebhook["config"] & { insecure_ssl?: string }).insecure_ssl === "1"
                  ? "1"
                  : "0",
              events: editing.events,
              active: editing.active,
            }}
            editingWithSecret
            submitLabel="Update webhook"
            pendingLabel="Updating…"
            pending={editMut.isPending}
            onSubmit={(values) => editMut.mutate({ id: editing.id, values })}
          />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setEditing(null)}>
              Cancel
            </Button>
          </DialogActions>
        </Modal>
      )}

      {(toggleMut.error || pingMut.error || deleteMut.error) && (
        <ErrorBanner>{String(toggleMut.error ?? pingMut.error ?? deleteMut.error)}</ErrorBanner>
      )}
      {pingMut.isSuccess && <div style={{ fontSize: "0.82rem", color: "var(--gh-open)" }}>Ping sent.</div>}

      {listQ.isLoading ? (
        <Spinner label="loading webhooks" />
      ) : listQ.isError ? (
        <InlineError title="Failed to load webhooks" detail={String(listQ.error)} />
      ) : hooks.length === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No webhooks yet.</p>
      ) : (
        <Box>
          {hooks.map((h, i) => (
            <div
              key={h.id}
              className="flex items-center gap-3"
              style={{ padding: "0.7rem 1rem", borderBottom: i < hooks.length - 1 ? "1px solid var(--color-border)" : "none" }}
            >
              <span
                aria-hidden
                style={{ width: 8, height: 8, borderRadius: "999px", background: h.active ? "var(--gh-open)" : "var(--color-fg-subtle)", flexShrink: 0 }}
              />
              <div className="min-w-0 flex-1">
                <div style={{ fontSize: "0.88rem", fontWeight: 500, color: "var(--color-fg)" }}>
                  {h.name} <span style={{ color: "var(--color-fg-subtle)", fontWeight: 400 }}>#{h.id}</span>
                </div>
                <div className="font-mono" style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)", wordBreak: "break-all" }}>
                  {h.config.url || "no url"} · events: {h.events.join(", ") || "none"}
                </div>
              </div>
              <Link to={`/ui/repos/${owner}/${repo}/hooks/${h.id}/deliveries`} style={{ fontSize: "0.8rem", color: "var(--color-accent)", textDecoration: "none" }}>
                Deliveries
              </Link>
              <Button size="sm" aria-label={`Edit webhook ${h.id}`} disabled={busy} onClick={() => setEditing(h)}>
                Edit
              </Button>
              <Button size="sm" aria-label={`Ping webhook ${h.id}`} disabled={busy} onClick={() => pingMut.mutate(h.id)}>
                Ping
              </Button>
              <Button size="sm" disabled={busy} onClick={() => toggleMut.mutate(h)}>
                {h.active ? "Disable" : "Enable"}
              </Button>
              <Button
                size="sm"
                variant="danger"
                aria-label={`Delete webhook ${h.id}`}
                disabled={busy}
                onClick={async () => {
                  if (await confirmAction(`Delete webhook #${h.id}?`, { title: "Delete webhook", confirmLabel: "Delete" })) {
                    deleteMut.mutate(h.id);
                  }
                }}
              >
                Delete
              </Button>
            </div>
          ))}
        </Box>
      )}
    </div>
  );
}

function ArchiveRepoCard({ owner, repo, repoData }: { owner: string; repo: string; repoData: BleephubRepo }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const archived = repoData.archived;

  const mutation = useMutation({
    mutationFn: () => updateRepo(owner, repo, { archived: !archived }),
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["repo", owner, repo] });
      void queryClient.invalidateQueries({ queryKey: ["repos"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const handleToggle = async () => {
    if (archived) {
      mutation.mutate();
      return;
    }
    const confirmed = await confirmAction(
      `Archiving makes ${owner}/${repo} read-only. Issues, pull requests, and settings can no longer be changed until you unarchive it.`,
      { title: "Archive this repository?", confirmLabel: "Archive", expectedText: repo },
    );
    if (confirmed) mutation.mutate();
  };

  return (
    <Box header={<span style={{ fontWeight: 600, color: "var(--gh-danger, var(--color-danger-fg))" }}>{archived ? "Unarchive this repository" : "Archive this repository"}</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          {archived
            ? "This repository is archived and read-only. Unarchive it to allow changes again."
            : "Mark this repository as archived and read-only. You can unarchive it at any time."}
        </p>
        <div className="flex justify-end">
          <Button variant="danger" onClick={handleToggle} disabled={mutation.isPending}>
            {archived ? "Unarchive this repository" : "Archive this repository"}
          </Button>
        </div>
      </div>
    </Box>
  );
}

function DeleteRepoCard({ owner, repo }: { owner: string; repo: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => deleteRepo(owner, repo),
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["repos"] });
      navigate("/ui/");
    },
    onError: (err: Error) => setError(err.message),
  });

  const handleDelete = async () => {
    const confirmed = await confirmAction(
      `This permanently deletes ${owner}/${repo}, including its issues, pull requests, and all data. This cannot be undone.`,
      { title: "Delete this repository?", confirmLabel: "Delete", expectedText: `${owner}/${repo}` },
    );
    if (confirmed) mutation.mutate();
  };

  return (
    <Box header={<span style={{ fontWeight: 600, color: "var(--gh-danger, var(--color-danger-fg))" }}>Delete this repository</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Once you delete a repository, there is no going back. All issues, pull requests, and data are permanently removed.
        </p>
        <div className="flex justify-end">
          <Button variant="danger" onClick={handleDelete} disabled={mutation.isPending}>
            Delete this repository
          </Button>
        </div>
      </div>
    </Box>
  );
}

function RenameBranchTab({ owner, repo }: { owner: string; repo: string }) {
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [branch, setBranch] = useState("");
  const [newName, setNewName] = useState("");

  const branchesQuery = useQuery({
    queryKey: ["repo-branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo,
  });

  const mutation = useMutation({
    mutationFn: () => renameBranch(owner, repo, branch.trim(), newName.trim()),
    onSuccess: () => {
      setError(null);
      setSuccess(`Branch ${branch.trim()} renamed to ${newName.trim()}.`);
      setBranch("");
      setNewName("");
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Rename branch</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {success && <div style={{ color: "var(--gh-open)" }}>{success}</div>}
        <FormLabel id="rename-branch-old">Branch to rename</FormLabel>
        {branchesQuery.isLoading ? (
          <Spinner label="loading branches" />
        ) : (
          <select
            id="rename-branch-old"
            value={branch}
            onChange={(e) => setBranch(e.target.value)}
            className="w-full"
          >
            <option value="">Select branch…</option>
            {(branchesQuery.data ?? []).map((b) => (
              <option key={b.name} value={b.name}>{b.name}</option>
            ))}
          </select>
        )}
        <FormLabel id="rename-branch-new">New name</FormLabel>
        <input
          id="rename-branch-new"
          type="text"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          placeholder="new-branch-name"
          className="w-full"
        />
        <div className="flex justify-end">
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              setSuccess(null);
              mutation.mutate();
            }}
            disabled={mutation.isPending || !branch.trim() || !newName.trim()}
          >
            Rename branch
          </Button>
        </div>
      </div>
    </Box>
  );
}

// ─── GitHub Pages panel ──────────────────────────────────────────────────

function PagesTab({ owner, repo }: { owner: string; repo: string }) {
  const queryClient = useQueryClient();
  const siteQ = useQuery({
    queryKey: ["pages-site", owner, repo],
    queryFn: ({ signal }) => fetchPagesSite(owner, repo, signal),
    enabled: !!owner && !!repo,
  });

  if (siteQ.isLoading) return <Spinner label="loading Pages site" />;
  if (siteQ.isError)
    return <InlineError title="Failed to load Pages site" detail={String(siteQ.error)} />;

  const site = siteQ.data ?? null;
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["pages-site", owner, repo] });
    void queryClient.invalidateQueries({ queryKey: ["pages-builds", owner, repo] });
    void queryClient.invalidateQueries({ queryKey: ["pages-health", owner, repo] });
  };

  if (site === null) return <PagesEnableForm owner={owner} repo={repo} onEnabled={invalidate} />;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <PagesSiteCard owner={owner} repo={repo} site={site} onChanged={invalidate} />
      <PagesBuildsCard
        owner={owner}
        repo={repo}
        buildType={site.build_type === "workflow" ? "workflow" : "legacy"}
      />
      <PagesHealthCard owner={owner} repo={repo} hasCustomDomain={!!site.cname} />
      <PagesDeploymentLookupCard owner={owner} repo={repo} />
    </div>
  );
}

function PagesEnableForm({
  owner,
  repo,
  onEnabled,
}: {
  owner: string;
  repo: string;
  onEnabled: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [branch, setBranch] = useState("");
  const [path, setPath] = useState("/");
  const [buildType, setBuildType] = useState<"legacy" | "workflow">("legacy");
  const branchesQ = useQuery({
    queryKey: ["repo-branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
  });

  const enableMut = useMutation({
    mutationFn: () =>
      createPagesSite(owner, repo, {
        build_type: buildType,
        ...(buildType === "legacy"
          ? { source: { branch: branch.trim(), path: path.trim() || "/" } }
          : branch.trim()
            ? { source: { branch: branch.trim(), path: path.trim() || "/" } }
            : {}),
      }),
    onSuccess: () => {
      setError(null);
      onEnabled();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>GitHub Pages</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Pages is not enabled for this repository.
        </p>
        <FormLabel id="pages-build-type">Build type</FormLabel>
        <select
          id="pages-build-type"
          value={buildType}
          onChange={(e) => setBuildType(e.target.value as "legacy" | "workflow")}
          className="w-full"
        >
          <option value="legacy">Deploy from a branch (legacy)</option>
          <option value="workflow">GitHub Actions workflow</option>
        </select>
        <FormLabel id="pages-source-branch">Source branch</FormLabel>
        <input
          id="pages-source-branch"
          type="text"
          value={branch}
          onChange={(e) => setBranch(e.target.value)}
          placeholder={buildType === "workflow" ? "optional for workflow builds" : "main"}
          list="pages-source-branches"
          className="w-full"
        />
        <datalist id="pages-source-branches">
          {(branchesQ.data ?? []).map((candidate) => (
            <option key={candidate.name} value={candidate.name} />
          ))}
        </datalist>
        {branchesQ.isError && (
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
            Branch suggestions are unavailable; enter a branch name.
          </div>
        )}
        <FormLabel id="pages-source-path">Source path</FormLabel>
        <input
          id="pages-source-path"
          type="text"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          className="w-full"
        />
        <div className="flex justify-end">
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              enableMut.mutate();
            }}
            disabled={enableMut.isPending || (buildType === "legacy" && !branch.trim())}
          >
            Enable Pages
          </Button>
        </div>
      </div>
    </Box>
  );
}

function PagesSiteCard({
  owner,
  repo,
  site,
  onChanged,
}: {
  owner: string;
  repo: string;
  site: GithubPagesSite;
  onChanged: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [cname, setCname] = useState(site.cname);
  const [httpsEnforced, setHttpsEnforced] = useState(site.https_enforced);
  const [buildType, setBuildType] = useState<"legacy" | "workflow">(
    site.build_type === "workflow" ? "workflow" : "legacy",
  );
  const [sourceBranch, setSourceBranch] = useState(site.source?.branch ?? "");
  const [sourcePath, setSourcePath] = useState(site.source?.path ?? "/");
  const [isPublic, setIsPublic] = useState(site.public);

  const updateMut = useMutation({
    mutationFn: () =>
      updatePagesSite(owner, repo, {
        cname: cname.trim() || null,
        https_enforced: httpsEnforced,
        build_type: buildType,
        public: isPublic,
        ...(buildType === "legacy"
          ? { source: { branch: sourceBranch.trim(), path: sourcePath } }
          : {}),
      }),
    onSuccess: () => {
      setError(null);
      setSuccess("Pages settings saved.");
      onChanged();
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  const disableMut = useMutation({
    mutationFn: () => deletePagesSite(owner, repo),
    onSuccess: () => {
      setError(null);
      onChanged();
    },
    onError: (err: Error) => {
      setSuccess(null);
      setError(err.message);
    },
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>GitHub Pages</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {success && <div style={{ color: "var(--gh-open)" }}>{success}</div>}
        <div style={{ fontSize: "0.85rem" }}>
          Status: <span className="font-mono">{site.status}</span>
          {" · "}
          Site:{" "}
          <a href={site.html_url} style={{ color: "var(--color-accent)" }}>
            {site.html_url}
          </a>
        </div>
        <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
          Build type: {site.build_type ?? "legacy"}
          {site.source?.branch ? ` · source: ${site.source.branch} at ${site.source.path ?? "/"}` : ""}
          {" · "}
          {site.public ? "public" : "private"} site
        </div>
        <FormLabel id="pages-cname">Custom domain (CNAME)</FormLabel>
        <input
          id="pages-cname"
          type="text"
          value={cname}
          onChange={(e) => setCname(e.target.value)}
          placeholder="www.example.com"
          className="w-full"
        />
        <FormLabel id="pages-current-build-type">Build and deployment source</FormLabel>
        <select
          id="pages-current-build-type"
          value={buildType}
          onChange={(event) => setBuildType(event.target.value as "legacy" | "workflow")}
          className="w-full"
        >
          <option value="legacy">Deploy from a branch</option>
          <option value="workflow">GitHub Actions workflow</option>
        </select>
        {buildType === "legacy" && (
          <div className="grid gap-3 md:grid-cols-2">
            <div>
              <FormLabel id="pages-current-source-branch">Source branch</FormLabel>
              <input
                id="pages-current-source-branch"
                value={sourceBranch}
                onChange={(event) => setSourceBranch(event.target.value)}
                className="w-full"
              />
            </div>
            <div>
              <FormLabel id="pages-current-source-path">Source path</FormLabel>
              <select
                id="pages-current-source-path"
                value={sourcePath}
                onChange={(event) => setSourcePath(event.target.value)}
                className="w-full"
              >
                <option value="/">/ (repository root)</option>
                <option value="/docs">/docs</option>
              </select>
            </div>
          </div>
        )}
        <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
          <input
            type="checkbox"
            checked={httpsEnforced}
            onChange={(e) => setHttpsEnforced(e.target.checked)}
          />
          Enforce HTTPS
        </label>
        <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", fontSize: "0.85rem" }}>
          <input
            type="checkbox"
            checked={isPublic}
            onChange={(event) => setIsPublic(event.target.checked)}
          />
          Publish this site publicly
        </label>
        <div className="flex justify-end gap-2">
          <Button
            variant="danger"
            onClick={async () => {
              if (await confirmAction("Disable GitHub Pages for this repository?")) {
                setError(null);
                setSuccess(null);
                disableMut.mutate();
              }
            }}
            disabled={disableMut.isPending}
          >
            Disable Pages
          </Button>
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              setSuccess(null);
              updateMut.mutate();
            }}
            disabled={updateMut.isPending}
          >
            Save Pages settings
          </Button>
        </div>
      </div>
    </Box>
  );
}

function PagesBuildsCard({
  owner,
  repo,
  buildType,
}: {
  owner: string;
  repo: string;
  buildType: "legacy" | "workflow";
}) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);

  const buildsQ = useQuery({
    queryKey: ["pages-builds", owner, repo],
    queryFn: () => fetchPagesBuilds(owner, repo),
  });

  const requestMut = useMutation({
    mutationFn: () => requestPagesBuild(owner, repo),
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["pages-builds", owner, repo] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Box
      header={
        <div className="flex w-full items-center justify-between">
          <span style={{ fontWeight: 600 }}>Builds</span>
          {buildType === "legacy" ? (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setError(null);
                requestMut.mutate();
              }}
              disabled={requestMut.isPending}
            >
              Request build
            </Button>
          ) : (
            <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
              Deployments are published by GitHub Actions
            </span>
          )}
        </div>
      }
    >
      <div style={{ padding: "0" }}>
        {error && (
          <div style={{ padding: "0.75rem 1rem" }}>
            <ErrorBanner>{error}</ErrorBanner>
          </div>
        )}
        {buildsQ.isLoading ? (
          <div style={{ padding: "1rem" }}>
            <Spinner label="loading builds" />
          </div>
        ) : buildsQ.isError ? (
          <div style={{ padding: "1rem" }}>
            <InlineError title="Failed to load Pages builds" detail={String(buildsQ.error)} />
          </div>
        ) : (buildsQ.data ?? []).length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No builds yet.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {(buildsQ.data ?? []).map((b: GithubPagesBuild, i: number) => (
              <li
                key={b.url}
                className="flex items-center gap-3"
                style={{
                  padding: "0.6rem 1rem",
                  borderBottom:
                    i < (buildsQ.data ?? []).length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <span
                  className="font-mono"
                  style={{
                    fontSize: "0.74rem",
                    color: b.status === "built" ? "var(--gh-open)" : "var(--color-fg-muted)",
                    border: "1px solid var(--color-border)",
                    borderRadius: "999px",
                    padding: "0.05rem 0.5rem",
                    flexShrink: 0,
                  }}
                >
                  {b.status}
                </span>
                <div className="min-w-0 flex-1" style={{ fontSize: "0.8rem" }}>
                  <span className="font-mono">{b.commit.slice(0, 7)}</span>
                  {b.pusher ? ` · by ${b.pusher.login}` : ""} ·{" "}
                  <RelativeTime iso={b.created_at} />
                  {b.error?.message ? (
                    <span style={{ color: "var(--color-danger-fg)" }}> · {b.error.message}</span>
                  ) : null}
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Box>
  );
}

function PagesHealthCard({
  owner,
  repo,
  hasCustomDomain,
}: {
  owner: string;
  repo: string;
  hasCustomDomain: boolean;
}) {
  const healthQ = useQuery({
    queryKey: ["pages-health", owner, repo],
    queryFn: ({ signal }) => fetchPagesHealth(owner, repo, signal),
    enabled: hasCustomDomain,
    retry: false,
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Custom domain health check</span>}>
      <div style={{ padding: "1rem", fontSize: "0.85rem" }}>
        {!hasCustomDomain ? (
          <span style={{ color: "var(--color-fg-muted)" }}>
            No custom domain configured — set a CNAME above to run the health check.
          </span>
        ) : healthQ.isLoading ? (
          <Spinner label="running health check" />
        ) : healthQ.isError ? (
          <InlineError title="Health check failed" detail={String(healthQ.error)} />
        ) : healthQ.data?.domain == null ? (
          <span style={{ color: "var(--color-fg-muted)" }}>No domain checks reported.</span>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: "0.3rem" }}>
            <div>
              <span className="font-mono">{healthQ.data.domain.host}</span> —{" "}
              {healthQ.data.domain.is_valid ? (
                <span style={{ color: "var(--gh-open)" }}>healthy</span>
              ) : (
                <span style={{ color: "var(--color-danger-fg)" }}>
                  unhealthy{healthQ.data.domain.reason ? ` (${healthQ.data.domain.reason})` : ""}
                </span>
              )}
            </div>
            <ul style={{ listStyle: "none", margin: 0, padding: 0, color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>
              <li>DNS resolves: {healthQ.data.domain.dns_resolves ? "yes" : "no"}</li>
              <li>Valid domain: {healthQ.data.domain.is_valid_domain ? "yes" : "no"}</li>
              <li>Apex domain: {healthQ.data.domain.is_apex_domain ? "yes" : "no"}</li>
              <li>Pages domain: {healthQ.data.domain.is_pages_domain ? "yes" : "no"}</li>
              <li>Enforces HTTPS: {healthQ.data.domain.enforces_https ? "yes" : "no"}</li>
            </ul>
          </div>
        )}
      </div>
    </Box>
  );
}

function PagesDeploymentLookupCard({ owner, repo }: { owner: string; repo: string }) {
  const [error, setError] = useState<string | null>(null);
  const [deploymentId, setDeploymentId] = useState("");
  const [result, setResult] = useState<{ id: number; status: string } | null>(null);

  const lookupMut = useMutation({
    mutationFn: (id: number) => fetchPagesDeploymentStatus(owner, repo, id),
    onSuccess: (data, id) => {
      setError(null);
      setResult({ id, status: data.status });
    },
    onError: (err: Error) => {
      setResult(null);
      setError(err.message);
    },
  });

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Pages deployment status</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
          Look up the status of a Pages deployment created via POST /pages/deployments
          (e.g. by the actions/deploy-pages workflow step).
        </p>
        <FormLabel id="pages-deployment-id">Deployment ID</FormLabel>
        <div className="flex gap-2">
          <input
            id="pages-deployment-id"
            type="text"
            inputMode="numeric"
            value={deploymentId}
            onChange={(e) => setDeploymentId(e.target.value)}
            placeholder="e.g. 1"
            style={{ flex: 1 }}
          />
          <Button
            variant="secondary"
            onClick={() => {
              const id = parseInt(deploymentId.trim(), 10);
              if (Number.isNaN(id)) {
                setResult(null);
                setError("Deployment ID must be a number.");
                return;
              }
              setError(null);
              lookupMut.mutate(id);
            }}
            disabled={lookupMut.isPending || !deploymentId.trim()}
          >
            Check status
          </Button>
        </div>
        {result && (
          <div style={{ fontSize: "0.85rem" }}>
            Deployment <span className="font-mono">#{result.id}</span>:{" "}
            <span className="font-mono">{result.status}</span>
          </div>
        )}
      </div>
    </Box>
  );
}

const settingsInputStyle: CSSProperties = {
  padding: "0.4rem 0.6rem",
  fontSize: "0.85rem",
  borderRadius: "var(--radius-md)",
  border: "1px solid var(--color-border)",
  background: "var(--color-surface)",
  color: "var(--color-fg)",
};

const settingsH2: CSSProperties = { fontSize: "1.1rem", fontWeight: 600 };

// Repo Actions fork-PR approval + artifact/log retention. These wrappers live in
// this lazily-loaded page (not entry-resident api.ts) to keep the entry bundle
// under budget, per the ghFetch/ghSend lazy-wrapper pattern.
interface ForkPRApproval { approval_policy: string }
interface ArtifactRetention { days: number; maximum_allowed_days: number }
const FORK_PR_POLICIES = [
  { value: "first_time_contributors_new_to_github", label: "Require approval for first-time contributors who are new to GitHub" },
  { value: "first_time_contributors", label: "Require approval for first-time contributors" },
  { value: "all_external_contributors", label: "Require approval for all outside collaborators" },
] as const;
const actionsPermBase = (owner: string, repo: string) =>
  `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/permissions`;
const fetchForkPRApproval = (owner: string, repo: string) =>
  ghFetch<ForkPRApproval>(`${actionsPermBase(owner, repo)}/fork-pr-contributor-approval`);
const updateForkPRApproval = (owner: string, repo: string, approval_policy: string) =>
  ghSend("PUT", `${actionsPermBase(owner, repo)}/fork-pr-contributor-approval`, { approval_policy });
const fetchArtifactRetention = (owner: string, repo: string) =>
  ghFetch<ArtifactRetention>(`${actionsPermBase(owner, repo)}/artifact-and-log-retention`);
const updateArtifactRetention = (owner: string, repo: string, days: number) =>
  ghSend("PUT", `${actionsPermBase(owner, repo)}/artifact-and-log-retention`, { days });

// Allow-list backing the "Allow select actions and reusable workflows" radio.
interface SelectedActions { github_owned_allowed: boolean; verified_allowed: boolean; patterns_allowed: string[] }
const fetchSelectedActions = (owner: string, repo: string) =>
  ghFetch<SelectedActions>(`${actionsPermBase(owner, repo)}/selected-actions`);
const updateSelectedActions = (owner: string, repo: string, body: SelectedActions) =>
  ghSend("PUT", `${actionsPermBase(owner, repo)}/selected-actions`, body);

// Which other repositories may consume this repo's actions/reusable workflows.
interface ActionsAccess { access_level: string }
const ACTIONS_ACCESS_LEVELS = [
  { value: "none", label: "Not accessible outside the repository" },
  { value: "organization", label: "Accessible from repositories in the organization" },
  { value: "enterprise", label: "Accessible from repositories in the enterprise" },
] as const;
const fetchActionsAccess = (owner: string, repo: string) =>
  ghFetch<ActionsAccess>(`${actionsPermBase(owner, repo)}/access`);
const updateActionsAccess = (owner: string, repo: string, access_level: string) =>
  ghSend("PUT", `${actionsPermBase(owner, repo)}/access`, { access_level });

// Fork PR workflow controls for private repositories (all server-side optional).
interface ForkPRWorkflowsPrivate {
  run_workflows_from_fork_pull_requests?: boolean;
  send_write_tokens_to_workflows?: boolean;
  send_secrets_and_variables?: boolean;
  require_approval_for_fork_pr_workflows?: boolean;
}
const fetchForkPRWorkflowsPrivate = (owner: string, repo: string) =>
  ghFetch<ForkPRWorkflowsPrivate>(`${actionsPermBase(owner, repo)}/fork-pr-workflows-private-repos`);
const updateForkPRWorkflowsPrivate = (owner: string, repo: string, body: ForkPRWorkflowsPrivate) =>
  ghSend("PUT", `${actionsPermBase(owner, repo)}/fork-pr-workflows-private-repos`, body);

// Immutable releases: GET 404s when disabled; PUT enables, DELETE disables.
interface ImmutableReleases { enabled: boolean; enforced_by_owner?: boolean }
const immutableReleasesPath = (owner: string, repo: string) =>
  `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/immutable-releases`;
const fetchImmutableReleases = async (owner: string, repo: string): Promise<ImmutableReleases> => {
  try {
    return await ghFetch<ImmutableReleases>(immutableReleasesPath(owner, repo));
  } catch (err) {
    if (isNotFound(err)) return { enabled: false };
    throw err;
  }
};
const enableImmutableReleases = (owner: string, repo: string) =>
  ghSend("PUT", immutableReleasesPath(owner, repo));
const disableImmutableReleases = (owner: string, repo: string) =>
  ghSend("DELETE", immutableReleasesPath(owner, repo));

// ─── Actions settings ──────────────────────────────────────────────────────
function ActionsSettingsTab({ owner, repo }: { owner: string; repo: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const perms = useQuery({ queryKey: ["actions-permissions", owner, repo], queryFn: () => fetchActionsPermissions(owner, repo) });
  const wf = useQuery({ queryKey: ["workflow-permissions", owner, repo], queryFn: () => fetchWorkflowPermissions(owner, repo) });
  const forkApproval = useQuery({ queryKey: ["fork-pr-approval", owner, repo], queryFn: () => fetchForkPRApproval(owner, repo) });
  const retention = useQuery({ queryKey: ["artifact-retention", owner, repo], queryFn: () => fetchArtifactRetention(owner, repo) });
  const [retentionDays, setRetentionDays] = useState<number | null>(null);
  useEffect(() => { if (retention.data) setRetentionDays(retention.data.days); }, [retention.data]);
  const forkMut = useMutation({
    mutationFn: (policy: string) => updateForkPRApproval(owner, repo, policy),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["fork-pr-approval", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });
  const retentionMut = useMutation({
    mutationFn: (days: number) => updateArtifactRetention(owner, repo, days),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["artifact-retention", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });
  const permMut = useMutation({
    mutationFn: (body: GithubActionsPermissions) => updateActionsPermissions(owner, repo, body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["actions-permissions", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });
  const wfMut = useMutation({
    mutationFn: (body: GithubWorkflowPermissions) => updateWorkflowPermissions(owner, repo, body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["workflow-permissions", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });

  // Allow-list editor for the "selected" actions policy.
  const isSelected = perms.data?.allowed_actions === "selected";
  const selectedActions = useQuery({
    queryKey: ["selected-actions", owner, repo],
    queryFn: () => fetchSelectedActions(owner, repo),
    enabled: isSelected,
  });
  const [githubOwned, setGithubOwned] = useState(false);
  const [verifiedAllowed, setVerifiedAllowed] = useState(false);
  const [patternsText, setPatternsText] = useState("");
  useEffect(() => {
    if (selectedActions.data) {
      setGithubOwned(selectedActions.data.github_owned_allowed);
      setVerifiedAllowed(selectedActions.data.verified_allowed);
      setPatternsText((selectedActions.data.patterns_allowed ?? []).join("\n"));
    }
  }, [selectedActions.data]);
  const selectedMut = useMutation({
    mutationFn: (body: SelectedActions) => updateSelectedActions(owner, repo, body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["selected-actions", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });

  const access = useQuery({ queryKey: ["actions-access", owner, repo], queryFn: () => fetchActionsAccess(owner, repo) });
  const accessMut = useMutation({
    mutationFn: (access_level: string) => updateActionsAccess(owner, repo, access_level),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["actions-access", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });

  const forkPrivate = useQuery({ queryKey: ["fork-pr-workflows-private", owner, repo], queryFn: () => fetchForkPRWorkflowsPrivate(owner, repo) });
  const forkPrivateMut = useMutation({
    mutationFn: (body: ForkPRWorkflowsPrivate) => updateForkPRWorkflowsPrivate(owner, repo, body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["fork-pr-workflows-private", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });

  const immutable = useQuery({ queryKey: ["immutable-releases", owner, repo], queryFn: () => fetchImmutableReleases(owner, repo) });
  const immutableMut = useMutation({
    mutationFn: (enabled: boolean) => (enabled ? enableImmutableReleases(owner, repo) : disableImmutableReleases(owner, repo)),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["immutable-releases", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });

  const PERM_OPTIONS = [
    { enabled: true, allowed: "all" as const, label: "Allow all actions and reusable workflows" },
    { enabled: true, allowed: "local_only" as const, label: "Allow local actions only" },
    { enabled: true, allowed: "selected" as const, label: "Allow select actions and reusable workflows" },
    { enabled: false, allowed: undefined, label: "Disable actions" },
  ];

  return (
    <section style={{ display: "flex", flexDirection: "column", gap: "1.5rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div>
        <h2 style={settingsH2}>Actions permissions</h2>
        {perms.isLoading && <Spinner label="loading actions permissions" />}
        {perms.isError && <InlineError title="Failed to load Actions permissions" />}
        {perms.data && (
          <Box>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
              {PERM_OPTIONS.map((opt) => {
                const checked = opt.enabled === perms.data!.enabled && (!opt.enabled || opt.allowed === (perms.data!.allowed_actions ?? "all"));
                return (
                  <label key={opt.label} style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                    <input type="radio" name="actions-perm" checked={checked} disabled={permMut.isPending}
                      onChange={() => permMut.mutate(opt.enabled ? { enabled: true, allowed_actions: opt.allowed ?? "all" } : { enabled: false })} />
                    {opt.label}
                  </label>
                );
              })}
            </div>
          </Box>
        )}
        {isSelected && (
          <div style={{ marginTop: "0.6rem" }}>
            {selectedActions.isLoading && <Spinner label="loading allowed actions" />}
            {selectedActions.isError && <InlineError title="Failed to load allowed actions" />}
            {selectedActions.data && (
              <Box>
                <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
                  <label style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                    <input type="checkbox" checked={githubOwned} disabled={selectedMut.isPending}
                      onChange={(e) => setGithubOwned(e.target.checked)} />
                    Allow actions created by GitHub
                  </label>
                  <label style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                    <input type="checkbox" checked={verifiedAllowed} disabled={selectedMut.isPending}
                      onChange={(e) => setVerifiedAllowed(e.target.checked)} />
                    Allow actions by Marketplace verified creators
                  </label>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.4rem", fontSize: "0.85rem" }}>
                    <span>Allowed patterns (one per line)</span>
                    <textarea aria-label="Allowed action patterns" rows={4} value={patternsText} disabled={selectedMut.isPending}
                      onChange={(e) => setPatternsText(e.target.value)}
                      style={{ ...settingsInputStyle, width: "100%", maxWidth: "34rem", fontFamily: "var(--font-mono)" }} />
                  </label>
                  <div>
                    <Button type="button" variant="secondary" disabled={selectedMut.isPending}
                      onClick={() => selectedMut.mutate({
                        github_owned_allowed: githubOwned,
                        verified_allowed: verifiedAllowed,
                        patterns_allowed: patternsText.split("\n").map((p) => p.trim()).filter(Boolean),
                      })}>Save</Button>
                  </div>
                </div>
              </Box>
            )}
          </div>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Workflow permissions</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          Default permissions granted to the <code>GITHUB_TOKEN</code> when running workflows in this repository.
        </p>
        {wf.isLoading && <Spinner label="loading workflow permissions" />}
        {wf.isError && <InlineError title="Failed to load workflow permissions" detail={String(wf.error)} />}
        {wf.data && (
          <Box>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
              {(["read", "write"] as const).map((v) => (
                <label key={v} style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                  <input type="radio" name="wf-perm" checked={wf.data!.default_workflow_permissions === v} disabled={wfMut.isPending}
                    onChange={() => wfMut.mutate({ default_workflow_permissions: v, can_approve_pull_request_reviews: wf.data!.can_approve_pull_request_reviews })} />
                  {v === "read" ? "Read repository contents and packages permissions" : "Read and write permissions"}
                </label>
              ))}
              <label style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center", marginTop: "0.4rem" }}>
                <input type="checkbox" checked={wf.data!.can_approve_pull_request_reviews ?? false} disabled={wfMut.isPending}
                  onChange={(e) => wfMut.mutate({ default_workflow_permissions: wf.data!.default_workflow_permissions, can_approve_pull_request_reviews: e.target.checked })} />
                Allow GitHub Actions to create and approve pull requests
              </label>
            </div>
          </Box>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Fork pull request workflows from outside collaborators</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          Choose which contributors need a maintainer to approve running workflows on their fork pull requests.
        </p>
        {forkApproval.isLoading && <Spinner label="loading fork PR approval policy" />}
        {forkApproval.isError && <InlineError title="Failed to load fork PR approval policy" />}
        {forkApproval.data && (
          <Box>
            <div style={{ padding: "1rem" }}>
              <select aria-label="Fork pull request approval policy" value={forkApproval.data.approval_policy}
                disabled={forkMut.isPending} onChange={(e) => forkMut.mutate(e.target.value)} style={{ ...settingsInputStyle, width: "100%", maxWidth: "34rem" }}>
                {FORK_PR_POLICIES.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
              </select>
            </div>
          </Box>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Artifact and log retention</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          The number of days to retain artifacts and logs produced by workflows in this repository.
        </p>
        {retention.isLoading && <Spinner label="loading artifact and log retention" />}
        {retention.isError && <InlineError title="Failed to load artifact and log retention" />}
        {retention.data && (
          <Box>
            <div style={{ padding: "1rem", display: "flex", alignItems: "flex-end", gap: "0.6rem" }}>
              <label style={{ display: "flex", flexDirection: "column", gap: "0.4rem", fontSize: "0.85rem" }}>
                <span>Days (1–{retention.data.maximum_allowed_days})</span>
                <input type="number" aria-label="Artifact and log retention days" min={1} max={retention.data.maximum_allowed_days}
                  value={retentionDays ?? retention.data.days} disabled={retentionMut.isPending}
                  onChange={(e) => setRetentionDays(Number(e.target.value))} style={{ ...settingsInputStyle, width: "8rem" }} />
              </label>
              <Button type="button" variant="secondary"
                disabled={retentionMut.isPending || retentionDays === null || retentionDays === retention.data.days || retentionDays < 1 || retentionDays > retention.data.maximum_allowed_days}
                onClick={() => { if (retentionDays !== null) retentionMut.mutate(retentionDays); }}>Save</Button>
            </div>
          </Box>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Actions access</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          Choose which other repositories may use the actions and reusable workflows in this repository.
        </p>
        {access.isLoading && <Spinner label="loading actions access level" />}
        {access.isError && <InlineError title="Failed to load actions access level" />}
        {access.data && (
          <Box>
            <div style={{ padding: "1rem" }}>
              <select aria-label="Actions access level" value={access.data.access_level}
                disabled={accessMut.isPending} onChange={(e) => accessMut.mutate(e.target.value)}
                style={{ ...settingsInputStyle, width: "100%", maxWidth: "34rem" }}>
                {ACTIONS_ACCESS_LEVELS.map((l) => <option key={l.value} value={l.value}>{l.label}</option>)}
              </select>
            </div>
          </Box>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Fork pull request workflows in private repositories</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          Control how workflows run for pull requests from forks of this private repository.
        </p>
        {forkPrivate.isLoading && <Spinner label="loading fork PR workflow settings" />}
        {forkPrivate.isError && <InlineError title="Failed to load fork PR workflow settings" />}
        {forkPrivate.data && (
          <Box>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
              {([
                ["run_workflows_from_fork_pull_requests", "Run workflows from fork pull requests"],
                ["send_write_tokens_to_workflows", "Send write tokens to workflows from fork pull requests"],
                ["send_secrets_and_variables", "Send secrets and variables to workflows from fork pull requests"],
                ["require_approval_for_fork_pr_workflows", "Require approval for fork pull request workflows"],
              ] as const).map(([key, label]) => (
                <label key={key} style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                  <input type="checkbox" checked={forkPrivate.data![key] ?? false} disabled={forkPrivateMut.isPending}
                    onChange={(e) => forkPrivateMut.mutate({ ...forkPrivate.data, [key]: e.target.checked })} />
                  {label}
                </label>
              ))}
            </div>
          </Box>
        )}
      </div>
      <div>
        <h2 style={settingsH2}>Immutable releases</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: "0.3rem 0 0.5rem" }}>
          Prevent published releases and their assets from being modified after they are created.
        </p>
        {immutable.isLoading && <Spinner label="loading immutable releases setting" />}
        {immutable.isError && <InlineError title="Failed to load immutable releases setting" />}
        {immutable.data && (
          <Box>
            <div style={{ padding: "1rem" }}>
              <label style={{ display: "flex", gap: "0.5rem", fontSize: "0.9rem", alignItems: "center" }}>
                <input type="checkbox" checked={immutable.data.enabled} disabled={immutableMut.isPending}
                  onChange={(e) => immutableMut.mutate(e.target.checked)} />
                Enable immutable releases
              </label>
            </div>
          </Box>
        )}
      </div>
    </section>
  );
}

// ─── repo Custom properties ─────────────────────────────────────────────────
interface RepoCustomPropertyValue {
  property_name: string;
  value: string | string[] | null;
}

// Repo Settings › Custom properties: set values for the org-defined property
// schema. Values are authored per value_type and saved via
// PATCH /repos/{owner}/{repo}/properties/values {properties:[…]}.
function CustomPropertiesTab({ owner, repo }: { owner: string; repo: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [edits, setEdits] = useState<Record<string, string | string[] | null>>({});
  const schema = useQuery({
    queryKey: ["org-custom-properties", owner],
    queryFn: () => fetchOrgCustomProperties(owner),
    retry: false,
  });
  const values = useQuery({
    queryKey: ["repo-custom-property-values", owner, repo],
    queryFn: () => ghFetch<RepoCustomPropertyValue[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/properties/values`),
    retry: false,
  });
  const save = useMutation({
    mutationFn: (properties: RepoCustomPropertyValue[]) =>
      ghSend("PATCH", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/properties/values`, { properties }),
    onSuccess: () => {
      setError(null);
      setEdits({});
      void qc.invalidateQueries({ queryKey: ["repo-custom-property-values", owner, repo] });
    },
    onError: (e: Error) => setError(e.message),
  });

  if (schema.isLoading || values.isLoading) return <Spinner label="loading custom properties" />;
  // Custom properties are an organization feature; a user-owned repo (or an org
  // with no schema) simply has none to set.
  const props: GithubCustomProperty[] = schema.data ?? [];
  if (schema.isError || props.length === 0) {
    return (
      <section style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
        <h2 style={settingsH2}>Custom properties</h2>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          No custom properties are defined for this repository&apos;s organization.
        </p>
      </section>
    );
  }

  const currentValue = (name: string): string | string[] | null => {
    if (name in edits) return edits[name]!;
    const found = (values.data ?? []).find((v) => v.property_name === name);
    return found ? found.value : null;
  };
  const setValue = (name: string, value: string | string[] | null) => setEdits((prev) => ({ ...prev, [name]: value }));

  const submit = () => {
    const properties: RepoCustomPropertyValue[] = props.map((p) => ({
      property_name: p.property_name,
      value: currentValue(p.property_name),
    }));
    save.mutate(properties);
  };

  return (
    <section style={{ display: "flex", flexDirection: "column", gap: "1rem", maxWidth: "44rem" }}>
      <h2 style={settingsH2}>Custom properties</h2>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {props.map((p) => {
        const val = currentValue(p.property_name);
        const allowed = (p.allowed_values ?? []) as string[];
        return (
          <div key={p.property_name}>
            <FormLabel id={`cp-${p.property_name}`}>
              {p.property_name}{p.required ? " *" : ""}
            </FormLabel>
            {p.description && <p style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", margin: "0.1rem 0 0.3rem" }}>{p.description}</p>}
            {p.value_type === "true_false" ? (
              <select id={`cp-${p.property_name}`} aria-label={p.property_name} value={typeof val === "string" ? val : ""}
                onChange={(e) => setValue(p.property_name, e.target.value || null)} style={settingsInputStyle}>
                <option value="">— unset —</option>
                <option value="true">true</option>
                <option value="false">false</option>
              </select>
            ) : p.value_type === "single_select" ? (
              <select id={`cp-${p.property_name}`} aria-label={p.property_name} value={typeof val === "string" ? val : ""}
                onChange={(e) => setValue(p.property_name, e.target.value || null)} style={settingsInputStyle}>
                <option value="">— unset —</option>
                {allowed.map((a) => <option key={a} value={a}>{a}</option>)}
              </select>
            ) : p.value_type === "multi_select" ? (
              <div style={{ display: "flex", flexWrap: "wrap", gap: "0.75rem" }}>
                {allowed.map((a) => {
                  const arr = Array.isArray(val) ? val : [];
                  return (
                    <label key={a} style={{ display: "flex", alignItems: "center", gap: "0.35rem", fontSize: "0.85rem", minHeight: "1.625rem" }}>
                      <input type="checkbox" aria-label={`${p.property_name}: ${a}`} checked={arr.includes(a)}
                        onChange={(e) => setValue(p.property_name, e.target.checked ? [...arr, a] : arr.filter((x) => x !== a))} />
                      {a}
                    </label>
                  );
                })}
              </div>
            ) : (
              <input id={`cp-${p.property_name}`} aria-label={p.property_name} type={p.value_type === "url" ? "url" : "text"}
                value={typeof val === "string" ? val : ""} onChange={(e) => setValue(p.property_name, e.target.value || null)}
                style={settingsInputStyle} />
            )}
          </div>
        );
      })}
      <div>
        <Button variant="primary" disabled={save.isPending || Object.keys(edits).length === 0} onClick={submit}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </section>
  );
}

// ─── repo Rulesets ─────────────────────────────────────────────────────────
function RepoRulesetsTab({ owner, repo }: { owner: string; repo: string }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [target, setTarget] = useState<GithubRulesetTarget>("branch");
  const [enforcement, setEnforcement] = useState<GithubRulesetEnforcement>("active");
  const [ruleConfig, setRuleConfig] = useState<RulesetRuleConfig>({ rules: [], bypass_actors: [] });
  const [error, setError] = useState<string | null>(null);
  const list = useQuery({ queryKey: ["repo-rulesets", owner, repo], queryFn: () => fetchRepoRulesets(owner, repo) });
  // Team picker data for bypass actors; 404s for user-owned repos (no org).
  const teamsQ = useQuery({ queryKey: ["org-teams", owner], queryFn: () => fetchOrgTeams(owner), retry: false });
  const createMut = useMutation({
    mutationFn: () => createRepoRuleset(owner, repo, {
      name: name.trim(),
      target,
      enforcement,
      rules: ruleConfig.rules,
      ...(ruleConfig.conditions ? { conditions: ruleConfig.conditions } : {}),
      bypass_actors: ruleConfig.bypass_actors,
    }),
    onSuccess: () => { setName(""); setError(null); void qc.invalidateQueries({ queryKey: ["repo-rulesets", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });
  const delMut = useMutation({
    mutationFn: (id: number) => deleteRepoRuleset(owner, repo, id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["repo-rulesets", owner, repo] }),
    onError: (e: Error) => setError(e.message),
  });
  const rulesets: GithubRuleset[] = list.data ?? [];
  return (
    <section style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <h2 style={settingsH2}>Rulesets</h2>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>New ruleset</span>}>
        <form onSubmit={(e: FormEvent) => { e.preventDefault(); if (name.trim()) createMut.mutate(); }}
          style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
          <div style={{ display: "flex", flexWrap: "wrap", gap: "0.6rem", alignItems: "end" }}>
            <div><FormLabel id="ruleset-name">Name</FormLabel><input id="ruleset-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Ruleset name" style={settingsInputStyle} /></div>
            <div><FormLabel id="ruleset-target">Target</FormLabel><select id="ruleset-target" value={target} onChange={(e) => setTarget(e.target.value as GithubRulesetTarget)} style={settingsInputStyle}><option value="branch">Branch</option><option value="tag">Tag</option><option value="push">Push</option></select></div>
            <div><FormLabel id="ruleset-enf">Enforcement</FormLabel><select id="ruleset-enf" value={enforcement} onChange={(e) => setEnforcement(e.target.value as GithubRulesetEnforcement)} style={settingsInputStyle}><option value="active">Active</option><option value="evaluate">Evaluate</option><option value="disabled">Disabled</option></select></div>
          </div>
          <RulesetEditor target={target} onChange={setRuleConfig} teams={teamsQ.data ?? []} />
          <div><Button type="submit" variant="primary" size="sm" disabled={!name.trim() || createMut.isPending}>{createMut.isPending ? "Creating…" : "Create ruleset"}</Button></div>
        </form>
      </Box>
      {list.isLoading && <Spinner label="loading rulesets" />}
      {list.isError && <InlineError title="Failed to load rulesets" />}
      {list.data && (rulesets.length === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No rulesets yet.</p>
      ) : (
        <Box>
          {rulesets.map((rs, i) => (
            <div key={rs.id} style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "0.7rem 1rem", borderBottom: i < rulesets.length - 1 ? "1px solid var(--color-border)" : "none" }}>
              <span style={{ fontSize: "0.9rem" }}><strong>{rs.name}</strong> <span style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>{rs.target} · {rs.enforcement}</span></span>
              <Button size="sm" variant="danger" aria-label={`Delete ruleset ${rs.name}`} disabled={delMut.isPending}
                onClick={async () => { if (await confirmAction(`Delete ruleset "${rs.name}"?`, { title: "Delete ruleset", confirmLabel: "Delete" })) delMut.mutate(rs.id); }}>Delete</Button>
            </div>
          ))}
        </Box>
      ))}
    </section>
  );
}

// ─── repo Environments ─────────────────────────────────────────────────────
function EnvironmentsTab({ owner, repo }: { owner: string; repo: string }) {
  const qc = useQueryClient();
  const [newEnv, setNewEnv] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [openEnv, setOpenEnv] = useState<string | null>(null);
  const list = useQuery({ queryKey: ["environments", owner, repo], queryFn: () => fetchEnvironments(owner, repo) });
  const createMut = useMutation({
    mutationFn: () => createEnvironment(owner, repo, newEnv.trim()),
    onSuccess: () => { setNewEnv(""); setError(null); void qc.invalidateQueries({ queryKey: ["environments", owner, repo] }); },
    onError: (e: Error) => setError(e.message),
  });
  const delMut = useMutation({
    mutationFn: (name: string) => deleteEnvironment(owner, repo, name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["environments", owner, repo] }),
    onError: (e: Error) => setError(e.message),
  });
  const envs: GithubEnvironment[] = list.data ?? [];
  return (
    <section style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <h2 style={settingsH2}>Environments</h2>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>New environment</span>}>
        <form onSubmit={(e: FormEvent) => { e.preventDefault(); if (newEnv.trim()) createMut.mutate(); }}
          style={{ padding: "1rem", display: "flex", gap: "0.6rem", alignItems: "end" }}>
          <div style={{ flex: 1 }}><FormLabel id="env-name">Name</FormLabel><input id="env-name" value={newEnv} onChange={(e) => setNewEnv(e.target.value)} placeholder="e.g. production" style={settingsInputStyle} /></div>
          <Button type="submit" variant="primary" size="sm" disabled={!newEnv.trim() || createMut.isPending}>{createMut.isPending ? "Creating…" : "Configure environment"}</Button>
        </form>
      </Box>
      {list.isLoading && <Spinner label="loading environments" />}
      {list.isError && <InlineError title="Failed to load environments" />}
      {list.data && (envs.length === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No environments yet.</p>
      ) : (
        <Box>
          {envs.map((env, i) => (
            <div key={env.id} style={{ borderBottom: i < envs.length - 1 ? "1px solid var(--color-border)" : "none" }}>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "0.7rem 1rem" }}>
                <button type="button" onClick={() => setOpenEnv(openEnv === env.name ? null : env.name)} aria-expanded={openEnv === env.name}
                  style={{ background: "transparent", border: 0, color: "var(--color-accent)", fontSize: "0.9rem", fontWeight: 600, cursor: "pointer", minHeight: "1.625rem" }}>
                  {env.name}
                </button>
                <Button size="sm" variant="danger" aria-label={`Delete environment ${env.name}`} disabled={delMut.isPending}
                  onClick={async () => { if (await confirmAction(`Delete environment "${env.name}"?`, { title: "Delete environment", confirmLabel: "Delete" })) delMut.mutate(env.name); }}>Delete</Button>
              </div>
              {openEnv === env.name && <EnvironmentDetail owner={owner} repo={repo} env={env.name} />}
            </div>
          ))}
        </Box>
      ))}
    </section>
  );
}

// One entry in the environment's required-reviewers PUT payload, plus a label
// for display. The GET response resolves each reviewer to its user/team
// object (id + login for users, id + slug/name for teams), so both kinds
// round-trip their ids on save.
interface EnvReviewerDraft {
  type: "User" | "Team";
  id: number;
  label: string;
  /** Team reviewers only: the team slug, shown alongside the name. */
  slug?: string;
}

function EnvironmentDetail({ owner, repo, env }: { owner: string; repo: string; env: string }) {
  const qc = useQueryClient();
  const [vname, setVName] = useState("");
  const [vval, setVVal] = useState("");
  const detailQ = useQuery({
    queryKey: ["environments-detail", owner, repo],
    queryFn: () => fetchEnvironmentsDetail(owner, repo),
  });
  const thisEnv = (detailQ.data ?? []).find((e) => e.name === env);
  const currentWait = thisEnv?.protection_rules?.find((r) => r.wait_timer != null)?.wait_timer ?? 0;
  const [waitTimer, setWaitTimer] = useState<string>("");
  useEffect(() => setWaitTimer(String(currentWait)), [currentWait]);

  // ── required reviewers ────────────────────────────────────────────────────
  const reviewersRule = thisEnv?.protection_rules?.find((r) => r.type === "required_reviewers");
  const [reviewers, setReviewers] = useState<EnvReviewerDraft[]>([]);
  useEffect(() => {
    const drafts: EnvReviewerDraft[] = [];
    for (const entry of reviewersRule?.reviewers ?? []) {
      // The typed reviewer only declares login; the payload carries the full
      // simple-user | team union (id, and slug/name for teams).
      const reviewer = entry.reviewer as
        | { id?: number; login?: string; slug?: string; name?: string }
        | undefined;
      if (reviewer?.id == null) continue;
      if (entry.type === "Team") {
        drafts.push({
          type: "Team",
          id: reviewer.id,
          label: reviewer.name ?? reviewer.slug ?? `#${reviewer.id}`,
          ...(reviewer.slug ? { slug: reviewer.slug } : {}),
        });
      } else {
        drafts.push({ type: "User", id: reviewer.id, label: reviewer.login ?? `#${reviewer.id}` });
      }
    }
    setReviewers(drafts);
    // Re-seed when the rule content (not the array identity) changes.
  }, [JSON.stringify(reviewersRule?.reviewers ?? [])]);

  const collaboratorsQ = useQuery({
    queryKey: ["repo-collaborators", owner, repo],
    queryFn: () => fetchRepoCollaborators(owner, repo),
  });
  // Team pickers only make sense for org-owned repos; a user owner 404s.
  const teamsQ = useQuery({
    queryKey: ["org-teams", owner],
    queryFn: () => fetchOrgTeams(owner),
    retry: false,
  });
  const [pickUser, setPickUser] = useState("");
  const [pickTeam, setPickTeam] = useState("");

  const addReviewer = (draft: EnvReviewerDraft) =>
    setReviewers((prev) =>
      prev.some((r) => r.type === draft.type && r.id === draft.id) ? prev : [...prev, draft],
    );

  const saveWait = useMutation({
    mutationFn: () =>
      putEnvironment(owner, repo, env, {
        wait_timer: Number(waitTimer) || 0,
        reviewers: reviewers.map((r) => ({ type: r.type, id: r.id })),
      }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["environments-detail", owner, repo] }),
  });
  const vars = useQuery({ queryKey: ["env-vars", owner, repo, env], queryFn: () => fetchEnvVariables(owner, repo, env) });
  const secrets = useQuery({ queryKey: ["env-secrets", owner, repo, env], queryFn: () => fetchEnvSecrets(owner, repo, env) });
  const addVar = useMutation({ mutationFn: () => createEnvVariable(owner, repo, env, vname.trim(), vval), onSuccess: () => { setVName(""); setVVal(""); void qc.invalidateQueries({ queryKey: ["env-vars", owner, repo, env] }); } });
  const delVar = useMutation({ mutationFn: (n: string) => deleteEnvVariable(owner, repo, env, n), onSuccess: () => void qc.invalidateQueries({ queryKey: ["env-vars", owner, repo, env] }) });
  const delSecret = useMutation({ mutationFn: (n: string) => deleteEnvSecret(owner, repo, env, n), onSuccess: () => void qc.invalidateQueries({ queryKey: ["env-secrets", owner, repo, env] }) });
  const variables: GithubActionsVariable[] = vars.data ?? [];
  const secretList: GithubSecret[] = secrets.data ?? [];
  return (
    <div style={{ padding: "0 1rem 1rem", display: "flex", flexDirection: "column", gap: "0.8rem", background: "var(--color-bg-subtle)" }}>
      <MutationError of={[saveWait, addVar, delVar, delSecret]} />
      <div>
        <h3 style={{ fontSize: "0.85rem", fontWeight: 600, margin: "0.6rem 0 0.3rem" }}>Protection rules</h3>
        <div style={{ display: "flex", flexDirection: "column", gap: "0.6rem" }}>
          <div>
            <h4 style={{ fontSize: "0.8rem", fontWeight: 600, margin: "0 0 0.25rem" }}>Required reviewers</h4>
            {reviewers.length === 0 ? (
              <p style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", margin: "0 0 0.25rem" }}>
                No required reviewers.
              </p>
            ) : (
              <ul style={{ listStyle: "none", margin: "0 0 0.25rem", padding: 0 }}>
                {reviewers.map((r) => (
                  <li key={`${r.type}-${r.id}`} style={{ display: "flex", alignItems: "center", justifyContent: "space-between", fontSize: "0.82rem", padding: "0.15rem 0" }}>
                    <span>
                      {r.label}{" "}
                      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.74rem" }}>
                        {r.type}
                        {r.slug ? ` · ${r.slug}` : ""}
                      </span>
                    </span>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Remove reviewer ${r.label}`}
                      onClick={() => setReviewers((prev) => prev.filter((x) => !(x.type === r.type && x.id === r.id)))}
                    >
                      Remove
                    </Button>
                  </li>
                ))}
              </ul>
            )}
            <div style={{ display: "flex", gap: "0.4rem", flexWrap: "wrap", alignItems: "flex-end" }}>
              <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                Add user reviewer
                <select
                  aria-label={`Add user reviewer for ${env}`}
                  value={pickUser}
                  onChange={(e) => setPickUser(e.target.value)}
                  style={settingsInputStyle}
                >
                  <option value="">Select collaborator…</option>
                  {(collaboratorsQ.data ?? []).map((c) => (
                    <option key={c.id} value={String(c.id)}>{c.login}</option>
                  ))}
                </select>
              </label>
              <Button
                size="sm"
                variant="secondary"
                disabled={!pickUser}
                onClick={() => {
                  const c = (collaboratorsQ.data ?? []).find((x) => String(x.id) === pickUser);
                  if (c) addReviewer({ type: "User", id: c.id, label: c.login });
                  setPickUser("");
                }}
              >
                Add user
              </Button>
              {teamsQ.data && teamsQ.data.length > 0 && (
                <>
                  <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                    Add team reviewer
                    <select
                      aria-label={`Add team reviewer for ${env}`}
                      value={pickTeam}
                      onChange={(e) => setPickTeam(e.target.value)}
                      style={settingsInputStyle}
                    >
                      <option value="">Select team…</option>
                      {teamsQ.data.map((t) => (
                        <option key={t.id} value={String(t.id)}>{t.name}</option>
                      ))}
                    </select>
                  </label>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={!pickTeam}
                    onClick={() => {
                      const t = (teamsQ.data ?? []).find((x) => String(x.id) === pickTeam);
                      if (t) addReviewer({ type: "Team", id: t.id, label: t.name });
                      setPickTeam("");
                    }}
                  >
                    Add team
                  </Button>
                </>
              )}
            </div>
          </div>
          <div style={{ display: "flex", alignItems: "flex-end", gap: "0.4rem", flexWrap: "wrap" }}>
            <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
              Wait timer (minutes)
              <input
                type="number"
                min={0}
                max={43200}
                aria-label={`Wait timer for ${env}`}
                value={waitTimer}
                onChange={(e) => setWaitTimer(e.target.value)}
                style={settingsInputStyle}
              />
            </label>
            <Button size="sm" variant="secondary" disabled={saveWait.isPending} onClick={() => saveWait.mutate()}>
              {saveWait.isPending ? "Saving…" : "Save protection"}
            </Button>
          </div>
        </div>
      </div>
      <div>
        <h3 style={{ fontSize: "0.85rem", fontWeight: 600, margin: "0.6rem 0 0.3rem" }}>Environment variables</h3>
        {vars.isLoading ? (
          <Spinner label="loading variables" />
        ) : vars.isError ? (
          <InlineError title="Failed to load variables" detail={String(vars.error)} />
        ) : variables.length === 0 ? (
          <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>No variables.</p>
        ) : (
          variables.map((v) => (
            <div key={v.name} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", fontSize: "0.82rem", padding: "0.2rem 0" }}>
              <span><code>{v.name}</code> = {v.value}</span>
              <Button size="sm" variant="ghost" aria-label={`Delete variable ${v.name}`} onClick={() => delVar.mutate(v.name)}>Remove</Button>
            </div>
          ))
        )}
        <form onSubmit={(e: FormEvent) => { e.preventDefault(); if (vname.trim()) addVar.mutate(); }} style={{ display: "flex", gap: "0.4rem", marginTop: "0.4rem", flexWrap: "wrap" }}>
          <input value={vname} onChange={(e) => setVName(e.target.value)} placeholder="NAME" aria-label="Variable name" style={settingsInputStyle} />
          <input value={vval} onChange={(e) => setVVal(e.target.value)} placeholder="value" aria-label="Variable value" style={settingsInputStyle} />
          <Button type="submit" size="sm" variant="secondary" disabled={!vname.trim() || addVar.isPending}>Add variable</Button>
        </form>
      </div>
      <div>
        <h3 style={{ fontSize: "0.85rem", fontWeight: 600, margin: "0 0 0.3rem" }}>Environment secrets</h3>
        {secretList.length ? secretList.map((s) => (
          <div key={s.name} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", fontSize: "0.82rem", padding: "0.2rem 0" }}>
            <code>{s.name}</code>
            <Button size="sm" variant="ghost" aria-label={`Delete secret ${s.name}`} onClick={() => delSecret.mutate(s.name)}>Remove</Button>
          </div>
        )) : <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>No secrets.</p>}
      </div>
    </div>
  );
}
