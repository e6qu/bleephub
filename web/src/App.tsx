import { lazy, Suspense, useCallback, useEffect, useState, type ReactNode } from "react";
import { BrowserRouter, Routes, Route, Navigate, useLocation } from "react-router";
import { ErrorBoundary, InlineError, Spinner, ToastProvider } from "@bleephub/ui-core/components";
import { fetchBrowserSession, isLoggedIn, UNAUTHORIZED_EVENT } from "./api.js";
import { BleephubShell } from "./components/Shell.js";
import { Button } from "./components/ui.js";

const LoginPage = lazy(() => import("./pages/LoginPage.js").then(({ LoginPage }) => ({ default: LoginPage })));
const OverviewPage = lazy(() => import("./pages/OverviewPage.js").then(({ OverviewPage }) => ({ default: OverviewPage })));
const WorkflowsPage = lazy(() => import("./pages/WorkflowsPage.js").then(({ WorkflowsPage }) => ({ default: WorkflowsPage })));
const WorkflowDetailPage = lazy(() => import("./pages/WorkflowDetailPage.js").then(({ WorkflowDetailPage }) => ({ default: WorkflowDetailPage })));
const RunnersPage = lazy(() => import("./pages/RunnersPage.js").then(({ RunnersPage }) => ({ default: RunnersPage })));
const ReposPage = lazy(() => import("./pages/ReposPage.js").then(({ ReposPage }) => ({ default: ReposPage })));
const OrgReposPage = lazy(() => import("./pages/OrgReposPage.js").then(({ OrgReposPage }) => ({ default: OrgReposPage })));
const OrgProjectsV2Page = lazy(() => import("./pages/OrgProjectsV2Page.js").then(({ OrgProjectsV2Page }) => ({ default: OrgProjectsV2Page })));
const RepoDetailPage = lazy(() => import("./pages/RepoDetailPage.js").then(({ RepoDetailPage }) => ({ default: RepoDetailPage })));
const RepoCommitPage = lazy(() => import("./pages/RepoDetailPage.js").then(({ RepoCommitPage }) => ({ default: RepoCommitPage })));
const RepoComparePage = lazy(() => import("./pages/RepoDetailPage.js").then(({ RepoComparePage }) => ({ default: RepoComparePage })));
const RepoFilePage = lazy(() => import("./pages/RepoDetailPage.js").then(({ RepoFilePage }) => ({ default: RepoFilePage })));
const ReleasesPage = lazy(() => import("./pages/ReleasesPage.js").then(({ ReleasesPage }) => ({ default: ReleasesPage })));
const IssuesPage = lazy(() => import("./pages/IssuesPage.js").then(({ IssuesPage }) => ({ default: IssuesPage })));
const PullsPage = lazy(() => import("./pages/PullsPage.js").then(({ PullsPage }) => ({ default: PullsPage })));
const DiscussionsPage = lazy(() => import("./pages/DiscussionsPage.js").then(({ DiscussionsPage }) => ({ default: DiscussionsPage })));
const ActionsPage = lazy(() => import("./pages/ActionsPage.js").then(({ ActionsPage }) => ({ default: ActionsPage })));
const RunDetailPage = lazy(() => import("./pages/RunDetailPage.js").then(({ RunDetailPage }) => ({ default: RunDetailPage })));
const RepoSettingsPage = lazy(() => import("./pages/RepoSettingsPage.js").then(({ RepoSettingsPage }) => ({ default: RepoSettingsPage })));
const BranchProtectionPage = lazy(() => import("./pages/BranchProtectionPage.js").then(({ BranchProtectionPage }) => ({ default: BranchProtectionPage })));
const SecretScanningPage = lazy(() => import("./pages/SecretScanningPage.js").then(({ SecretScanningPage }) => ({ default: SecretScanningPage })));
const CodeScanningPage = lazy(() => import("./pages/CodeScanningPage.js").then(({ CodeScanningPage }) => ({ default: CodeScanningPage })));
const DependabotPage = lazy(() => import("./pages/DependabotPage.js").then(({ DependabotPage }) => ({ default: DependabotPage })));
const SecurityAdvisoriesPage = lazy(() => import("./pages/SecurityAdvisoriesPage.js").then(({ SecurityAdvisoriesPage }) => ({ default: SecurityAdvisoriesPage })));
const ProjectsClassicPage = lazy(() => import("./pages/ProjectsClassicPage.js").then(({ ProjectsClassicPage }) => ({ default: ProjectsClassicPage })));
const WikiPage = lazy(() => import("./pages/WikiPage.js").then(({ WikiPage }) => ({ default: WikiPage })));
const RepoSecretsPage = lazy(() => import("./pages/RepoSecretsPage.js").then(({ RepoSecretsPage }) => ({ default: RepoSecretsPage })));
const MetricsPage = lazy(() => import("./pages/MetricsPage.js").then(({ MetricsPage }) => ({ default: MetricsPage })));
const AppsPage = lazy(() => import("./pages/AppsPage.js").then(({ AppsPage }) => ({ default: AppsPage })));
const OAuthPage = lazy(() => import("./pages/OAuthPage.js").then(({ OAuthPage }) => ({ default: OAuthPage })));
const UsersPage = lazy(() => import("./pages/UsersPage.js").then(({ UsersPage }) => ({ default: UsersPage })));
const OrgsPage = lazy(() => import("./pages/OrgsPage.js").then(({ OrgsPage }) => ({ default: OrgsPage })));
const TeamsPage = lazy(() => import("./pages/TeamsPage.js").then(({ TeamsPage }) => ({ default: TeamsPage })));
const RulesetsPage = lazy(() => import("./pages/RulesetsPage.js").then(({ RulesetsPage }) => ({ default: RulesetsPage })));
const AuditLogPage = lazy(() => import("./pages/AuditLogPage.js").then(({ AuditLogPage }) => ({ default: AuditLogPage })));
const GistsPage = lazy(() => import("./pages/GistsPage.js").then(({ GistsPage }) => ({ default: GistsPage })));
const NotificationsPage = lazy(() => import("./pages/NotificationsPage.js").then(({ NotificationsPage }) => ({ default: NotificationsPage })));
const MigrationsPage = lazy(() => import("./pages/MigrationsPage.js").then(({ MigrationsPage }) => ({ default: MigrationsPage })));
const CodespacesPage = lazy(() => import("./pages/CodespacesPage.js").then(({ CodespacesPage }) => ({ default: CodespacesPage })));
const PackagesPage = lazy(() => import("./pages/PackagesPage.js").then(({ PackagesPage }) => ({ default: PackagesPage })));
const InsightsPage = lazy(() => import("./pages/InsightsPage.js").then(({ InsightsPage }) => ({ default: InsightsPage })));
const OrgGovernancePage = lazy(() => import("./pages/OrgGovernancePage.js").then(({ OrgGovernancePage }) => ({ default: OrgGovernancePage })));
const CopilotPage = lazy(() => import("./pages/CopilotPage.js").then(({ CopilotPage }) => ({ default: CopilotPage })));
const PersonalCopilotSpacesPage = lazy(() =>
  import("./pages/CopilotPage.js").then(({ PersonalCopilotSpacesPage }) => ({ default: PersonalCopilotSpacesPage })),
);
const EnterprisePage = lazy(() => import("./pages/EnterprisePage.js").then(({ EnterprisePage }) => ({ default: EnterprisePage })));
const DeploymentsPage = lazy(() => import("./pages/DeploymentsPage.js").then(({ DeploymentsPage }) => ({ default: DeploymentsPage })));
const WebhookDeliveriesPage = lazy(() => import("./pages/WebhookDeliveriesPage.js").then(({ WebhookDeliveriesPage }) => ({ default: WebhookDeliveriesPage })));
const OrgHooksPage = lazy(() => import("./pages/OrgHooksPage.js").then(({ OrgHooksPage }) => ({ default: OrgHooksPage })));
const SearchPage = lazy(() => import("./pages/SearchPage.js").then(({ SearchPage }) => ({ default: SearchPage })));
const AccountPage = lazy(() => import("./pages/AccountPage.js").then(({ AccountPage }) => ({ default: AccountPage })));
const RepoSocialPage = lazy(() => import("./pages/RepoSocialPage.js").then(({ RepoSocialPage }) => ({ default: RepoSocialPage })));
const DashboardPage = lazy(() => import("./pages/DashboardPage.js").then(({ DashboardPage }) => ({ default: DashboardPage })));
const ProfilePage = lazy(() => import("./pages/ProfilePage.js").then(({ ProfilePage }) => ({ default: ProfilePage })));
const OrgOverviewPage = lazy(() => import("./pages/OrgOverviewPage.js").then(({ OrgOverviewPage }) => ({ default: OrgOverviewPage })));
const OrgPeoplePage = lazy(() => import("./pages/OrgPeoplePage.js").then(({ OrgPeoplePage }) => ({ default: OrgPeoplePage })));
const OrgTeamsPage = lazy(() => import("./pages/OrgTeamsPage.js").then(({ OrgTeamsPage }) => ({ default: OrgTeamsPage })));
const ClassroomPage = lazy(() => import("./pages/ClassroomPage.js").then(({ ClassroomPage }) => ({ default: ClassroomPage })));
const MarketplacePage = lazy(() => import("./pages/MarketplacePage.js").then(({ MarketplacePage }) => ({ default: MarketplacePage })));
const MarketplacePublisherPage = lazy(() => import("./pages/MarketplacePublisherPage.js").then(({ MarketplacePublisherPage }) => ({ default: MarketplacePublisherPage })));

function LoginRedirect() {
  const location = useLocation();
  const returnTo = location.pathname + location.search + location.hash;
  return <Navigate to={`/ui/login?return_to=${encodeURIComponent(returnTo)}`} replace />;
}

/**
 * A signed-out visitor and an unreachable server are different situations and
 * must not collapse into the same blank screen, so the probe's outcome is
 * modelled explicitly rather than as a nullable boolean.
 */
type SessionState =
  | { status: "probing" }
  | { status: "signed-in" }
  | { status: "signed-out" }
  | { status: "failed"; detail: Error };

function SessionPending() {
  return (
    <div role="status" className="flex min-h-screen items-center justify-center p-6">
      Checking your session…
    </div>
  );
}

function SessionUnavailable({ detail, onRetry }: { detail: Error; onRetry: () => void }) {
  return (
    <div className="mx-auto max-w-xl p-6">
      <InlineError
        title="Could not check your session"
        detail={detail}
        action={
          <Button type="button" onClick={onRetry}>
            Try again
          </Button>
        }
      />
    </div>
  );
}

function RouteErrorBoundary({ children }: { children: ReactNode }) {
  const location = useLocation();
  return <ErrorBoundary key={location.key}>{children}</ErrorBoundary>;
}

function useSessionState() {
  const [state, setState] = useState<SessionState>(() =>
    isLoggedIn() ? { status: "signed-in" } : { status: "probing" },
  );
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (isLoggedIn()) {
      setState({ status: "signed-in" });
      return;
    }
    let current = true;
    setState({ status: "probing" });
    fetchBrowserSession()
      .then((authenticated) => {
        if (current) setState({ status: authenticated ? "signed-in" : "signed-out" });
      })
      .catch((err: unknown) => {
        if (current) {
          setState({ status: "failed", detail: err instanceof Error ? err : new Error(String(err)) });
        }
      });
    return () => {
      current = false;
    };
  }, [attempt]);
  useEffect(() => {
    const signedOut = () => setState({ status: "signed-out" });
    window.addEventListener(UNAUTHORIZED_EVENT, signedOut);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, signedOut);
  }, []);

  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return { state, retry };
}

export function App() {
  const { state, retry } = useSessionState();

  if (state.status === "probing") {
    return <SessionPending />;
  }

  if (state.status === "failed") {
    return (
      <ErrorBoundary>
        <SessionUnavailable detail={state.detail} onRetry={retry} />
      </ErrorBoundary>
    );
  }

  if (state.status === "signed-out") {
    return (
      <ErrorBoundary>
        <BrowserRouter>
          <Suspense fallback={<Spinner label="Loading sign-in" />}>
            <Routes>
              <Route path="/ui/login" element={<LoginPage />} />
              <Route path="/ui/*" element={<LoginRedirect />} />
            </Routes>
          </Suspense>
        </BrowserRouter>
      </ErrorBoundary>
    );
  }

  return (
    <ErrorBoundary>
      <ToastProvider>
        <BrowserRouter>
          <BleephubShell>
            <RouteErrorBoundary>
              <Suspense fallback={<Spinner label="Loading page" />}>
              <Routes>
                <Route path="/ui/" element={<DashboardPage />} />
              <Route path="/ui/workflows" element={<WorkflowsPage />} />
              <Route path="/ui/workflows/:id" element={<WorkflowDetailPage />} />
              <Route path="/ui/runners" element={<RunnersPage />} />
              <Route path="/ui/repos" element={<ReposPage />} />
              <Route path="/ui/orgs/:org/repos" element={<OrgReposPage />} />
              <Route path="/ui/orgs/:org/projects" element={<OrgProjectsV2Page />} />
              <Route path="/ui/orgs/:org/projects/:number" element={<OrgProjectsV2Page />} />
              <Route path="/ui/orgs/:org/rulesets" element={<RulesetsPage />} />
              <Route path="/ui/orgs/:org/governance" element={<OrgGovernancePage />} />
              <Route path="/ui/orgs/:org/copilot" element={<CopilotPage />} />
              <Route path="/ui/repos/:owner/:repo" element={<RepoDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/commits" element={<RepoDetailPage initialTab="commits" />} />
              <Route path="/ui/repos/:owner/:repo/branches" element={<RepoDetailPage initialTab="branches" />} />
              <Route path="/ui/repos/:owner/:repo/tags" element={<RepoDetailPage initialTab="tags" />} />
              <Route path="/ui/repos/:owner/:repo/tree/:ref/*" element={<RepoDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/commits/:sha" element={<RepoCommitPage />} />
              <Route path="/ui/repos/:owner/:repo/compare/:range" element={<RepoComparePage />} />
              <Route path="/ui/repos/:owner/:repo/blob/:ref/*" element={<RepoFilePage />} />
              <Route path="/ui/repos/:owner/:repo/releases" element={<ReleasesPage />} />
              <Route path="/ui/repos/:owner/:repo/releases/new" element={<ReleasesPage />} />
              <Route path="/ui/repos/:owner/:repo/releases/:releaseId" element={<ReleasesPage />} />
              <Route path="/ui/repos/:owner/:repo/issues" element={<IssuesPage />} />
              <Route path="/ui/repos/:owner/:repo/issues/:number" element={<IssuesPage />} />
              <Route path="/ui/repos/:owner/:repo/labels" element={<IssuesPage view="labels" />} />
              <Route path="/ui/repos/:owner/:repo/milestones" element={<IssuesPage view="milestones" />} />
              <Route path="/ui/repos/:owner/:repo/insights" element={<InsightsPage />} />
              <Route path="/ui/repos/:owner/:repo/pulls" element={<PullsPage />} />
              <Route path="/ui/repos/:owner/:repo/pulls/:number" element={<PullsPage />} />
              <Route path="/ui/repos/:owner/:repo/pulls/:number/commits" element={<PullsPage />} />
              <Route path="/ui/repos/:owner/:repo/pulls/:number/files" element={<PullsPage />} />
              <Route path="/ui/repos/:owner/:repo/pulls/:number/checks" element={<PullsPage />} />
              <Route path="/ui/repos/:owner/:repo/discussions" element={<DiscussionsPage />} />
              <Route path="/ui/repos/:owner/:repo/discussions/:number" element={<DiscussionsPage />} />
              <Route path="/ui/repos/:owner/:repo/actions" element={<ActionsPage />} />
              <Route path="/ui/repos/:owner/:repo/actions/runs/:runId" element={<RunDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/settings" element={<RepoSettingsPage />} />
              <Route path="/ui/repos/:owner/:repo/settings/branch-protection" element={<BranchProtectionPage />} />
              <Route path="/ui/repos/:owner/:repo/security/secret-scanning" element={<SecretScanningPage />} />
              <Route path="/ui/repos/:owner/:repo/security/code-scanning" element={<CodeScanningPage />} />
              <Route path="/ui/repos/:owner/:repo/security/dependabot" element={<DependabotPage />} />
              <Route path="/ui/repos/:owner/:repo/security/advisories" element={<SecurityAdvisoriesPage />} />
              <Route path="/ui/repos/:owner/:repo/projects-classic" element={<ProjectsClassicPage />} />
              <Route path="/ui/repos/:owner/:repo/wiki" element={<WikiPage />} />
              <Route path="/ui/repos/:owner/:repo/wiki/:slug" element={<WikiPage />} />
              <Route path="/ui/repos/:owner/:repo/settings/secrets" element={<RepoSecretsPage />} />
              <Route path="/ui/apps" element={<AppsPage />} />
              <Route path="/ui/apps/:publisher/marketplace" element={<MarketplacePublisherPage />} />
              <Route path="/ui/oauth" element={<OAuthPage />} />
              <Route path="/ui/metrics" element={<MetricsPage />} />
              <Route path="/ui/gists" element={<GistsPage />} />
              <Route path="/ui/notifications" element={<NotificationsPage />} />
              <Route path="/ui/packages" element={<PackagesPage />} />
              <Route path="/ui/orgs/:org/packages" element={<PackagesPage />} />
              <Route path="/ui/repos/:owner/:repo/packages" element={<PackagesPage />} />
              <Route path="/ui/migrations" element={<MigrationsPage />} />
              <Route path="/ui/codespaces" element={<CodespacesPage />} />
              <Route path="/ui/codespaces/:codespaceName" element={<CodespacesPage />} />
              <Route path="/ui/repos/:owner/:repo/codespaces" element={<CodespacesPage />} />
              <Route path="/ui/copilot/spaces" element={<PersonalCopilotSpacesPage />} />
              <Route path="/ui/classrooms" element={<ClassroomPage />} />
              <Route path="/ui/classrooms/:classroomId" element={<ClassroomPage />} />
              <Route path="/ui/classrooms/accept/:inviteCode" element={<ClassroomPage />} />
              <Route path="/ui/marketplace" element={<MarketplacePage />} />
              <Route path="/ui/marketplace/:slug" element={<MarketplacePage />} />
              <Route path="/ui/operations" element={<OverviewPage />} />
              <Route path="/ui/operations/users" element={<UsersPage />} />
              <Route path="/ui/operations/orgs" element={<OrgsPage />} />
              <Route path="/ui/operations/teams" element={<TeamsPage />} />
              <Route path="/ui/operations/enterprise" element={<EnterprisePage />} />
              <Route path="/ui/operations/audit-log" element={<AuditLogPage />} />
              {/* Deployments + webhook deliveries + Pages */}
              <Route path="/ui/repos/:owner/:repo/deployments" element={<DeploymentsPage />} />
              <Route path="/ui/repos/:owner/:repo/hooks/:hookId/deliveries" element={<WebhookDeliveriesPage />} />
              <Route path="/ui/orgs/:org/hooks" element={<OrgHooksPage />} />
              <Route path="/ui/orgs/:org/hooks/:hookId/deliveries" element={<WebhookDeliveriesPage />} />
              {/* Search + repo social + account */}
              <Route path="/ui/search" element={<SearchPage />} />
              <Route path="/ui/account" element={<AccountPage />} />
              <Route path="/ui/repos/:owner/:repo/stargazers" element={<RepoSocialPage kind="stargazers" />} />
              <Route path="/ui/repos/:owner/:repo/watchers" element={<RepoSocialPage kind="watchers" />} />
              <Route path="/ui/repos/:owner/:repo/forks" element={<RepoSocialPage kind="forks" />} />
              {/* Bleephub organization overview, people, teams, and user profile pages.
                  Org sub-pages share the OrgHeader tab bar. The bare
                  top-level /ui/:login profile route is registered LAST so
                  every literal /ui/<page> route wins over it. */}
              <Route path="/ui/orgs/:org" element={<OrgOverviewPage />} />
              <Route path="/ui/orgs/:org/people" element={<OrgPeoplePage />} />
              <Route path="/ui/orgs/:org/teams" element={<OrgTeamsPage />} />
              <Route path="/ui/users/:login" element={<ProfilePage />} />
              {/* A logged-in user hitting /ui/login (bookmark) or any
                  unknown /ui/* path lands back on the dashboard. */}
              <Route path="/ui/login" element={<Navigate to="/ui/" replace />} />
              <Route path="/ui/:login" element={<ProfilePage />} />
                <Route path="/ui/*" element={<Navigate to="/ui/" replace />} />
              </Routes>
              </Suspense>
            </RouteErrorBoundary>
          </BleephubShell>
        </BrowserRouter>
      </ToastProvider>
    </ErrorBoundary>
  );
}
