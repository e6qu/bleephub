package bleephub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/gitstore"
	"github.com/e6qu/bleephub/internal/graphqlapi"
	"github.com/e6qu/bleephub/internal/store"
)

// Server is the bleephub HTTP server.
type Server struct {
	addr                   string
	mux                    *http.ServeMux
	logger                 zerolog.Logger
	store                  *store.Store
	graphql                *graphqlapi.Resolver
	actionCache            *ActionCache
	artifactStore          *store.ArtifactStore
	metrics                *Metrics
	maxConcurrentWorkflows int
	// inFlightSlots caps concurrent non-byte-transfer requests when
	// BLEEPHUB_MAX_INFLIGHT is set; nil (the default) means unlimited. Provides
	// backpressure — a 503 with Retry-After — under a request flood instead of
	// unbounded goroutines piling onto the global store lock. Git/artifact
	// byte-transfer routes are exempt so long transfers never consume a slot.
	inFlightSlots chan struct{}
	// injectedByteStore, set by a ServerOption before persistence wiring,
	// overrides the env-derived object byte store so a test can run a persistent
	// server with an in-memory store instead of a live S3/MinIO endpoint.
	injectedByteStore store.ActionsByteStore
	// localByteStore backs byte content when no object storage is configured
	// (filesystem under the data dir, or in-process). Git LFS is served from it.
	localByteStore store.ActionsByteStore
	// actions is the extracted Actions execution engine (ARCH-002), reached only
	// through its exported surface.
	actions           *actions.Engine
	registryUploadsMu sync.Mutex
	registryUploads   map[string]*containerRegistryUpload
	classroomMu       sync.Mutex // serializes multi-resource Classroom browser transactions
	marketplaceMu     sync.Mutex // serializes Marketplace billing transitions and webhook emission
	rateLimitsMu      sync.Mutex
	rateLimits        map[string]*apiRateWindow // hashed credential + resource -> current primary-limit window
	routePatterns     []string                  // every pattern registered via route(), for fidelity enumeration
	externalURL       string                    // BLEEPHUB_EXTERNAL_URL; overrides request-Host URL derivation
	// observedOrigin is the origin of the most recently served request, used to
	// render absolute hypermedia in out-of-request payloads (webhook deliveries)
	// when no external URL is configured.
	observedOrigin        observedOriginBox
	pagesJekyllExecutable string
	identity              identityConfig
	identityStateKey      []byte // random per-process HMAC key for OAuth state cookies
	build                 BuildInfo
	// monitoringTokenDigest authenticates the deployment-only observation
	// endpoint. Only the SHA-256 digest survives startup configuration parsing.
	monitoringTokenDigest *[sha256.Size]byte
	// clockNow is injected by deterministic tests/simulators; production leaves
	// it nil. clockMu keeps replacing a test clock safe while owned workers wind
	// down.
	clockMu            sync.RWMutex
	clockNow           func() time.Time
	webhookClientsOnce sync.Once
	webhookClients     [2]*http.Client // [verifying TLS, insecure_ssl=1]
	webhookPoolOnce    sync.Once
	webhookPool        *webhookDispatcher
	// responseObserver, when set before ListenAndServe, sees every
	// request/response pair; the test harness uses it to validate /api/v3 shapes
	// against the vendored OpenAPI description. nil costs nothing.
	responseObserver func(req *http.Request, status int, header http.Header, body []byte)
	// background tracks the goroutines the server starts so shutdown waits for
	// them rather than leaving them writing to an unread store.
	background sync.WaitGroup
	// lifetime is cancelled when the server stops serving. Work a request starts
	// but does not wait for (post-push compaction) derives from this, not the
	// request context, so shutdown can stop it.
	lifetime      context.Context
	stopServing   context.CancelFunc
	gitCompaction *gitCompactionScheduler
}

func (s *Server) currentTime() time.Time {
	if s != nil {
		s.clockMu.RLock()
		clockNow := s.clockNow
		s.clockMu.RUnlock()
		if clockNow != nil {
			return clockNow().UTC()
		}
	}
	return time.Now().UTC()
}

// goBackground runs fn as an owned goroutine that shutdown waits for.
func (s *Server) goBackground(fn func()) {
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		fn()
	}()
}

// BuildInfo identifies the immutable application artifact serving a request.
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

// serverConstruction holds the values newServerState needs to build complete
// server state. Production and tests share it so a test never exercises a
// structurally different Server production can't create.
type serverConstruction struct {
	byteStore              store.ActionsByteStore
	dataDir                string
	maxConcurrentWorkflows int
	externalURL            string
	pagesJekyllExecutable  string
	identity               identityConfig
	build                  BuildInfo
}

func newServerState(addr string, logger zerolog.Logger, construction serverConstruction) *Server {
	identityStateKey := make([]byte, 32)
	if _, err := rand.Read(identityStateKey); err != nil {
		panic(fmt.Sprintf("generate identity state HMAC key: %v", err))
	}
	lifetime, stopServing := context.WithCancel(context.Background())
	s := &Server{
		lifetime:               lifetime,
		stopServing:            stopServing,
		gitCompaction:          newGitCompactionScheduler(),
		addr:                   addr,
		mux:                    http.NewServeMux(),
		logger:                 logger,
		store:                  store.NewStore(),
		actionCache:            NewActionCache(),
		artifactStore:          store.NewArtifactStoreWithByteStore(construction.dataDir, construction.byteStore),
		localByteStore:         store.NewLocalByteStore(construction.dataDir),
		metrics:                NewMetrics(),
		maxConcurrentWorkflows: construction.maxConcurrentWorkflows,
		registryUploads:        map[string]*containerRegistryUpload{},
		rateLimits:             map[string]*apiRateWindow{},
		externalURL:            construction.externalURL,
		pagesJekyllExecutable:  construction.pagesJekyllExecutable,
		identity:               construction.identity,
		identityStateKey:       identityStateKey,
		build:                  construction.build,
	}
	s.store.ActionsArtifacts = s.artifactStore
	// Route the store's error logging through the structured logger.
	s.store.Logger = logger
	s.actions = s.newActionsEngine()
	// Build the GraphQL resolver here (like the Actions engine) so every
	// constructed server can serve POST /api/graphql without a nil resolver.
	s.graphql = s.newGraphQLResolver()
	return s
}

// newActionsEngine wires the Actions engine to this server's store and injected
// seams. Re-invoked by NewServer when a test-injected byte store replaces the
// artifact store before anything runs.
func (s *Server) newActionsEngine() *actions.Engine {
	return actions.NewEngine(actions.Config{
		Store:     s.store,
		Artifacts: s.artifactStore,
		Logger:    s.logger,
		Metrics:   s.metrics,
		Addr:      s.addr,
		Events:    s,
		// GITHUB_TOKEN minting stays in the auth layer: mint and verify share
		// the runner MAC key, and signature drift would break every job.
		MintJobToken: func(scopeID string, wf *store.Workflow, jd *store.JobDef) string {
			return makeJobJWT(scopeID, wf.RepoFullName, s.resolveJobTokenPermissions(wf, jd))
		},
		RepoEventPayload: func(repo *store.Repo) map[string]interface{} {
			return repoPayload(repo, s.publicOrigin())
		},
		Now: s.currentTime,
		Go:  s.goBackground,
		// Server-side housekeeping on the engine's minute tick.
		OnScheduleTick: []func(time.Time){
			func(now time.Time) {
				if err := s.store.ReapExpiredLoginSessions(now); err != nil {
					s.logger.Error().Err(err).Msg("expired login-session reap failed")
				}
			},
			s.reconcileOrgInvitationsSafely,
		},
		CompletedJobRetention: runnerTokenTTL,
	})
}

// reconcileOrgInvitationsSafely runs the org-invitation state machine on a
// background tick so a GET never has to (STORE-034). A durable write failure
// panics through the Must* helpers; this goroutine has no recover middleware,
// so catch it, reload durable state, and continue.
func (s *Server) reconcileOrgInvitationsSafely(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			if pf, ok := r.(*store.PersistenceFailure); ok {
				s.logger.Error().Err(pf).Msg("org-invitation reconcile persist failed; reloading")
				if err := s.store.ReloadFromPersistence(); err != nil {
					s.logger.Error().Err(err).Msg("reload after org-invitation reconcile failure")
				}
				return
			}
			panic(r)
		}
	}()
	s.store.ReconcileAllOrgInvitations(now)
}

// route registers a handler and records its "METHOD /path" pattern so the
// registered surface can be enumerated directly (e.g. against GitHub's API
// definition). The catch-all is deliberately not registered here, so a missing
// route is a visible gap in RegisteredRoutes() rather than swallowed by it.
func (s *Server) route(pattern string, handler http.HandlerFunc) {
	s.routePatterns = append(s.routePatterns, pattern)
	if strings.Contains(pattern, " /ui-data/") {
		handler = s.authenticateUIData(handler)
	}
	// /api/v3 routes feed the API insights stats; others pass through.
	s.mux.HandleFunc(pattern, s.nameSpan(pattern, s.instrumentAPIRoute(pattern,
		s.enforceEnterpriseIPAllowList(pattern, s.enforceFineGrainedPATResource(pattern,
			s.refuseInstallationTokenOnUserRoutes(pattern, handler))))))
}

// nameSpan renames the otelhttp span to the matched route template and records
// it as http.route; naming by template (not raw path) bounds span cardinality.
// A no-op when tracing is off.
func (s *Server) nameSpan(pattern string, next http.HandlerFunc) http.HandlerFunc {
	_, path, hasMethod := strings.Cut(pattern, " ")
	return func(w http.ResponseWriter, r *http.Request) {
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			span.SetName(pattern)
			if hasMethod {
				span.SetAttributes(semconv.HTTPRoute(path))
			}
		}
		next(w, r)
	}
}

// routeDispatch registers a dispatcher whose mux pattern is a wildcard Go's
// ServeMux can't disambiguate (e.g. `/releases/{p1}/{p2}` covering both
// `/releases/tags/{tag}` and `/releases/{release_id}/assets`). The mux matches
// dispatchPattern while routePatterns records the real endpoints, so
// RegisteredRoutes() enumerates usable operations, not the routing detail.
func (s *Server) routeDispatch(dispatchPattern string, handler http.HandlerFunc, servedPatterns ...string) {
	s.routePatterns = append(s.routePatterns, servedPatterns...)
	s.mux.HandleFunc(dispatchPattern, s.nameSpan(dispatchPattern, s.instrumentAPIRoute(dispatchPattern, s.enforceFineGrainedPATResource(dispatchPattern, handler))))
}

// authenticateUIData gives every browser-only adapter the same credential
// semantics, so a handler need not authenticate itself.
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

// NewServer creates a bleephub server with all routes registered.
//
// Env vars: BLEEPHUB_DATA_DIR (SQLite state directory) and BLEEPHUB_PERSIST
// ("true" enables SQLite-backed state). Persistence that fails to open is fatal.
//
// Persistence requires durable git storage (BLEEPHUB_GIT_DIR or
// BLEEPHUB_S3_BUCKET) and object-backed byte storage (BLEEPHUB_OBJECT_S3_BUCKET);
// reloading repo metadata against in-memory git would resurrect every repo
// empty, and SQLite persists metadata only, not byte content — so either
// combination is a startup error, not a silent degraded mode.
//
// In-flight workflow runs reload as cancelled (runner dispatch is
// process-local); browser sessions persist. Agent connections and the OIDC
// signing key stay process-local, so consumers re-fetch the JWKS after rotation.
func NewServer(addr string, logger zerolog.Logger, options ...ServerOption) *Server {
	maxWF := 10
	if v := os.Getenv("BLEEPHUB_MAX_WORKFLOWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxWF = n
		} else {
			// Warn rather than silently defaulting an invalid value.
			logger.Warn().Str("value", v).Int("using", maxWF).
				Msg("ignoring invalid BLEEPHUB_MAX_WORKFLOWS (want a positive integer)")
		}
	}
	dataDir := os.Getenv("BLEEPHUB_DATA_DIR")
	byteStore, err := store.NewActionsByteStoreFromEnv(context.Background())
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to initialize BLEEPHUB_OBJECT_S3_* byte storage")
	}
	s := newServerState(addr, logger, serverConstruction{
		byteStore:              byteStore,
		dataDir:                dataDir,
		maxConcurrentWorkflows: maxWF,
		externalURL:            strings.TrimRight(os.Getenv("BLEEPHUB_EXTERNAL_URL"), "/"),
		pagesJekyllExecutable:  store.CoalesceStr(os.Getenv("BLEEPHUB_PAGES_JEKYLL_EXECUTABLE"), "bleephub-pages-jekyll"),
		identity:               identityConfigFromEnv(),
		build:                  BuildInfo{Version: "development", Commit: "none", PublishedAt: "not-yet-published"},
	})
	if v := os.Getenv("BLEEPHUB_MAX_INFLIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			s.inFlightSlots = make(chan struct{}, n)
		} else {
			logger.Warn().Str("value", v).Msg("ignoring invalid BLEEPHUB_MAX_INFLIGHT (want a positive integer)")
		}
	}
	for _, option := range options {
		option(s)
	}
	// A test-injected byte store replaces the env-derived one here, before
	// persistence validation reads it.
	if s.injectedByteStore != nil {
		byteStore = s.injectedByteStore
		s.artifactStore = store.NewArtifactStoreWithByteStore(dataDir, byteStore)
		// Re-point the engine and store at the replaced artifact store; leaving
		// store.ActionsArtifacts on the discarded one diverges the two access
		// paths (ACT-099).
		s.store.ActionsArtifacts = s.artifactStore
		s.actions = s.newActionsEngine()
	}
	if err := s.identity.validate(); err != nil {
		logger.Fatal().Err(err).Msg("invalid Bleephub external identity configuration")
	}
	if err := validateShauthExternalURL(s.identity, s.externalURL); err != nil {
		logger.Fatal().Err(err).Msg("invalid Bleephub external identity configuration")
	}
	s.store.ObjectByteStore = byteStore
	s.store.Releases.ByteStore = byteStore
	if dataDir != "" {
		s.store.PackageDataDir = dataDir
	}

	persist := store.MustNewPersistence()
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
		s.logger.Info().Str("dialect", persist.Dialect.Name).Str("data_dir", dataDir).Msg("bleephub persistence enabled")
	}

	// Seed the default user only if none loaded from disk.
	if s.store.LookupUserByLogin("admin") == nil {
		s.store.SeedDefaultUser()
	}
	// Seed pre-registered GitHub Apps (BLEEPHUB_SEED_APPS / _FILE) so a
	// coordinate-only consumer can hold a fixed app id + key + org.
	if err := s.seedConfiguredApps(); err != nil {
		logger.Fatal().Err(err).Msg("failed to seed configured GitHub Apps")
	}
	// The instance's own enterprise account, keyed on the same enterprise the
	// REST settings and GraphQL account describe.
	s.seedPrimaryEnterprise()
	s.registerRoutes()
	return s
}

func validatePersistentServerStorage(serviceByteStoreReady bool) error {
	if gitstore.GitDataDir() == "" && !gitstore.IsS3GitStorage() {
		return errors.New("persistence is enabled (BLEEPHUB_PERSIST=true) but git storage is in-memory: " +
			"repo metadata would survive a restart while every git repo reloads empty. " +
			"Configure durable git storage (BLEEPHUB_GIT_DIR=<dir> or BLEEPHUB_S3_BUCKET=<bucket>) or disable persistence")
	}
	if !serviceByteStoreReady {
		return errors.New("persistence is enabled (BLEEPHUB_PERSIST=true) but service byte storage is not object-backed: " +
			"GitHub Actions artifacts, dependency caches, runner logs, release assets, package files, container-registry blobs, CodeQL database archives, CodeQL variant-analysis query packs, artifact attestation bundles, and Git LFS objects require BLEEPHUB_OBJECT_S3_BUCKET")
	}
	return nil
}

func (s *Server) registerRoutes() {
	// Health check
	s.route("GET /health", s.handleHealth)
	s.route("GET /ready", s.handleReady)
	s.route("GET /monitoring/observation", s.handleMonitoringObservation)

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
	s.registerGHBranchProtectionPatternRoutes()

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
	s.registerGHWikiRoutes()
	s.registerGHUIBootstrapRoutes()
	s.registerGHDiscussionsUIDataRoutes()
	s.registerGHPullsUIDataRoutes()
	s.registerGHPinnedRoutes()
	s.registerGHAchievementsRoutes()
	s.registerGHProfileReadmeRoutes()
	s.registerGHAccountSettingsRoutes()
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
	s.registerGHESAdminStatsRoutes()
	s.registerGHESManageRoutes()
	s.registerGHProjectsV2Routes()
	s.registerGHAttestationsRoutes()
	s.registerGHOrgArtifactMetadataRoutes()
	s.registerGHCopilotRoutes()
	// Copilot subscription policy, seat activity and usage (gh_copilot_policy.go)
	s.registerGHCopilotPolicyRoutes()
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
	s.registerGHEnterpriseRemainderRoutes()

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
	s.registerAvatarRoutes()
	s.registerGHCodesOfConductRoutes()
	s.registerGHGlobalAdvisoriesRoutes()
	s.registerGHClassroomRoutes()
	s.registerGHClassroomWebRoutes()
	s.registerGHMarketplaceRoutes()
	// Marketplace pending-change lifecycle, categories and listing profiles
	// (gh_marketplace_lifecycle.go)
	s.registerGHMarketplaceLifecycleRoutes()
	// GitHub Sponsors browser surface + webhooks (gh_sponsors.go)
	s.registerGHSponsorsRoutes()
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
	// registerUI mounts /ui/ and the root redirect only in the UI-embedded
	// build; the noui stub registers neither (a redirect to an unserved /ui/
	// would 307 into a guaranteed 404, CORE-012).
	s.registerUI()

	// Catch-all: tries smart HTTP git protocol, then logs unmatched
	s.mux.HandleFunc("/", s.handleCatchAll)
}

func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	if s.tryHandleGitRequest(w, r) {
		return
	}

	// Git LFS: /{owner}/{repo}[.git]/info/lfs/... (batch API, transfers, locks)
	if s.tryHandleLFSRequest(w, r) {
		return
	}

	// Codeload-style source archive downloads (legacy.tar.gz / legacy.zip)
	if s.tryHandleArchiveRequest(w, r) {
		return
	}

	// Raw file contents (download_url/raw_url targets).
	if s.tryHandleRawRequest(w, r) {
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

// handleHealth is the anonymous liveness probe. It carries no build identity or
// enterprise slug: exposing the commit SHA and tenant slug on an anonymous
// endpoint is a fingerprint (CORE-016). Build identity lives behind the
// authenticated internal status endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "bleephub",
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.PersistenceReady(ctx); err != nil {
		s.logger.Warn().Err(err).Msg("readiness check failed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleInternalMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}

func (s *Server) handleInternalStatus(w http.ResponseWriter, r *http.Request) {
	s.store.Mu.RLock()
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
	s.store.Mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"active_workflows":  activeWfs,
		"jobs_by_status":    jobsByStatus,
		"connected_runners": sessions,
		"uptime_seconds":    int(time.Since(s.metrics.StartedAt).Seconds()),
		// Build identity lives here, behind site-admin auth, not on /health (CORE-016).
		"enterprise_slug": s.enterpriseSlug(),
		"version":         s.build.Version,
		"commit":          s.build.Commit,
		"published_at":    s.build.PublishedAt,
	})
}

func (s *Server) handleInternalStorage(w http.ResponseWriter, r *http.Request) {
	persistenceBackend := "none"
	dialectName := ""
	if s.store.Persist != nil {
		persistenceBackend = s.store.Persist.Dialect.Name
		dialectName = s.store.Persist.Dialect.Name
	}

	gitBackend := "memory"
	gitDetails := map[string]string{}
	gitDir := gitstore.GitDataDir()
	if gitstore.IsS3GitStorage() {
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

// ListenAndServe runs the server until ctx is cancelled, then drains. The drain
// is bounded: long-poll routes (the runner broker) would otherwise never let
// every connection close. Requests past the bound are cut.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.startGitSSH(ctx); err != nil {
		return err
	}
	// Adopt the storage layer's "loose tier is full" signal here, not at
	// construction: the handler is process-global but a Server is not, and a
	// test process stands up many. Only a serving server owns it, and hands it
	// back below.
	gitstore.SetCompactionRequestHandler(func(repo string, stor gitStorage.Storer) {
		s.scheduleGitCompaction(repo, stor)
	})
	defer gitstore.SetCompactionRequestHandler(nil)
	// Bind the git object store's S3 I/O to the server lifetime so a slow or dead
	// store cancels in-flight calls on shutdown instead of detaching and holding
	// per-repo git locks past the drain.
	if fs, _ := gitstore.GetS3FS(ctx); fs != nil {
		fs.SetBaseContext(ctx)
	}
	s.startObjectReaper(ctx)
	s.actions.Start(ctx)
	// Re-run migration exports a dead process left "exporting"; nothing else
	// claims them.
	s.resumeMigrationExports()
	// Likewise requeue GEI (data-in) migrations left mid-flight.
	s.resumeGEIMigrations()
	handler := s.requestHandler()

	srv := &http.Server{
		Addr:           s.addr,
		Handler:        handler,
		MaxHeaderBytes: 1 << 20,
		// Bound only the header read (slowloris); a fixed Read/WriteTimeout caps
		// the whole body and cuts off large git pushes and artifact transfers.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Route net/http's connection-level errors through the structured logger
		// instead of the stdlib default.
		ErrorLog: newStdLogBridge(s.logger),
	}

	host, port, _ := net.SplitHostPort(s.addr)
	if host == "" {
		host = "localhost"
	}

	// TLS is all-or-nothing: treating a half-configured pair as "no TLS" would
	// silently serve plaintext on a port the operator believes is encrypted, so
	// refuse to start instead.
	certFile := os.Getenv("BPH_TLS_CERT")
	keyFile := os.Getenv("BPH_TLS_KEY")
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("BPH_TLS_CERT and BPH_TLS_KEY must be set together (cert %s, key %s)",
			presenceLabel(certFile), presenceLabel(keyFile))
	}
	serveErr := make(chan error, 1)
	if certFile != "" {
		// Fail on an unreadable cert at startup, not at the first handshake.
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
	// Stop owned work before the drain so shutdown is bounded by the grace
	// period, not by whatever timeout that work set itself.
	s.stopServing()
	drain, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(drain)
	s.background.Wait()
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// requestHandler is the one production HTTP pipeline; in-process tests use it
// too, so the middleware chain cannot diverge from the live listener.
func (s *Server) requestHandler() http.Handler {
	inner := s.originMiddleware(s.prefixStripMiddleware(s.replicaRefreshMiddleware(s.internalAuthMiddleware(s.repoRedirectMiddleware(s.mux)))))
	ghWrapped := s.ghHeadersMiddleware(inner)
	observed := ghWrapped
	if s.responseObserver != nil {
		observed = s.observeMiddleware(ghWrapped)
	}
	// Compression sits outside the observer so the shape validator and ETag
	// layer keep seeing identity bytes; only the wire is gzipped.
	compressed := compressionMiddleware(observed)
	bounded := s.requestBodyLimitMiddleware(compressed)
	secured := s.securityHeadersMiddleware(bounded)
	// slowBodyGuard sits outermost so its ResponseController resolves to the
	// base net/http writer (whose SetReadDeadline coordinates with connection
	// management) without relying on every inner wrapper implementing Unwrap.
	return slowBodyGuard(bodyReadInactivityTimeout,
		otelhttp.NewHandler(s.recoverMiddleware(s.loggingMiddleware(s.adminHostMiddleware(s.inFlightLimitMiddleware(s.durabilityBarrierMiddleware(secured))))), "bleephub"))
}

// inFlightLimitMiddleware bounds concurrent non-byte-transfer requests to the
// BLEEPHUB_MAX_INFLIGHT slots (a no-op when unset). Under saturation it sheds
// load with a 503 + Retry-After rather than admitting unbounded goroutines that
// would queue on the global store lock. Git and artifact byte transfers are
// exempt so a long clone or upload never holds a slot.
func (s *Server) inFlightLimitMiddleware(next http.Handler) http.Handler {
	if s.inFlightSlots == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isByteTransferRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.inFlightSlots <- struct{}{}:
			defer func() { <-s.inFlightSlots }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			writeGHError(w, http.StatusServiceUnavailable, "Server is at capacity; retry shortly")
		}
	})
}

// isByteTransferRoute reports whether a path streams bytes (git smart-HTTP, LFS,
// the runner artifact/cache protocol, release-asset up/download). These are
// long-lived and exempt from the in-flight cap.
func isByteTransferRoute(path string) bool {
	for _, marker := range []string{
		"/info/refs", "git-upload-pack", "git-receive-pack", ".git/",
		"/info/lfs", "/_apis/", "/releases/assets/", "/releases/download/",
	} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

// bodyReadInactivityTimeout is a sliding per-read deadline (reset before every
// read), not a total cap: a client making steady progress on a large git pack
// never trips it, while a slow-body Slowloris is dropped. A fixed ReadTimeout
// would cap the whole body, so the server leaves it unset and closes only the
// header-read blind spot here (CORE-010).
const bodyReadInactivityTimeout = 60 * time.Second

// slowBodyGuard wraps each request body so a stalled read trips a sliding
// inactivity deadline on the connection.
func slowBodyGuard(idle time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &inactivityDeadlineBody{rc: http.NewResponseController(w), body: r.Body, idle: idle}
		}
		next.ServeHTTP(w, r)
	})
}

// inactivityDeadlineBody resets a read deadline before every Read.
type inactivityDeadlineBody struct {
	rc   *http.ResponseController
	body io.ReadCloser
	idle time.Duration
}

func (b *inactivityDeadlineBody) Read(p []byte) (int, error) {
	// Best-effort: on a transport without read deadlines this errors and the
	// guard degrades to the configured header/idle timeouts.
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	return b.body.Read(p)
}

func (b *inactivityDeadlineBody) Close() error { return b.body.Close() }

// uiContentSecurityPolicy locks the SPA to its own origin: 'self' suffices for
// script-src (no inline script), 'unsafe-inline' for styles (React inline
// styles), 'wasm-unsafe-eval' for the crypto bundle, https: img-src for remote
// avatars, and frame-ancestors 'none' against clickjacking.
//
// form-action is deliberately unrestricted: the sign-out form redirects
// cross-origin to the OIDC end_session endpoint, and Chromium applies
// form-action to that redirect chain, so 'self' would break federated logout.
const uiContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'wasm-unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'"

// securityHeadersMiddleware sets baseline security headers on every response.
// Handlers needing stricter values (identity pages, Pages sandbox) run after
// this and win.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		// The SPA (and its /ui-data adapters) get a CSP and framing lock; other
		// surfaces keep the headers their handlers set.
		if strings.HasPrefix(r.URL.Path, "/ui") {
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", uiContentSecurityPolicy)
		}
		next.ServeHTTP(w, r)
	})
}

const maxStructuredRequestBody = 32 << 20

// requestBodyLimitMiddleware bounds structured (JSON/form) request bodies with
// MaxBytesReader (a 413 on overflow). Byte-transfer protocols (Git, artifacts,
// packages, registry uploads) keep their route-specific streaming limits, since
// a whole-request cap would truncate legitimate large transfers.
func (s *Server) requestBodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
		switch contentType {
		case "application/json", "application/graphql", "application/x-www-form-urlencoded", "multipart/form-data":
			r.Body = http.MaxBytesReader(w, r.Body, maxStructuredRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

// shutdownGrace bounds the drain: long enough for an ordinary request or git
// push, short enough to stay under Docker/Kubernetes' 10s SIGKILL default.
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

// internalAuthMiddleware gates the operator-facing /internal/* surface (no
// GitHub API equivalent). It requires a valid token and injects the resolved
// user into the context for ghUserFromContext. /health stays open for probes.
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
		// Require site admin for the whole /internal/ prefix. Gating a single
		// route table would leave routes registered outside it (e.g.
		// /internal/exec/*, which dispatches containers to the runner fleet
		// without its own authz) open to any token holder — arbitrary code
		// execution. The check belongs here where nothing registers around it.
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

// internalTokenUser resolves the user for a PAT on the internal surface, or nil.
// Installation/OAuth tokens (ghs_/gho_/ghu_) are not accepted: the surface is
// operator-only.
func (s *Server) internalTokenUser(r *http.Request) *store.User {
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

// prefixStripMiddleware removes path segments before the known API prefixes:
// the runner prepends the tenant URL path, so a call arrives as
// /owner/repo/_apis/... rather than /_apis/....
func (s *Server) prefixStripMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
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
		withTraceContext(s.logger.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.status).
			Dur("dur", time.Since(start)), r.Context()).
			Msg("request")
	})
}

// withTraceContext stamps the active span's trace_id/span_id onto a log event
// so the line correlates with its trace (CORE-007). A spanless context is a
// no-op.
func withTraceContext(evt *zerolog.Event, ctx context.Context) *zerolog.Event {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return evt.Str("trace_id", sc.TraceID().String()).Str("span_id", sc.SpanID().String())
	}
	return evt
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// writeJSON marshals the body before writing the status, so an unencodable
// value becomes a 500 rather than a committed 200 with a truncated body.
// Content-Length is left to net/http, since some handlers write further bytes
// after calling this.
//
// checkIfMatch enforces optimistic concurrency (STORE-016): an If-Match must
// equal the resource's strong ETag (writeJSON's sha256(body)) or the write is
// rejected 412, so a stale client cannot clobber a concurrent update. An absent
// If-Match is unconditional. Pass the present JSON representation; returns false
// once it has written the 412.
func checkIfMatch(w http.ResponseWriter, r *http.Request, current interface{}) bool {
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		return true
	}
	body, err := json.Marshal(current)
	if err != nil {
		return true
	}
	sum := sha256.Sum256(body)
	etag := fmt.Sprintf(`"%x"`, sum)
	if etagMatches(ifMatch, etag) {
		return true
	}
	writeGHError(w, http.StatusPreconditionFailed, "Precondition Failed")
	return false
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		stdlog.Printf("writeJSON: json.Marshal of response body failed: %v", err)
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
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// writeJSONCreated writes a 201 with a Location header pointing at the new
// resource (typically the body's "url"). Location is set before writeJSON,
// which commits status and body in one shot.
func writeJSONCreated(w http.ResponseWriter, location string, v interface{}) {
	if location != "" {
		w.Header().Set("Location", location)
	}
	writeJSON(w, http.StatusCreated, v)
}

// jsonStringField reads a string field from a JSON-shaped map, feeding
// writeJSONCreated a Location from the same map that becomes the body.
func jsonStringField(v interface{}, key string) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// recoverMiddleware turns a panicking handler into a 500 (net/http's own
// recover writes nothing and drops the connection with no record). It sits
// inside otelhttp so the span can record the error, and outside everything else
// so any panic below is caught. It bounds blast radius but does not make a
// panic harmless: a handler that panics holding the store lock still leaks it.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A client that hung up mid-write is not a server fault.
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			if _, ok := recovered.(*store.PersistenceFailure); ok {
				if reloadErr := s.store.ReloadFromPersistence(); reloadErr != nil {
					s.logger.Error().Err(reloadErr).Msg("failed to restore durable state after persistence error")
				}
			}

			span := trace.SpanFromContext(r.Context())
			err := fmt.Errorf("panic: %v", recovered)
			span.RecordError(err)
			span.SetStatus(codes.Error, "handler panicked")

			withTraceContext(s.logger.Error().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()), r.Context()).
				Msg("handler panicked")

			writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		}()
		next.ServeHTTP(w, r)
	})
}

// newStdLogBridge adapts zerolog to the *log.Logger net/http wants for ErrorLog.
func newStdLogBridge(logger zerolog.Logger) *stdlog.Logger {
	return stdlog.New(stdLogWriter{logger: logger}, "", 0)
}

type stdLogWriter struct{ logger zerolog.Logger }

func (w stdLogWriter) Write(p []byte) (int, error) {
	w.logger.Error().Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}

// redactedQuery replaces every query value unless its name is on a small
// no-secret allowlist. The allowlist is inverted by design: OAuth/device-flow
// params (code, client_secret, access_token, device_code) reach telemetry
// through the log bridge, and a denylist would leak every param added later.
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
