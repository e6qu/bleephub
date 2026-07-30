package bleephub

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Server is the bleephub HTTP server implementing the GitHub Actions
// runner service API (GHES-style endpoints).
type Server struct {
	addr                   string
	mux                    *http.ServeMux
	logger                 zerolog.Logger
	store                  *Store
	graphqlSchema          graphql.Schema
	actionCache            *ActionCache
	artifactStore          *ArtifactStore
	metrics                *Metrics
	maxConcurrentWorkflows int
	scheduleFired          scheduleFiredKeys // cron-firing dedup (on: schedule)
	actionsEvents          actionsEventLoop  // checks/webhook fan-out for run+job transitions
	registryUploadsMu      sync.Mutex
	registryUploads        map[string]*containerRegistryUpload
	classroomMu            sync.Mutex // serializes multi-resource Classroom browser transactions
	marketplaceMu          sync.Mutex // serializes Marketplace billing transitions and webhook emission
	workflowConcurrencyMu  sync.Mutex // serializes concurrency-group admission and queue promotion
	rateLimitsMu           sync.Mutex
	rateLimits             map[string]*apiRateWindow // hashed credential + resource -> current primary-limit window
	routePatterns          []string                  // every pattern registered via route(), for fidelity enumeration
	externalURL            string                    // BLEEPHUB_EXTERNAL_URL; when set, overrides request-Host URL derivation (job messages, action URLs) — the GHES "external URL" knob
	pagesJekyllExecutable  string
	identity               identityConfig
	build                  BuildInfo
	// clockNow is injected by deterministic tests and simulators. Production
	// leaves it nil and currentTime uses the process clock.
	clockNow func() time.Time
	// allowPrivateOutboundTargets opts every server-initiated fetch — webhook
	// delivery and source import — out of the public-address requirement, for
	// a development or test instance whose receivers and sources live on
	// loopback. Denying is the default; nothing turns this on implicitly, and
	// it never relaxes the http/https scheme rule.
	allowPrivateOutboundTargets bool
	webhookClientsOnce          sync.Once
	webhookClients              [2]*http.Client // [verifying TLS, insecure_ssl=1]
	webhookPoolOnce             sync.Once
	webhookPool                 *webhookDispatcher
	// responseObserver, when set before ListenAndServe, sees every
	// request/response pair in the handler chain. The test harness
	// assigns it (same package) to validate /api/v3 response shapes
	// against the vendored GitHub OpenAPI description; nil costs nothing.
	responseObserver func(req *http.Request, status int, header http.Header, body []byte)
	// background tracks the goroutines the server itself starts, so shutdown
	// can wait for them instead of returning while they still run. A goroutine
	// that outlives the process it belongs to keeps writing to a store nobody
	// is reading and holds the listener it was told to release.
	background sync.WaitGroup
}

func (s *Server) currentTime() time.Time {
	if s != nil && s.clockNow != nil {
		return s.clockNow().UTC()
	}
	return time.Now().UTC()
}

// goBackground runs fn as an owned goroutine: shutdown waits for it. Every
// goroutine the server starts for its own lifetime goes through here, so
// "which goroutines are still running" has an answer.
func (s *Server) goBackground(fn func()) {
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		fn()
	}()
}

// BuildInfo identifies the immutable application artifact serving a request.
// Identity and deployment validators can use it without knowing how or where
// Bleephub is deployed.
type BuildInfo struct {
	Version     string
	Commit      string
	PublishedAt string
}

// ServerOption configures immutable server construction state.
type ServerOption func(*Server)

// WithBuildInfo records the versioned artifact metadata for this server.
func WithBuildInfo(info BuildInfo) ServerOption {
	return func(server *Server) {
		server.build = info
	}
}

// serverConstruction contains the dependency/configuration values needed to
// build the complete in-memory server state. Production and tests deliberately
// share newServerState: a test server may choose not to register every route,
// but it must not exercise a structurally different six-field Server that
// production can never create.
type serverConstruction struct {
	byteStore                  actionsByteStore
	dataDir                    string
	maxConcurrentWorkflows     int
	externalURL                string
	pagesJekyllExecutable      string
	identity                   identityConfig
	build                      BuildInfo
	allowPrivateOutboundTarget bool
}

func newServerState(addr string, logger zerolog.Logger, construction serverConstruction) *Server {
	s := &Server{
		addr:                        addr,
		mux:                         http.NewServeMux(),
		logger:                      logger,
		store:                       NewStore(),
		actionCache:                 NewActionCache(),
		artifactStore:               NewArtifactStoreWithByteStore(construction.dataDir, construction.byteStore),
		metrics:                     NewMetrics(),
		maxConcurrentWorkflows:      construction.maxConcurrentWorkflows,
		registryUploads:             map[string]*containerRegistryUpload{},
		rateLimits:                  map[string]*apiRateWindow{},
		externalURL:                 construction.externalURL,
		pagesJekyllExecutable:       construction.pagesJekyllExecutable,
		identity:                    construction.identity,
		build:                       construction.build,
		allowPrivateOutboundTargets: construction.allowPrivateOutboundTarget,
	}
	s.store.actionsArtifacts = s.artifactStore
	return s
}

func (s *Server) setArtifactStore(store *ArtifactStore) {
	s.artifactStore = store
	s.store.actionsArtifacts = store
}

// route registers a handler AND records its "METHOD /path" pattern so the
// registered surface can be enumerated and validated directly (e.g. against
// GitHub's API definition) rather than inferred by probing the catch-all
// fallback. The catch-all is intentionally NOT registered through here, so a
// route that should exist but doesn't is a visible gap in RegisteredRoutes(),
// never silently swallowed by the fallback.
func (s *Server) route(pattern string, handler http.HandlerFunc) {
	s.routePatterns = append(s.routePatterns, pattern)
	if strings.Contains(pattern, " /ui-data/") {
		handler = s.authenticateUIData(handler)
	}
	// /api/v3 routes are instrumented so served requests feed the API
	// insights stats (gh_api_insights.go); other patterns pass through.
	s.mux.HandleFunc(pattern, s.instrumentAPIRoute(pattern, s.enforceFineGrainedPATResource(pattern, handler)))
}

// authenticateUIData gives every browser-only adapter the same credential
// semantics. Previously individual handlers had to remember to authenticate
// themselves; package views did not, so both browser sessions and bearer-based
// browser tests reached them with an empty context and failed 401.
func (s *Server) authenticateUIData(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := s.authenticateRequest(r)
		if invalid, _ := ctx.Value(ctxInvalidCredential).(bool); invalid {
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		if suspended, _ := ctx.Value(ctxSuspendedInstallation).(bool); suspended {
			writeGHError(w, http.StatusForbidden, "This installation has been suspended")
			return
		}
		if suspended, _ := ctx.Value(ctxSuspendedUser).(bool); suspended {
			writeGHError(w, http.StatusForbidden, "This account has been suspended")
			return
		}
		next(w, r.WithContext(ctx))
	}
}

// retiredEnvVars maps an environment variable that no longer exists to the one
// that replaced it. A renamed switch that is simply ignored is a silent loss of
// whatever the operator configured — here, an instance that would stop
// delivering to its own loopback receivers with nothing said — so startup
// refuses instead.
var retiredEnvVars = map[string]string{
	"BLEEPHUB_ALLOW_PRIVATE_WEBHOOK_TARGETS": "BLEEPHUB_ALLOW_PRIVATE_OUTBOUND_TARGETS (it now gates source import as well as webhook delivery)",
}

// retiredEnvVarMessage returns the startup refusal for the first retired
// variable still set, or "" when none is.
func retiredEnvVarMessage() string {
	names := make([]string, 0, len(retiredEnvVars))
	for name := range retiredEnvVars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, present := os.LookupEnv(name); present {
			return name + " has been renamed to " + retiredEnvVars[name] + "; unset the old name and set the new one"
		}
	}
	return ""
}

// NewServer creates a bleephub server with all routes registered.
//
// Honors the persistence-related env vars:
//   - BLEEPHUB_DATA_DIR     — directory for SQLite DB state.
//   - BLEEPHUB_PERSIST      — when "true", enables SQLite-backed state.
//
// Operator-requested persistence that fails to open will log.Fatalf.
//
// When persistence is enabled, the full metadata surface persists: users,
// tokens, apps (incl. credentials + webhook config), OAuth apps,
// installations (incl. selected repos), installation / user-to-server /
// refresh tokens, repos, orgs, teams, memberships, issues, labels,
// milestones, comments, issue events, pull requests, PR reviews + review comments,
// hooks (incl. secrets) + deliveries, app hook deliveries, repo secrets
// (incl. values), check suites/runs/prefs, workflow files, releases,
// deployments + statuses + environments (incl. reviewers/wait timer),
// reactions, Projects v2, user SSH/GPG keys, Pages, branch protection,
// the audit log, and marketplace plans.
//
// Persistence requires durable git storage (BLEEPHUB_GIT_DIR or
// BLEEPHUB_S3_BUCKET): reloading repo metadata against in-memory git
// storage would resurrect every repo empty, so that combination is a
// startup error rather than a silent degraded mode.
//
// Persistence also requires BLEEPHUB_OBJECT_S3_BUCKET for service byte
// content: GitHub Actions artifacts, dependency caches, runner logs, release
// assets, GitHub Packages files, GitHub Container Registry blobs, GitHub
// CodeQL database archives, CodeQL variant-analysis query packs, and artifact
// attestation bundles, and published GitHub Pages archives. SQLite persists only Bleephub metadata; byte content
// must be backed by object storage so a restarted service does not advertise
// durable records whose bytes lived only in memory or local development files.
//
// Workflow run history is persisted; in-flight runs are marked terminal
// cancelled on reload because the runner dispatch state is process-local.
// Browser sessions are persisted so sign-in survives replacement and remains
// consistent across replicas. Agent connections and the OIDC signing key
// (gh_misc_endpoints.go oidcKey) remain process-local; consumers must re-fetch
// the JWKS after key rotation, as they do against real GitHub.
func NewServer(addr string, logger zerolog.Logger, options ...ServerOption) *Server {
	maxWF := 10
	if v := os.Getenv("BLEEPHUB_MAX_WORKFLOWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxWF = n
		}
	}
	dataDir := os.Getenv("BLEEPHUB_DATA_DIR")
	byteStore, err := newActionsByteStoreFromEnv(context.Background())
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize BLEEPHUB_OBJECT_S3_* byte storage")
	}
	s := newServerState(addr, logger, serverConstruction{
		byteStore:                  byteStore,
		dataDir:                    dataDir,
		maxConcurrentWorkflows:     maxWF,
		externalURL:                strings.TrimRight(os.Getenv("BLEEPHUB_EXTERNAL_URL"), "/"),
		pagesJekyllExecutable:      coalesceStr(os.Getenv("BLEEPHUB_PAGES_JEKYLL_EXECUTABLE"), "bleephub-pages-jekyll"),
		identity:                   identityConfigFromEnv(),
		build:                      BuildInfo{Version: "development", Commit: "none", PublishedAt: "not-yet-published"},
		allowPrivateOutboundTarget: strings.EqualFold(strings.TrimSpace(os.Getenv("BLEEPHUB_ALLOW_PRIVATE_OUTBOUND_TARGETS")), "true"),
	})
	if msg := retiredEnvVarMessage(); msg != "" {
		logger.Fatal().Msg(msg)
	}
	for _, option := range options {
		option(s)
	}
	if err := s.identity.validate(); err != nil {
		logger.Fatal().Err(err).Msg("invalid Bleephub external identity configuration")
	}
	if err := validateShauthExternalURL(s.identity, s.externalURL); err != nil {
		logger.Fatal().Err(err).Msg("invalid Bleephub external identity configuration")
	}
	s.store.ObjectByteStore = byteStore
	s.store.Releases.byteStore = byteStore
	if dataDir != "" {
		s.store.PackageDataDir = dataDir
	}

	// Wire persistence. BLEEPHUB_PERSIST=true enables SQLite and fails loud
	// on open failure.
	persist := MustNewPersistence()
	if persist != nil {
		if err := validatePersistentServerStorage(byteStore != nil); err != nil {
			logger.Fatal().Err(err).Msg("invalid Bleephub persistent storage configuration")
		}
		if err := s.artifactStore.SetPersistence(persist); err != nil {
			logger.Fatal().Err(err).Msg("failed to load persisted Actions artifact and cache metadata")
		}
		if err := s.store.SetPersistence(persist); err != nil {
			logger.Fatal().Err(err).Msg("failed to load persisted state")
		}
		s.logger.Info().Str("dialect", persist.dialect.name).Str("data_dir", dataDir).Msg("bleephub persistence enabled")
	}

	// Seed default user only if the store didn't load one from disk.
	if s.store.LookupUserByLogin("admin") == nil {
		s.store.SeedDefaultUser()
	}
	// Seed pre-registered GitHub Apps from config (BLEEPHUB_SEED_APPS /
	// BLEEPHUB_SEED_APPS_FILE) so a coordinate-only consumer can hold a fixed
	// app id + private key + org, exactly as it would against real GitHub.
	if err := s.seedConfiguredApps(); err != nil {
		logger.Fatal().Err(err).Msg("failed to seed configured GitHub Apps")
	}
	s.initGraphQLSchema()
	s.registerRoutes()
	return s
}

func validatePersistentServerStorage(serviceByteStoreReady bool) error {
	if GitDataDir() == "" && !IsS3GitStorage() {
		return errors.New("persistence is enabled (BLEEPHUB_PERSIST=true) but git storage is in-memory: " +
			"repo metadata would survive a restart while every git repo reloads empty. " +
			"Configure durable git storage (BLEEPHUB_GIT_DIR=<dir> or BLEEPHUB_S3_BUCKET=<bucket>) or disable persistence")
	}
	if !serviceByteStoreReady {
		return errors.New("persistence is enabled (BLEEPHUB_PERSIST=true) but service byte storage is not object-backed: " +
			"GitHub Actions artifacts, dependency caches, runner logs, release assets, package files, container-registry blobs, CodeQL database archives, CodeQL variant-analysis query packs, and artifact attestation bundles require BLEEPHUB_OBJECT_S3_BUCKET")
	}
	return nil
}

func (s *Server) registerRoutes() {
	// Health check
	s.route("GET /health", s.handleHealth)

	// Auth + connection data (auth.go)
	s.registerAuthRoutes()

	// Agent management (agents.go)
	s.registerAgentRoutes()

	// Broker: sessions + message poll (broker.go)
	s.registerBrokerRoutes()

	// Job submission (jobs.go)
	s.registerJobRoutes()

	// Action resolution + tarball proxy (actions.go)
	s.registerActionRoutes()

	// Artifact + cache storage (artifacts.go)
	s.registerArtifactRoutes()

	// Run service: acquire/renew/complete (run_service.go)
	s.registerRunServiceRoutes()

	// Timeline + logs (timeline.go)
	s.registerTimelineRoutes()

	// Secrets API (secrets.go)
	s.registerSecretsRoutes()

	// Webhooks API (gh_hooks_rest.go)
	s.registerGHHookRoutes()

	// GitHub Apps API (gh_apps_rest.go)
	s.registerGHAppsRoutes()

	// GitHub Apps webhook config + deliveries (gh_app_hooks_rest.go)
	s.registerGHAppHookRoutes()

	// /applications/{client_id}/* (gh_apps_oauth_mgmt.go) + OAuth Apps mgmt
	s.registerGHAppsOAuthMgmtRoutes()

	// Checks API (gh_checks_rest.go)
	s.registerGHChecksRoutes()

	// Commit Statuses API (gh_statuses_rest.go)
	s.registerGHStatusesRoutes()

	// Commit Comments API (gh_commit_comments_rest.go)
	s.registerGHCommitCommentsRoutes()

	// Reactions API (gh_reactions.go)
	s.registerGHReactionsRoutes()

	// Releases API (gh_releases.go)
	s.registerGHReleasesRoutes()

	// Search API (gh_search.go)
	s.registerGHSearchRoutes()

	// Notifications API (gh_notifications.go)
	s.registerGHNotificationsRoutes()

	// Repository Rulesets API (gh_rulesets.go)
	s.registerGHRulesetRoutes()

	// Secret scanning API (gh_secret_scanning.go)
	s.registerGHSecretScanningRoutes()

	// Code scanning API (gh_code_scanning.go)
	s.registerGHCodeScanningRoutes()

	// Dependabot API (gh_dependabot.go)
	s.registerGHDependabotRoutes()

	// Branch protection API (gh_branch_protection.go)
	s.registerGHBranchProtectionRoutes()

	// Projects classic (v1) API (gh_projects_classic.go)
	s.registerGHProjectsClassicRoutes()

	// Migrations API (gh_migrations.go)
	s.registerGHMigrationsRoutes()

	// Packages API (gh_packages.go)
	s.registerGHPackagesRoutes()

	// Codespaces API (gh_codespaces.go)
	s.registerGHCodespacesRoutes()

	// Actions extras (gh_actions_extras.go) — repository_dispatch, logs, timing
	s.registerGHActionsExtrasRoutes()

	// Deployments + Environments (gh_deployments.go)
	s.registerGHDeploymentsRoutes()

	// PR review comments (gh_pr_comments.go) — inline / file-line / threads
	s.registerGHPRCommentsRoutes()

	// Long-tail surfaces (gh_misc_endpoints.go) — Users keys/follow, OIDC,
	// Pages, branch protection, organization members, and Marketplace.
	s.registerGHMiscEndpoints()

	// GitHub API: REST, GraphQL, OAuth (gh_*.go)
	s.registerGHRestRoutes()
	s.registerGHRepoRoutes()
	s.registerGHSecurityAdvisoriesRoutes()
	s.registerGHRepoAutolinkRoutes()
	s.registerGHRepoInvitationRoutes()
	s.registerGHTemplateRoutes()
	s.registerGHOrgRoutes()
	s.registerGHIssueRoutes()
	s.registerGHPullRoutes()
	s.registerGHGistRoutes()
	s.registerGHOAuthRoutes()
	s.registerGHGraphQLRoutes()
	s.registerGHActionsRoutes()
	s.registerGHActionsPermissionsRoutes()
	s.registerGHWorkflowsRoutes()

	// Org runner groups (gh_runner_groups.go)
	s.registerRunnerGroupRoutes()

	s.registerGHEnterpriseRoutes()
	s.registerGHProjectsV2Routes()
	s.registerGHAttestationsRoutes()
	s.registerGHOrgArtifactMetadataRoutes()
	s.registerGHCopilotRoutes()
	s.registerGHCopilotSpacesRoutes()
	s.registerGHCodeQualityRoutes()
	s.registerGHIssueTypeRoutes()
	s.registerGHIssueFieldRoutes()
	s.registerGHCustomPropertyRoutes()
	s.registerGHCodeSecurityConfigurationRoutes()
	s.registerGHCampaignRoutes()
	s.registerGHPrivateRegistryRoutes()
	s.registerGHNetworkConfigurationRoutes()
	s.registerGHImmutableReleaseRoutes()
	// GitHub-hosted runners (gh_actions_hosted_runners.go)
	s.registerGHHostedRunnerRoutes()
	// Actions OIDC custom property inclusions (gh_actions_oidc_properties.go)
	s.registerGHActionsOIDCPropertyRoutes()
	// Actions concurrency groups (gh_actions_concurrency.go)
	s.registerGHActionsConcurrencyRoutes()
	// Workflow-run control extras (gh_actions_run_control.go)
	s.registerGHActionsRunControlRoutes()
	// GitHub Copilot coding agent secrets + variables (gh_agents_secrets.go)
	s.registerGHAgentsSecretsRoutes()

	// GitHub Copilot coding agent tasks (gh_agents_tasks.go)
	s.registerGHAgentsTasksRoutes()
	// Org people: invitations, outside collaborators, blocks, interaction
	// limits, organization roles, security managers (gh_orgs_people_rest.go)
	s.registerGHOrgsPeopleRoutes()

	// Legacy ID-addressed team endpoints (gh_teams_legacy_rest.go)
	s.registerGHLegacyTeamRoutes()
	// Organization billing budgets + usage reports (gh_org_billing.go)
	s.registerGHOrgBillingRoutes()

	// API insights (gh_api_insights.go)
	s.registerGHAPIInsightsRoutes()

	// Fine-grained personal access token administration (gh_org_pat_admin.go)
	s.registerGHOrgPATAdminRoutes()

	// Organization activity events feed (gh_org_events.go)
	s.registerGHOrgEventsRoutes()
	// User-account surface: profile, emails, interaction limits,
	// Marketplace purchases, billing usage, hovercards (gh_user_surface.go)
	s.registerGHUserSurfaceRoutes()
	// GitHub Pages deployments + health check (gh_pages_deployments.go)
	s.registerGHPagesDeploymentRoutes()
	s.registerGHPagesContentRoutes()

	// Environment deployment branch policies + protection rules (gh_environment_policies.go)
	s.registerGHEnvironmentPolicyRoutes()

	// Repository generation from a template repository (gh_repos_generate.go)
	s.registerGHRepoGenerateRoutes()

	// Source Import API (gh_import.go)
	s.registerGHImportRoutes()

	// Dependency graph: snapshots, SBOM, compare (gh_dependency_graph.go)
	s.registerGHDependencyGraphRoutes()
	s.registerGHMarkdownRoutes()
	s.registerGHMetaExtrasRoutes()
	s.registerGHCodesOfConductRoutes()
	s.registerGHGlobalAdvisoriesRoutes()
	s.registerGHClassroomRoutes()
	s.registerGHClassroomWebRoutes()
	s.registerGHMarketplaceRoutes()
	s.registerGHEventsFeedsRoutes()
	s.registerGHUserIssuesRoutes()
	// Repository read surfaces (gh_repos_reads.go)
	s.registerGHRepoReadsRoutes()

	// Management API (metrics, status, dashboard data)
	s.route("GET /internal/metrics", s.handleInternalMetrics)
	s.route("GET /internal/status", s.handleInternalStatus)
	s.registerMgmtRoutes()

	s.route("GET /internal/storage", s.handleInternalStorage)

	// UI dashboard
	s.registerUIAPIRoutes()
	s.registerUI()
	s.route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
	})

	// Catch-all: tries smart HTTP git protocol, then logs unmatched
	s.mux.HandleFunc("/", s.handleCatchAll)
}

func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	// Try smart HTTP git protocol
	if s.tryHandleGitRequest(w, r) {
		return
	}

	// Codeload-style source archive downloads (legacy.tar.gz / legacy.zip)
	if s.tryHandleArchiveRequest(w, r) {
		return
	}

	s.logger.Warn().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("query", redactedQuery(r.URL)).
		Msg("UNHANDLED REQUEST")
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":          "ok",
		"service":         "bleephub",
		"enterprise_slug": s.enterpriseSlug(),
		"version":         s.build.Version,
		"commit":          s.build.Commit,
		"published_at":    s.build.PublishedAt,
	})
}

func (s *Server) handleInternalMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}

func (s *Server) handleInternalStatus(w http.ResponseWriter, r *http.Request) {
	s.store.mu.RLock()
	activeWfs := 0
	jobsByStatus := make(map[string]int)
	for _, wf := range s.store.Workflows {
		if wf.Status == "running" {
			activeWfs++
		}
		for _, j := range wf.Jobs {
			jobsByStatus[string(j.Status)]++
		}
	}
	sessions := len(s.store.Sessions)
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"active_workflows":  activeWfs,
		"jobs_by_status":    jobsByStatus,
		"connected_runners": sessions,
		"uptime_seconds":    int(time.Since(s.metrics.StartedAt).Seconds()),
	})
}

func (s *Server) handleInternalStorage(w http.ResponseWriter, r *http.Request) {
	persistenceBackend := "none"
	dialectName := ""
	if s.store.persist != nil {
		persistenceBackend = s.store.persist.dialect.name
		dialectName = s.store.persist.dialect.name
	}

	gitBackend := "memory"
	gitDetails := map[string]string{}
	gitDir := GitDataDir()
	if IsS3GitStorage() {
		gitBackend = "s3"
		if bucket := os.Getenv("BLEEPHUB_S3_BUCKET"); bucket != "" {
			gitDetails["bucket"] = bucket
		}
		if endpoint := os.Getenv("BLEEPHUB_S3_ENDPOINT"); endpoint != "" {
			gitDetails["endpoint"] = endpoint
		}
		if prefix := os.Getenv("BLEEPHUB_S3_PREFIX"); prefix != "" {
			gitDetails["prefix"] = prefix
		}
	} else if gitDir != "" {
		gitBackend = "filesystem"
		gitDetails["dir"] = gitDir
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"persistence": persistenceBackend,
		"dialect":     dialectName,
		"git":         gitBackend,
		"git_details": gitDetails,
	})
}

// ListenAndServe starts the HTTP server (crash-only, no graceful shutdown).
// ListenAndServe runs the server until ctx is cancelled, then drains.
//
// The drain is bounded because this server has long-poll routes by design —
// the runner's broker holds a request open waiting for work — so waiting for
// every connection to close would mean never shutting down. Requests still in
// flight past the bound are cut, which is the honest trade: a container that
// refuses to stop is killed anyway, and then nothing drains at all.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.startGitSSH(ctx); err != nil {
		return err
	}
	s.startScheduleDispatcher(ctx)
	handler := s.requestHandler()

	srv := &http.Server{
		Addr:    s.addr,
		Handler: handler,
		// Bound only the header read (slowloris protection). A fixed
		// ReadTimeout/WriteTimeout caps the WHOLE body, which cuts off large
		// git push/pull + artifact uploads/downloads under load.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// net/http logs connection-level errors to the standard logger by
		// default, which bypasses zerolog's level filter and the telemetry
		// bridge entirely. Route them through the server's own logger.
		ErrorLog: newStdLogBridge(s.logger),
	}

	// Resolve addr for log output
	host, port, _ := net.SplitHostPort(s.addr)
	if host == "" {
		host = "localhost"
	}

	// TLS support via environment variables.
	//
	// The pair is all-or-nothing. Treating a half-configured pair as "no TLS"
	// silently serves plaintext on the port the operator believes is encrypted
	// — a typo, an unmounted secret or a half-finished rotation becomes a
	// downgrade nobody is told about. Refuse to start instead.
	certFile := os.Getenv("BPH_TLS_CERT")
	keyFile := os.Getenv("BPH_TLS_KEY")
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("BPH_TLS_CERT and BPH_TLS_KEY must be set together (cert %s, key %s)",
			presenceLabel(certFile), presenceLabel(keyFile))
	}
	serveErr := make(chan error, 1)
	if certFile != "" {
		// Fail on an unreadable cert here rather than at the first handshake,
		// so a bad path is a startup error and not an outage discovered by a
		// client.
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return fmt.Errorf("load TLS keypair: %w", err)
		}
		s.logger.Info().Msgf("bleephub listening on https://%s:%s", host, port)
		go func() { serveErr <- srv.ListenAndServeTLS(certFile, keyFile) }()
	} else {
		s.logger.Info().Msgf("bleephub listening on http://%s:%s", host, port)
		go func() { serveErr <- srv.ListenAndServe() }()
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.logger.Info().Msg("shutting down")
	drain, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(drain)
	// Whatever the listeners did, the owned goroutines get their chance to
	// notice ctx and finish; a goroutine still running after this is a bug in
	// its own ownership, not something to wait indefinitely for.
	s.background.Wait()
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// requestHandler is the one production HTTP pipeline. In-process tests use it
// too, so authentication, prefix routing, replica refresh, recovery, logging,
// response observation, and telemetry cannot silently diverge from the live
// listener.
func (s *Server) requestHandler() http.Handler {
	inner := s.prefixStripMiddleware(s.replicaRefreshMiddleware(s.internalAuthMiddleware(s.mux)))
	ghWrapped := s.ghHeadersMiddleware(inner)
	observed := ghWrapped
	if s.responseObserver != nil {
		observed = s.observeMiddleware(ghWrapped)
	}
	return otelhttp.NewHandler(s.recoverMiddleware(s.loggingMiddleware(s.adminHostMiddleware(observed))), "bleephub")
}

// shutdownGrace bounds the drain. Long enough for an ordinary request or a
// git push to finish, short enough that a container stop does not escalate to
// SIGKILL — Docker and Kubernetes both default to ten seconds, so anything
// larger is theatre unless the operator raises theirs too.
const shutdownGrace = 8 * time.Second

func presenceLabel(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}

func (s *Server) adminHostMiddleware(next http.Handler) http.Handler {
	adminHost := strings.TrimSpace(os.Getenv("BLEEPHUB_ADMIN_HOST"))
	if adminHost == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(strings.Split(r.Host, ":")[0], adminHost) && r.URL.Path == "/" {
			http.Redirect(w, r, "/control", http.StatusTemporaryRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// internalAuthMiddleware gates the operator-facing /internal/* surface — the
// sim-control + dashboard endpoints that have no GitHub API equivalent
// (job/workflow submission + status under /internal/exec, app/oauth-app
// management, and the dashboard aggregations). These are NOT part of the
// GitHub-compatible /api/ surface, so they live here rather than under
// /api/v3/. They require a valid token (the UI sends the admin token as a
// Bearer credential); the resolved user is injected into the request context
// so management handlers can attribute ownership via ghUserFromContext.
// /health stays open for liveness probes.
func (s *Server) internalAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		user := s.internalTokenUser(r)
		if user == nil {
			if session := s.sessionFromRequest(r); session != nil {
				user = s.store.GetUserByID(session.UserID)
			}
		}
		if user == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"})
			return
		}
		// Site admin, for the whole /internal/ prefix rather than per route
		// table. Ten routes are registered outside registerMgmtRoutes —
		// /internal/exec/*, /internal/metrics, /internal/status,
		// /internal/storage, POST /internal/orgs — and gating one table left
		// those open to any account holding any token. handleSubmitJob does no
		// authorization of its own and dispatches a container to the runner
		// fleet, so that gap was arbitrary code execution for the
		// lowest-privileged account on the instance.
		//
		// The prefix is an operator surface, not a naming convention, so the
		// check belongs here where nothing can be registered around it.
		if !user.SiteAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Must be a site admin"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, user)))
	})
}

func (s *Server) replicaRefreshMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.RefreshFromPersistenceIfStale(); err != nil {
			s.logger.Error().Err(err).Msg("failed to refresh shared persistence state")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"message": "Shared state is temporarily unavailable",
			})
			return
		}
		if err := s.artifactStore.RefreshFromPersistenceIfStale(); err != nil {
			s.logger.Error().Err(err).Msg("failed to refresh shared Actions metadata")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"message": "Shared Actions metadata is temporarily unavailable",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// internalTokenUser resolves the user for a token recognized on the internal
// surface: any PAT in the store (which includes the seeded admin token).
// Returns nil when absent/unknown. ghs_/gho_/ghu_ installation/OAuth tokens
// are intentionally not accepted here — the internal surface is operator-only.
func (s *Server) internalTokenUser(r *http.Request) *User {
	scheme, cred := authScheme(r.Header.Get("Authorization"))
	var tok string
	if scheme == "bearer" || scheme == "token" {
		tok = cred
	}
	if tok == "" {
		return nil
	}
	t, user := s.store.LookupToken(tok)
	if t == nil {
		return nil
	}
	return user
}

// prefixStripMiddleware removes any path segments before the known API
// prefixes. The runner prepends the tenant URL path to every call, so a
// request arrives as /owner/repo/_apis/... rather than /_apis/... — which
// is why a refused runner call logs the stripped path.
func (s *Server) prefixStripMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Strip everything before /_apis/ or /api/
		for _, prefix := range []string{"/_apis/", "/api/"} {
			if idx := strings.Index(path, prefix); idx > 0 {
				r.URL.Path = path[idx:]
				r.URL.RawPath = ""
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.logger.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.status).
			Dur("dur", time.Since(start)).
			Msg("request")
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// writeJSON marshals v as JSON and writes it to w.
// writeJSON is the response choke point for essentially every handler.
//
// The body is marshalled before the status is written, so a value that cannot
// be encoded — an unsupported type, a NaN, a cycle — becomes a 500 instead of a
// 200 with a truncated body and no record of what happened. Previously the
// encoder wrote straight to the ResponseWriter and its error was discarded, so
// the status line had already been committed by the time anything could fail.
//
// Content-Length is deliberately left to net/http: a handful of handlers write
// further bytes after calling this, and declaring a length here would make
// those responses malformed.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error","documentation_url":"https://docs.github.com/rest"}` + "\n"))
		return
	}
	sum := sha256.Sum256(body)
	etag := fmt.Sprintf(`"%x"`, sum)
	for current := w; current != nil; {
		if conditional, ok := current.(interface {
			conditionalJSON(string, int) bool
		}); ok {
			if conditional.conditionalJSON(etag, status) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			break
		}
		unwrapper, ok := current.(interface {
			Unwrap() http.ResponseWriter
		})
		if !ok {
			break
		}
		current = unwrapper.Unwrap()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// recoverMiddleware turns a panicking handler into a 500 instead of a silently
// aborted connection.
//
// Without it, net/http's per-connection recover catches the panic, writes
// nothing, and closes the connection: the client sees a truncated read, and
// there is no zerolog record, no span error and no metric. It sits inside the
// otelhttp handler so the span is still open and can record the error, and
// outside everything else so a panic anywhere below is caught.
//
// This bounds the blast radius of a panic; it does not make one harmless. A
// handler that panics while holding the store lock still leaves it held, and
// the recovery cannot release it — the fix for that is the lock discipline
// itself, not this middleware.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A client that hung up mid-write is not a server fault; the
			// standard library uses this sentinel for exactly that.
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			if _, ok := recovered.(*persistenceFailure); ok {
				if reloadErr := s.store.ReloadFromPersistence(); reloadErr != nil {
					s.logger.Error().Err(reloadErr).Msg("failed to restore durable state after persistence error")
				}
			}

			span := trace.SpanFromContext(r.Context())
			err := fmt.Errorf("panic: %v", recovered)
			span.RecordError(err)
			span.SetStatus(codes.Error, "handler panicked")

			s.logger.Error().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()).
				Msg("handler panicked")

			writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		}()
		next.ServeHTTP(w, r)
	})
}

// newStdLogBridge adapts zerolog to the *log.Logger net/http wants for
// ErrorLog, so connection-level errors reach the same sink as everything else.
func newStdLogBridge(logger zerolog.Logger) *stdlog.Logger {
	return stdlog.New(stdLogWriter{logger: logger}, "", 0)
}

type stdLogWriter struct{ logger zerolog.Logger }

func (w stdLogWriter) Write(p []byte) (int, error) {
	w.logger.Error().Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}

// redactedQuery renders a query string with every value replaced unless the
// parameter is on a small allowlist of names known to carry no secret.
//
// The allowlist is deliberately the inverted default. This server implements
// OAuth and the device flow, so an unhandled or mistyped request can carry
// `code`, `client_secret`, `access_token` or `device_code` in the query — and
// anything logged here also reaches the telemetry backend through the log
// bridge. Redacting a known-secret denylist would mean every parameter added
// later is disclosed until someone remembers to add it.
func redactedQuery(u *url.URL) string {
	values := u.Query()
	if len(values) == 0 {
		return ""
	}
	safe := map[string]bool{
		"page": true, "per_page": true, "state": true, "sort": true,
		"direction": true, "since": true, "before": true, "after": true,
		"ref": true, "sha": true, "path": true, "filter": true, "type": true,
		"scope": true, "q": true, "service": true, "visibility": true,
	}
	out := url.Values{}
	for key, vals := range values {
		if safe[key] {
			out[key] = vals
			continue
		}
		out.Set(key, "<redacted>")
	}
	return out.Encode()
}
