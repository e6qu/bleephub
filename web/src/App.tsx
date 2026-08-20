import { lazy, Suspense, useCallback, useEffect, useState, type ReactElement, type ReactNode } from "react";
import { BrowserRouter, Routes, Route, Navigate, useLocation } from "react-router";
import { ErrorBoundary, InlineError, Spinner, ToastProvider } from "@bleephub/ui-core/components";
import { fetchBrowserSession, isLoggedIn, UNAUTHORIZED_EVENT } from "./api.js";
import { BleephubShell } from "./components/Shell.js";
import { SessionContext } from "./session.js";
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
const BlamePage = lazy(() => import("./pages/BlamePage.js").then(({ BlamePage }) => ({ default: BlamePage })));
const ReleasesPage = lazy(() => import("./pages/ReleasesPage.js").then(({ ReleasesPage }) => ({ default: ReleasesPage })));
const IssuesPage = lazy(() => import("./pages/IssuesPage.js").then(({ IssuesPage }) => ({ default: IssuesPage })));
const PullsPage = lazy(() => import("./pages/PullsPage.js").then(({ PullsPage }) => ({ default: PullsPage })));
const DiscussionsPage = lazy(() => import("./pages/DiscussionsPage.js").then(({ DiscussionsPage }) => ({ default: DiscussionsPage })));
const ActionsPage = lazy(() => import("./pages/ActionsPage.js").then(({ ActionsPage }) => ({ default: ActionsPage })));
const RunDetailPage = lazy(() => import("./pages/RunDetailPage.js").then(({ RunDetailPage }) => ({ default: RunDetailPage })));
const RepoSettingsPage = lazy(() => import("./pages/RepoSettingsPage.js").then(({ RepoSettingsPage }) => ({ default: RepoSettingsPage })));
const BranchProtectionPage = lazy(() => import("./pages/BranchProtectionPage.js").then(({ BranchProtectionPage }) => ({ default: BranchProtectionPage })));
const RepoSecurityOverviewPage = lazy(() => import("./pages/RepoSecurityOverviewPage.js").then(({ RepoSecurityOverviewPage }) => ({ default: RepoSecurityOverviewPage })));
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
const GistDetailPage = lazy(() =>
  import("./pages/GistDetailPage.js").then(({ GistDetailPage }) => ({ default: GistDetailPage })),
);
const NotificationsPage = lazy(() => import("./pages/NotificationsPage.js").then(({ NotificationsPage }) => ({ default: NotificationsPage })));
const MigrationsPage = lazy(() => import("./pages/MigrationsPage.js").then(({ MigrationsPage }) => ({ default: MigrationsPage })));
const CodespacesPage = lazy(() => import("./pages/CodespacesPage.js").then(({ CodespacesPage }) => ({ default: CodespacesPage })));
const PackagesPage = lazy(() => import("./pages/PackagesPage.js").then(({ PackagesPage }) => ({ default: PackagesPage })));
const InsightsPage = lazy(() => import("./pages/InsightsPage.js").then(({ InsightsPage }) => ({ default: InsightsPage })));
const OrgGovernancePage = lazy(() => import("./pages/OrgGovernancePage.js").then(({ OrgGovernancePage }) => ({ default: OrgGovernancePage })));
const OrgSettingsPage = lazy(() => import("./pages/OrgSettingsPage.js").then(({ OrgSettingsPage }) => ({ default: OrgSettingsPage })));
const OrgInsightsPage = lazy(() => import("./pages/OrgInsightsPage.js").then(({ OrgInsightsPage }) => ({ default: OrgInsightsPage })));
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
const MyOrganizationsPage = lazy(() => import("./pages/MyOrganizationsPage.js").then(({ MyOrganizationsPage }) => ({ default: MyOrganizationsPage })));
const RepoSocialPage = lazy(() => import("./pages/RepoSocialPage.js").then(({ RepoSocialPage }) => ({ default: RepoSocialPage })));
const DashboardPage = lazy(() => import("./pages/DashboardPage.js").then(({ DashboardPage }) => ({ default: DashboardPage })));
const ProfilePage = lazy(() => import("./pages/ProfilePage.js").then(({ ProfilePage }) => ({ default: ProfilePage })));
const OrgOverviewPage = lazy(() => import("./pages/OrgOverviewPage.js").then(({ OrgOverviewPage }) => ({ default: OrgOverviewPage })));
const OrgPeoplePage = lazy(() => import("./pages/OrgPeoplePage.js").then(({ OrgPeoplePage }) => ({ default: OrgPeoplePage })));
const OrgTeamsPage = lazy(() => import("./pages/OrgTeamsPage.js").then(({ OrgTeamsPage }) => ({ default: OrgTeamsPage })));
const OrgTeamDetailPage = lazy(() => import("./pages/OrgTeamDetailPage.js").then(({ OrgTeamDetailPage }) => ({ default: OrgTeamDetailPage })));
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

  const signedIn = state.status === "signed-in";
  return (
    <ErrorBoundary>
      <SessionContext.Provider value={signedIn}>
        <ToastProvider>
          <BrowserRouter>
            <AppRoutes signedIn={signedIn} />
          </BrowserRouter>
        </ToastProvider>
      </SessionContext.Provider>
    </ErrorBoundary>
  );
}

/**
 * The full route table, rendered for signed-in AND signed-out visitors so
 * public content is browsable anonymously (github.com behaviour). Every
 * viewer-scoped surface is wrapped in `gate(...)`: signed-in renders the
 * page, signed-out redirects to the sign-in page preserving the location.
 *
 * A route is gated when its page's data is viewer-scoped (dashboard,
 * notifications, settings, …) OR when the server refuses its reads
 * anonymously (discussions and projects-classic are GraphQL/auth-only,
 * packages / marketplace / org teams & projects / security alerts all 401
 * without a session) — an anonymous page must fire zero 401ing requests.
 */
function AppRoutes({ signedIn }: { signedIn: boolean }) {
  const location = useLocation();
  // The sign-in page renders chromeless, exactly as the signed-out app did
  // before anonymous browsing existed; a signed-in visitor bounces home.
  if (location.pathname === "/ui/login") {
    return signedIn ? (
      <Navigate to="/ui/" replace />
    ) : (
      <Suspense fallback={<Spinner label="Loading sign-in" />}>
        <LoginPage />
      </Suspense>
    );
  }
  const gate = (element: ReactElement) => (signedIn ? element : <LoginRedirect />);
  return (
    <BleephubShell>
      <RouteErrorBoundary>
        <Suspense fallback={<Spinner label="Loading page" />}>
              <Routes>
                <Route path="/ui/" element={gate(<DashboardPage />)} />
              <Route path="/ui/workflows" element={gate(<WorkflowsPage />)} />
              <Route path="/ui/workflows/:id" element={gate(<WorkflowDetailPage />)} />
              <Route path="/ui/runners" element={gate(<RunnersPage />)} />
              <Route path="/ui/repos" element={gate(<ReposPage />)} />
              <Route path="/ui/orgs/:org/repos" element={<OrgReposPage />} />
              <Route path="/ui/orgs/:org/projects" element={gate(<OrgProjectsV2Page />)} />
              <Route path="/ui/orgs/:org/projects/:number" element={gate(<OrgProjectsV2Page />)} />
              <Route path="/ui/orgs/:org/rulesets" element={gate(<RulesetsPage />)} />
              <Route path="/ui/orgs/:org/governance" element={gate(<OrgGovernancePage />)} />
              <Route path="/ui/orgs/:org/settings" element={gate(<OrgSettingsPage />)} />
              <Route path="/ui/orgs/:org/insights" element={gate(<OrgInsightsPage />)} />
              <Route path="/ui/orgs/:org/copilot" element={gate(<CopilotPage />)} />
              <Route path="/ui/repos/:owner/:repo" element={<RepoDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/commits" element={<RepoDetailPage initialTab="commits" />} />
              <Route path="/ui/repos/:owner/:repo/branches" element={<RepoDetailPage initialTab="branches" />} />
              <Route path="/ui/repos/:owner/:repo/tags" element={<RepoDetailPage initialTab="tags" />} />
              <Route path="/ui/repos/:owner/:repo/activity" element={<RepoDetailPage initialTab="activity" />} />
              <Route path="/ui/repos/:owner/:repo/tree/:ref/*" element={<RepoDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/commits/:sha" element={<RepoCommitPage />} />
              <Route path="/ui/repos/:owner/:repo/compare/:range" element={<RepoComparePage />} />
              <Route path="/ui/repos/:owner/:repo/blob/:ref/*" element={<RepoFilePage />} />
              <Route path="/ui/repos/:owner/:repo/blame/:ref/*" element={<BlamePage />} />
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
              <Route path="/ui/repos/:owner/:repo/discussions" element={gate(<DiscussionsPage />)} />
              <Route path="/ui/repos/:owner/:repo/discussions/:number" element={gate(<DiscussionsPage />)} />
              <Route path="/ui/repos/:owner/:repo/actions" element={<ActionsPage />} />
              <Route path="/ui/repos/:owner/:repo/actions/runs/:runId" element={<RunDetailPage />} />
              <Route path="/ui/repos/:owner/:repo/settings" element={gate(<RepoSettingsPage />)} />
              <Route path="/ui/repos/:owner/:repo/settings/branch-protection" element={gate(<BranchProtectionPage />)} />
              <Route path="/ui/repos/:owner/:repo/security" element={gate(<RepoSecurityOverviewPage />)} />
              <Route path="/ui/repos/:owner/:repo/security/secret-scanning" element={gate(<SecretScanningPage />)} />
              <Route path="/ui/repos/:owner/:repo/security/code-scanning" element={gate(<CodeScanningPage />)} />
              <Route path="/ui/repos/:owner/:repo/security/dependabot" element={gate(<DependabotPage />)} />
              <Route path="/ui/repos/:owner/:repo/security/advisories" element={gate(<SecurityAdvisoriesPage />)} />
              <Route path="/ui/repos/:owner/:repo/projects-classic" element={gate(<ProjectsClassicPage />)} />
              <Route path="/ui/repos/:owner/:repo/wiki" element={<WikiPage />} />
              <Route path="/ui/repos/:owner/:repo/wiki/:slug" element={<WikiPage />} />
              <Route path="/ui/repos/:owner/:repo/settings/secrets" element={gate(<RepoSecretsPage />)} />
              <Route path="/ui/apps" element={gate(<AppsPage />)} />
              <Route path="/ui/apps/:publisher/marketplace" element={gate(<MarketplacePublisherPage />)} />
              <Route path="/ui/oauth" element={gate(<OAuthPage />)} />
              <Route path="/ui/metrics" element={gate(<MetricsPage />)} />
              <Route path="/ui/gists" element={<GistsPage />} />
              <Route path="/ui/gists/:id" element={<GistDetailPage />} />
              <Route path="/ui/notifications" element={gate(<NotificationsPage />)} />
              <Route path="/ui/packages" element={gate(<PackagesPage />)} />
              <Route path="/ui/orgs/:org/packages" element={gate(<PackagesPage />)} />
              <Route path="/ui/repos/:owner/:repo/packages" element={gate(<PackagesPage />)} />
              <Route path="/ui/migrations" element={gate(<MigrationsPage />)} />
              <Route path="/ui/codespaces" element={gate(<CodespacesPage />)} />
              <Route path="/ui/codespaces/:codespaceName" element={gate(<CodespacesPage />)} />
              <Route path="/ui/repos/:owner/:repo/codespaces" element={gate(<CodespacesPage />)} />
              <Route path="/ui/copilot/spaces" element={gate(<PersonalCopilotSpacesPage />)} />
              <Route path="/ui/classrooms" element={gate(<ClassroomPage />)} />
              <Route path="/ui/classrooms/:classroomId" element={gate(<ClassroomPage />)} />
              <Route path="/ui/classrooms/accept/:inviteCode" element={gate(<ClassroomPage />)} />
              <Route path="/ui/marketplace" element={gate(<MarketplacePage />)} />
              <Route path="/ui/marketplace/:slug" element={gate(<MarketplacePage />)} />
              <Route path="/ui/operations" element={gate(<OverviewPage />)} />
              <Route path="/ui/operations/users" element={gate(<UsersPage />)} />
              <Route path="/ui/operations/orgs" element={gate(<OrgsPage />)} />
              <Route path="/ui/operations/teams" element={gate(<TeamsPage />)} />
              <Route path="/ui/operations/enterprise" element={gate(<EnterprisePage />)} />
              <Route path="/ui/operations/audit-log" element={gate(<AuditLogPage />)} />
              {/* Deployments + webhook deliveries + Pages */}
              <Route path="/ui/repos/:owner/:repo/deployments" element={<DeploymentsPage />} />
              <Route path="/ui/repos/:owner/:repo/hooks/:hookId/deliveries" element={gate(<WebhookDeliveriesPage />)} />
              <Route path="/ui/orgs/:org/hooks" element={gate(<OrgHooksPage />)} />
              <Route path="/ui/orgs/:org/hooks/:hookId/deliveries" element={gate(<WebhookDeliveriesPage />)} />
              {/* Search + repo social + account */}
              <Route path="/ui/search" element={<SearchPage />} />
              <Route path="/ui/account" element={gate(<AccountPage />)} />
              <Route path="/ui/settings/organizations" element={gate(<MyOrganizationsPage />)} />
              <Route path="/ui/repos/:owner/:repo/stargazers" element={<RepoSocialPage kind="stargazers" />} />
              <Route path="/ui/repos/:owner/:repo/watchers" element={<RepoSocialPage kind="watchers" />} />
              <Route path="/ui/repos/:owner/:repo/forks" element={<RepoSocialPage kind="forks" />} />
              {/* Bleephub organization overview, people, teams, and user profile pages.
                  Org sub-pages share the OrgHeader tab bar. The bare
                  top-level /ui/:login profile route is registered LAST so
                  every literal /ui/<page> route wins over it. */}
              <Route path="/ui/orgs/:org" element={<OrgOverviewPage />} />
              <Route path="/ui/orgs/:org/people" element={<OrgPeoplePage />} />
              <Route path="/ui/orgs/:org/teams" element={gate(<OrgTeamsPage />)} />
              <Route path="/ui/orgs/:org/teams/:slug" element={gate(<OrgTeamDetailPage />)} />
              <Route path="/ui/users/:login" element={<ProfilePage />} />
              {/* Any unknown /ui/* path lands back on the dashboard
                  (which, signed out, redirects to sign-in). /ui/login is
                  handled above, before the shell. */}
              <Route path="/ui/:login" element={<ProfilePage />} />
                <Route path="/ui/*" element={<Navigate to="/ui/" replace />} />
              </Routes>
        </Suspense>
      </RouteErrorBoundary>
    </BleephubShell>
  );
}
