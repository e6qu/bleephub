package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// AdminToken returns the seeded admin token, which MUST be supplied via
// BLEEPHUB_ADMIN_TOKEN. There is no default: the token is a credential, so the
// sim fails loudly rather than seeding a guessable value (and a hardcoded value
// would be GitHub-PAT-shaped, tripping secret scanners). Consumers and test
// harnesses set the env var explicitly.
func AdminToken() string {
	v := os.Getenv("BLEEPHUB_ADMIN_TOKEN")
	if v == "" {
		zlog.Fatal().Msg("bleephub: BLEEPHUB_ADMIN_TOKEN is required (the admin token has no default — set it explicitly)")
	}
	return v
}

// LoadJSON is a thin wrapper to keep error wrapping uniform across persistence loaders.
func LoadJSON(raw []byte, v interface{}) error { return json.Unmarshal(raw, v) }

// ReserveRunID hands out the next workflow run ID and persists the
// counter. Artifacts persist on disk keyed by their run ID, so the
// sequence must never restart from 1 after a reload — a new run #1
// would inherit the previous epoch's run-1 artifacts.
func (st *Store) ReserveRunID() int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	id := st.NextRunID
	if st.Persist != nil {
		reserved, err := st.Persist.AllocateCounterValue("next_run_id", int64(id))
		if err != nil {
			panic(&PersistenceFailure{Op: "counter", Bucket: "counters", Key: "next_run_id", Err: err})
		}
		id = int(reserved)
	}
	st.NextRunID = id + 1
	return id
}

// reserveGlobalID hands out the next value of a durable global-entity ID
// counter, mirroring ReserveRunID. Core entities (orgs, users, repos, teams,
// issues) previously minted their global ID from the in-memory NextX field
// alone, so two dqlite replicas could mint the same ID and the second write
// would silently overwrite the first. Routing allocation through
// AllocateCounterValue makes the sequence agree across replicas. The in-memory
// NextX (rebuilt as max+1 on load) supplies the minimum, so single-node and
// in-memory stores keep their sequential NextX++ semantics. Caller holds st.Mu.
func (st *Store) ReserveGlobalID(name string, next *int) int {
	id := *next
	if st.Persist != nil {
		reserved, err := st.Persist.AllocateCounterValue(name, int64(id))
		if err != nil {
			panic(&PersistenceFailure{Op: "counter", Bucket: "counters", Key: name, Err: err})
		}
		id = int(reserved)
	}
	*next = id + 1
	return id
}

// ReserveLogID returns an object-store-safe log identifier. The counter is
// durable so a service replacement cannot reuse an existing logs/{id} key and
// overwrite a completed job's bytes.
func (st *Store) ReserveLogID() int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	id := st.NextLog
	if st.Persist != nil {
		reserved, err := st.Persist.AllocateCounterValue("next_log_id", int64(id))
		if err != nil {
			panic(&PersistenceFailure{Op: "counter", Bucket: "counters", Key: "next_log_id", Err: err})
		}
		id = int(reserved)
	}
	st.NextLog = id + 1
	return id
}

// ReserveWorkflowRunNumber returns the next number for one workflow file.
// GitHub numbers each workflow independently; the run ID remains the global
// identifier. The durable counter also makes concurrent replicas agree.
func (st *Store) ReserveWorkflowRunNumber(wf *Workflow) int {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	minimum := 1
	sameWorkflow := func(existing *Workflow) bool {
		if existing == nil || existing.RepoFullName != wf.RepoFullName {
			return false
		}
		if wf.WorkflowFileID != 0 && existing.WorkflowFileID != 0 {
			return existing.WorkflowFileID == wf.WorkflowFileID
		}
		if wf.WorkflowFilePath != "" && existing.WorkflowFilePath != "" {
			return existing.WorkflowFilePath == wf.WorkflowFilePath
		}
		return existing.Name == wf.Name
	}
	for _, existing := range st.Workflows {
		if sameWorkflow(existing) && existing.RunNumber >= minimum {
			minimum = existing.RunNumber + 1
		}
	}
	for _, attempts := range st.WorkflowAttempts {
		for _, existing := range attempts {
			if sameWorkflow(existing) && existing.RunNumber >= minimum {
				minimum = existing.RunNumber + 1
			}
		}
	}
	if st.Persist == nil {
		return minimum
	}
	identity := wf.WorkflowFilePath
	if wf.WorkflowFileID != 0 {
		identity = strconv.FormatInt(wf.WorkflowFileID, 10)
	}
	if identity == "" {
		identity = wf.Name
	}
	counter := "workflow_run_number:" + wf.RepoFullName + ":" + identity
	number, err := st.Persist.AllocateCounterValue(counter, int64(minimum))
	if err != nil {
		panic(&PersistenceFailure{Op: "counter", Bucket: "counters", Key: counter, Err: err})
	}
	return int(number)
}

// User represents a GitHub user account.
type User struct {
	ID           int             `json:"id"`
	NodeID       string          `json:"node_id"`
	Login        string          `json:"login"`
	Name         string          `json:"name"`
	Email        string          `json:"email"`
	AvatarURL    string          `json:"avatar_url"`
	Bio          string          `json:"bio"`
	Type         string          `json:"type"`
	SiteAdmin    bool            `json:"site_admin"`
	Suspended    bool            `json:"suspended,omitempty"`
	StarredRepos map[string]bool `json:"starred_repos,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	// user-surface profile fields (PATCH /user), email addresses, and
	// account-level interaction limits.
	Blog                   string      `json:"blog,omitempty"`
	Company                string      `json:"company,omitempty"`
	Location               string      `json:"location,omitempty"`
	TwitterUsername        string      `json:"twitter_username,omitempty"`
	Hireable               *bool       `json:"hireable,omitempty"`
	Emails                 []UserEmail `json:"emails,omitempty"`
	InteractionLimit       string      `json:"interaction_limit,omitempty"`
	InteractionLimitExpiry *time.Time  `json:"interaction_limit_expiry,omitempty"`
	PasswordHash           string      `json:"password_hash,omitempty"`
	// ExternalIdentities binds the account to the stable (issuer, subject)
	// pairs its federated providers guarantee, so a mutable provider username
	// cannot re-key the account and one provider cannot overwrite another's
	// grant by logging in last.
	ExternalIdentities []ExternalIdentity `json:"external_identities,omitempty"`
	// SCIMManagedByOrg names the organization whose SCIM provisioning owns this
	// account, when set. Only that org's SCIM may mutate the account's global
	// login/name/email; an account provisioned outside SCIM (empty) is never
	// adopted or rewritten by an org's SCIM, which would otherwise let any org
	// owner rename or re-home an arbitrary global account.
	SCIMManagedByOrg string `json:"scim_managed_by_org,omitempty"`
}

// ExternalIdentity is one federated provider's stable handle on an account:
// the issuer and subject pair the provider guarantees immutable, unlike the
// mutable username it also presents.
type ExternalIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// UserEmail is one email address on a user account, matching GitHub's
// `email` schema (primary/verified/visibility).
type UserEmail struct {
	Email      string `json:"email"`
	Primary    bool   `json:"primary"`
	Verified   bool   `json:"verified"`
	Visibility string `json:"visibility,omitempty"` // "public", "private", or "" (null)
}

// Token represents a personal access token.
type Token struct {
	// Value is returned exactly once when a token is minted and is retained
	// only by the live process that minted it. Persistence keys tokens by a
	// keyed digest and never serializes the bearer credential itself.
	Value               string `json:"-"`
	UserID              int
	Scopes              string
	CreatedAt           time.Time
	FineGrained         bool              `json:"fine_grained,omitempty"`
	FineGrainedID       int               `json:"fine_grained_id,omitempty"`
	Name                string            `json:"name,omitempty"`
	ResourceOwner       string            `json:"resource_owner,omitempty"`
	RepositorySelection string            `json:"repository_selection,omitempty"`
	RepositoryIDs       []int             `json:"repository_ids,omitempty"`
	Permissions         OrgPATPermissions `json:"permissions,omitempty"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	// Impersonation marks a GHES site-admin impersonation OAuth token. GHES
	// permits at most one active impersonation authorization per user and
	// exposes it through /admin/users/{username}/authorizations.
	Impersonation bool   `json:"impersonation,omitempty"`
	Note          string `json:"note,omitempty"`
	NoteURL       string `json:"note_url,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}

func (st *Store) tokenMapKey(value string) string {
	if st.Persist == nil {
		return value
	}
	return st.Persist.opaqueLookupKey("tokens", value)
}

// tokenByValueLocked resolves a presented bearer without retaining or
// persisting a reversible copy. Callers must hold at least st.Mu.RLock.
//
// It looks up only under the derived digest key. The presented value is never
// probed raw: mint and restore both index the token by tokenMapKey(value), so
// a raw probe would only ever match a stored digest — i.e. it would let the
// persisted row key be presented as the credential.
func (st *Store) tokenByValueLocked(value string) (*Token, string) {
	key := st.tokenMapKey(value)
	return st.Tokens[key], key
}

func (st *Store) PersistTokenLocked(token *Token) {
	if st.Persist != nil {
		st.Persist.MustPut("tokens", token.Value, token)
	}
}

func (st *Store) DeleteTokenMapKeyLocked(mapKey string) {
	batch := NewPersistBatch(st.Persist)
	st.deleteTokenMapKeyBatchLocked(batch, mapKey)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "tokens", Err: err})
	}
}

// deleteTokenMapKeyBatchLocked stages a PAT token removal into batch so a
// multi-credential revoke commits every deleted token in one transaction
// (STORE-001/002). Callers hold st.Mu.
func (st *Store) deleteTokenMapKeyBatchLocked(batch *PersistBatch, mapKey string) {
	delete(st.Tokens, mapKey)
	batch.Delete("tokens", mapKey)
}

// DeviceCode represents a pending device authorization flow.
type DeviceCode struct {
	Code          string
	UserCode      string
	ClientID      string
	Scopes        string
	Token         string
	UserID        int
	AppID         int
	OAuthClientID string
	ApprovedAt    time.Time
	ExpiresAt     time.Time
}

// RepoAutolink represents a GitHub autolink reference configured on a repository.
type RepoAutolink struct {
	ID             int       `json:"id"`
	NodeID         string    `json:"node_id"`
	RepoKey        string    `json:"-"`
	KeyPrefix      string    `json:"key_prefix"`
	URLTemplate    string    `json:"url_template"`
	IsAlphanumeric bool      `json:"is_alphanumeric"`
	CreatedAt      time.Time `json:"created_at"`
}

// WikiPage is a single markdown page in a repository's wiki. Real GitHub backs
// wikis with a separate `<repo>.wiki.git` repository and exposes no REST API;
// the simulator uses a dedicated per-repo page store keyed by slug instead.
type WikiPage struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	RepoKey   string    `json:"-"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RepoInvitation represents a pending invitation to collaborate on a repository.
type RepoInvitation struct {
	ID           int       `json:"id"`
	NodeID       string    `json:"node_id"`
	RepoKey      string    `json:"-"`
	InviteeLogin string    `json:"invitee_login,omitempty"`
	InviteeEmail string    `json:"invitee_email,omitempty"`
	InviterID    int       `json:"inviter_id"`
	Permissions  string    `json:"permissions"`
	CreatedAt    time.Time `json:"created_at"`
	Status       string    `json:"status"`
}

// LoginSession is a browser session created by POST /login.
// It binds a session cookie to a user and carries the CSRF token
// embedded in the OAuth authorize consent form.
type LoginSession struct {
	UserID       int
	CSRFToken    string
	ExpiresAt    time.Time
	OIDCProvider string
	OIDCIssuer   string
	OIDCSubject  string
	OIDCSID      string
	OIDCIDToken  string
}

// GistFile is a single file inside a gist.
type GistFile struct {
	Filename string `json:"filename"`
	Type     string `json:"type"`
	Language string `json:"language"`
	RawURL   string `json:"raw_url"`
	Size     int    `json:"size"`
	Content  string `json:"content,omitempty"`
}

// GistHistory captures one revision of a gist.
type GistHistory struct {
	Version      string         `json:"version"`
	CommittedAt  time.Time      `json:"committed_at"`
	ChangeStatus map[string]int `json:"change_status"`
	URL          string         `json:"url"`
}

// Gist is a GitHub gist.
type Gist struct {
	ID          string               `json:"id"`
	NodeID      string               `json:"node_id"`
	Description string               `json:"description"`
	Public      bool                 `json:"public"`
	OwnerID     int                  `json:"owner_id"`
	Files       map[string]*GistFile `json:"files"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Comments    int                  `json:"comments"`
	CommentsURL string               `json:"comments_url"`
	HTMLURL     string               `json:"html_url"`
	URL         string               `json:"url"`
	ForksURL    string               `json:"forks_url"`
	CommitsURL  string               `json:"commits_url"`
	GitPullURL  string               `json:"git_pull_url"`
	GitPushURL  string               `json:"git_push_url"`
	History     []*GistHistory       `json:"history"`
	ForkOfID    string               `json:"fork_of_id,omitempty"`
	ForkIDs     []string             `json:"fork_ids,omitempty"`
}

// GistComment is a comment on a gist.
type GistComment struct {
	ID                int       `json:"id"`
	NodeID            string    `json:"node_id"`
	GistID            string    `json:"gist_id"`
	UserID            int       `json:"user_id"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	AuthorAssociation string    `json:"author_association"`
	URL               string    `json:"url"`
}

type persistedStarredGists struct {
	UserID int             `json:"user_id"`
	Stars  map[string]bool `json:"stars"`
}

// Store holds all in-memory state for bleephub.
type Store struct {
	Agents                       map[int]*Agent
	Sessions                     map[string]*Session
	Jobs                         map[string]*Job
	Users                        map[int]*User
	UsersByLogin                 map[string]*User
	UsersByExternalID            map[string]*User // issuer\x00subject → user (stable federated identity index)
	Tokens                       map[string]*Token
	DeviceCodes                  map[string]*DeviceCode
	AuthCodes                    map[string]*AuthCode     // OAuth web-flow codes
	LoginSessions                map[string]*LoginSession // _gh_sess cookie value → session
	OIDCLogoutClaims             map[string]time.Time     // replay key → expiry (ephemeral stores only)
	Repos                        map[int]*Repo
	ReposByName                  map[string]*Repo                       // "owner/name" → repo
	GitStorages                  map[string]gitStorage.Storer           // "owner/name" → go-git storage (memory or filesystem)
	Orgs                         map[int]*Org                           // id → org
	OrgsByLogin                  map[string]*Org                        // login → org
	Teams                        map[int]*Team                          // id → team
	TeamsBySlug                  map[string]*Team                       // "org/slug" → team
	Memberships                  map[string]*Membership                 // "org/user" → membership
	Issues                       map[int]*Issue                         // id → issue
	IssuesByRepo                 map[int]map[int]*Issue                 // repoID → number → issue (secondary index)
	Labels                       map[int]*IssueLabel                    // id → label
	Milestones                   map[int]*Milestone                     // id → milestone
	Comments                     map[int]*Comment                       // id → comment
	CommentCounts                map[string]int                         // "parentType\x1fparentID" → comment count (index)
	CommentsByParent             map[string][]*Comment                  // "parentType\x1fparentID" → comments (index, avoids scanning every comment per parent)
	IssueEvents                  map[int]*IssueEvent                    // id → issue event
	PullRequests                 map[int]*PullRequest                   // id → PR
	PullsByRepo                  map[int]map[int]*PullRequest           // repoID → number → PR (secondary index)
	PRReviews                    map[int]*PullRequestReview             // id → review
	PRReviewsByPR                map[int][]*PullRequestReview           // PR id → reviews (secondary index)
	Workflows                    map[string]*Workflow                   // id → workflow (run-level)
	WorkflowFiles                map[int64]*WorkflowFile                // id → workflow file (file-level)
	PendingMessages              []*TaskAgentMessage                    // messages awaiting delivery
	RepoSecrets                  map[string]map[string]*Secret          // "owner/repo" → name → secret
	RepoVariables                map[string]map[string]*ActionsVariable // "owner/repo" → NAME → variable
	RepoCollaborators            map[string]map[string]string           // "owner/repo" → login → permission (pull/push/admin)
	RepoAutolinks                map[string]map[int]*RepoAutolink       // "owner/repo" → id → autolink
	RepoWikiPages                map[string]map[string]*WikiPage        // "owner/repo" → slug → wiki page
	RepoInvitations              map[string]map[int]*RepoInvitation     // "owner/repo" → id → invitation
	RepoDeployKeys               map[string]map[int]*RepoDeployKey      // "owner/repo" → id → deploy key
	RepoSubscriptions            map[string]*RepoSubscription           // "userID:repoID" → subscription
	OrgSecrets                   map[string]map[string]*OrgSecret       // org login → NAME → org secret
	OrgVariables                 map[string]map[string]*ActionsVariable // org login → NAME → org variable
	EnvSecrets                   map[string]map[string]*Secret          // envScopeKey(repo, env) → NAME → secret
	EnvVariables                 map[string]map[string]*ActionsVariable // envScopeKey(repo, env) → NAME → variable
	TimelineRecords              map[string][]*TimelineRecord           // planID → runner-uploaded timeline records
	LogFiles                     map[int][]byte                         // logID → uploaded runner log content
	LogMasks                     map[string][]string                    // planID → exact values scrubbed from every log surface
	WorkflowAttempts             map[int][]*Workflow                    // runID → prior attempts (oldest first)
	RunnerGroups                 map[int]*RunnerGroup                   // org runner groups (global pool overlay)
	NextRunnerGroupID            int
	Hooks                        map[string][]*Webhook         // "owner/repo" → hooks
	OrgHooks                     map[string][]*Webhook         // org login → org-level hooks
	HookDeliveries               map[int][]*WebhookDelivery    // hookID → deliveries
	Apps                         map[int]*App                  // id → app
	AppsBySlug                   map[string]*App               // slug → app
	AppsByClientID               map[string]*App               // OAuth client_id → app
	OAuthApps                    map[string]*OAuthApp          // OAuth client_id → OAuth app (distinct from GitHub App)
	Installations                map[int]*Installation         // id → installation
	InstallationTokens           map[string]*InstallationToken // token value → token
	UserToServerTokens           map[string]*UserToServerToken // gho_/ghu_ token value → token
	RefreshTokens                map[string]*RefreshToken      // ghr_ token value → refresh token
	AppHookDeliveries            map[int][]*WebhookDelivery    // appID → app-level webhook deliveries
	ManifestCodes                map[string]int                // code → appID (one-time-use)
	CheckRuns                    map[int64]*CheckRun           // id → check run
	CheckSuites                  map[int64]*CheckSuite         // id → check suite
	CheckSuitePrefs              map[string][]*CheckSuitePref  // repoKey → autoTrigger prefs
	CommitStatuses               *CommitStatusStore            // commit status contexts per repo+ref
	CommitComments               *CommitCommentStore           // commit comments per repo/commit
	Reactions                    *ReactionStore                // reactions across all parent types
	Releases                     *ReleaseStore                 // release CRUD
	Deployments                  *DeploymentStore              // deployments + statuses + environments
	PRReviewComments             *PRReviewCommentStore         // PR review comments (inline / threads)
	Misc                         *MiscStore                    // long-tail surfaces
	ProjectsV2                   *ProjectV2Store               // GitHub Projects v2
	NotificationsState           map[int]*UserNotificationsState
	Rulesets                     map[int]*Ruleset
	RulesetSuites                map[int]*RulesetSuite
	ProjectClassic               map[int]*ProjectClassic                // id → project
	ProjectColumns               map[int]*ProjectColumn                 // id → column
	ProjectCards                 map[int]*ProjectCard                   // id → card
	UserMigrations               map[int]*UserMigration                 // id → user migration
	OrgMigrations                map[int]*OrgMigration                  // id → org migration
	Codespaces                   map[int]*Codespace                     // id → codespace
	CodespacesByName             map[string]*Codespace                  // name → codespace
	CodespaceSecrets             map[string]map[string]*CodespaceSecret // scope\x1fname → secret
	NextCodespaceID              int
	NextCodespaceSecretID        int
	LogLines                     map[string][]string     // jobID → captured console log lines
	Gists                        map[string]*Gist        // id → gist
	GistComments                 map[int]*GistComment    // id → gist comment
	StarredGists                 map[int]map[string]bool // userID → gistID → starred
	SecretScanningAlerts         map[int]*SecretScanningAlert
	SecretScanningAlertsByRepo   map[string]map[int]*SecretScanningAlert // repoKey → alertNumber → alert
	SecretScanningNextNumber     map[string]int                          // repoKey → next alert number
	CodeScanningAlerts           map[int]*CodeScanningAlert
	CodeScanningAlertsByRepo     map[string]map[int]*CodeScanningAlert // repoKey → alertNumber → alert
	CodeScanningNextNumber       map[string]int                        // repoKey → next alert number
	CodeScanningAnalyses         map[int]*CodeScanningAnalysis
	CodeScanningAnalysesByRepo   map[string]map[int]*CodeScanningAnalysis // repoKey → analysisID → analysis
	CodeScanningDefaultSetups    map[string]*CodeScanningDefaultSetup     // repoKey → default setup
	SARIFUploads                 map[string]*SARIFUpload                  // uploadID → upload
	DependabotAlerts             map[int]*DependabotAlert
	DependabotAlertsByRepo       map[string]map[int]*DependabotAlert         // repoKey → alertNumber → alert
	DependabotNextNumber         map[string]int                              // repoKey → next alert number
	DependabotSecrets            map[string]map[string]*DependabotSecret     // repoKey → name → secret
	DependabotOrgSecrets         map[string]map[string]*DependabotOrgSecret  // orgLogin → name → secret
	DependabotUserSecrets        map[string]map[string]*DependabotUserSecret // userLogin → name → secret
	DependabotRepositoryAccess   map[string][]int                            // orgLogin → repo IDs
	SecurityAdvisories           map[int]*SecurityAdvisory
	SecurityAdvisoriesByRepo     map[string]map[string]*SecurityAdvisory // repoKey → GHSA ID → advisory
	SecurityAdvisoryReports      map[int]*SecurityAdvisoryReport
	Packages                     map[int]*Package
	PackageVersions              map[int]*PackageVersion
	PackageFiles                 map[int]*PackageFile
	PackagesByOwnerKey           map[string]map[string]*Package  // ownerKey → PackageKey → package
	PackageVersionsByPackage     map[int]map[int]*PackageVersion // packageID → versionID → version
	PackageFilesByVersion        map[int]map[int]*PackageFile    // versionID → fileID → file
	PackageDataDir               string                          // directory for package file bytes
	ObjectByteStore              ActionsByteStore                // object storage for durable service bytes
	NextGistID                   int
	NextGistCommentID            int
	NextAgent                    int
	NextSecretScanningAlertID    int
	NextCodeScanningAlertID      int
	NextCodeScanningAnalysisID   int
	NextDependabotAlertID        int
	NextPackageID                int
	NextPackageVersionID         int
	NextPackageFileID            int
	NextMsg                      int64
	NextLog                      int
	NextReqID                    int64
	NextUser                     int
	NextRepo                     int
	NextOrg                      int
	NextTeam                     int
	NextIssue                    int
	NextLabel                    int
	NextMilestone                int
	NextComment                  int
	NextIssueEventID             int
	NextPR                       int
	NextPRReview                 int
	NextRunID                    int
	NextHookID                   int
	NextDeliveryID               int
	NextAppID                    int
	NextInstallationID           int
	NextCheckRunID               int64
	NextCheckSuiteID             int64
	NextRulesetID                int
	NextRulesetSuiteID           int
	NextProjectClassicID         int
	NextProjectColumnID          int
	NextProjectCardID            int
	NextUserMigrationID          int
	NextOrgMigrationID           int
	NextAutolinkID               int
	NextInvitationID             int
	NextDeployKeyID              int
	NextSecurityAdvisoryID       int
	NextSecurityAdvisoryReportID int
	Discussions                  map[int]*Discussion
	DiscussionCategories         map[int]*DiscussionCategory
	DiscussionComments           map[int]*DiscussionComment
	NextDiscussionID             int
	NextDiscussionNumber         map[int]int // repoID → next per-repo discussion number (high-water; monotonic across tombstones)
	NextDiscussionCategoryID     int
	NextDiscussionCommentID      int
	OrgActionsPermissions        map[string]*OrgActionsPermissions
	RepoActionsPermissions       map[string]*RepoActionsPermissions
	actionsKeyPair               *SecretsKeyPair // lazily generated sealed-box keypair (persisted)
	ActionsArtifacts             *ArtifactStore  `json:"-"`
	Persist                      *Persistence    `json:"-"`
	Logger                       zerolog.Logger  `json:"-"` // structured logger; NewServer wires the configured one, else a nop
	persistenceRevision          int64
	PersistenceRecoveryRequired  bool `json:"-"`
	nextLoginSessionReap         time.Time
	CodespaceRuntimeDelete       func(*Codespace) error                                                 `json:"-"`
	CodespaceWorkspacePrepare    func(string, *Repo, gitStorage.Storer, string) (string, func(), error) `json:"-"`
	RepoStorageOpen              func(context.Context, string) (gitStorage.Storer, error)               `json:"-"`
	// repoPrefixCopy/repoPrefixDelete are the slow object-store prefix moves the
	// S3 rename path runs outside the store lock (STORE-013). Nil in production
	// (the real S3 helpers run); a test injects a blocking copy to prove the
	// lock is released and to force the slow path without a live S3 backend.
	RepoPrefixCopy       func(oldFull, newFull string) error `json:"-"`
	RepoPrefixDelete     func(fullName string) error         `json:"-"`
	PendingRepoCreations map[string]bool                     `json:"-"`
	replicaRefreshMu     sync.Mutex
	// mu guards the Store's maps and counters. sync.RWMutex read locks are
	// NOT reentrant: once a writer queues on Lock, new RLock calls block, so
	// a goroutine that re-acquires mu while already holding it deadlocks.
	// The invariants that keep that impossible:
	//   - Public Store methods and JSON serializers (RepoToJSON, issueToJSON,
	//     CanReadRepoAsUser, …) acquire mu themselves and must never be called with
	//     mu held.
	//   - Helpers named xxxLocked (and helpers documented "callers hold
	//     st.Mu") never acquire mu; they run under the caller's lock.
	//   - Handlers and store methods that need both a coherent scan AND
	//     rendered JSON gather rows under one RLock, release, then render
	//     with the self-locking serializers (see deriveActivityEvents).
	//   - Lock order between mutexes: Store.Mu is acquired before any
	//     sub-store mutex (Misc.Mu, Reactions.Mu, Releases.Mu, persistence),
	//     never the reverse.
	Mu       sync.RWMutex     `json:"-"`
	ClockMu  sync.RWMutex     `json:"-"`
	ClockNow func() time.Time `json:"-"`
	// apiInsightsMu guards APIRequestRecords/NextAPIRequestID so recording
	// every API request never contends on the main store lock.
	apiInsightsMu sync.RWMutex
	// enterprises
	EnterpriseTeams                    map[int]*EnterpriseTeam
	EnterpriseTeamsBySlug              map[string]*EnterpriseTeam
	EnterpriseCodeSecurityConfigs      map[int]*EnterpriseCodeSecurityConfiguration
	EnterpriseCodeSecurityRepoConfigs  map[int]int // repoID → attached config ID
	EnterpriseSettings                 *EnterpriseSettings
	NextEnterpriseTeamID               int
	NextEnterpriseCodeSecurityConfigID int

	// attestations + org artifact metadata
	Attestations                   map[int]*Attestation // id → attestation
	NextAttestationID              int
	ArtifactStorageRecords         map[int]*ArtifactStorageRecord // id → storage record
	NextArtifactStorageRecordID    int
	ArtifactDeploymentRecords      map[int]*ArtifactDeploymentRecord // id → deployment record
	NextArtifactDeploymentRecordID int
	ArtifactDeploymentJobs         map[int]*ArtifactDeploymentJob // id → asynchronous cluster job
	NextArtifactDeploymentJobID    int

	// copilot + code quality (gh_copilot.go, gh_copilot_spaces.go, gh_code_quality.go)
	CopilotSeats             map[string]map[int]*CopilotSeat           // org login → user ID → seat
	CopilotContentExclusions map[string]*CopilotContentExclusion       // org login → rules
	CopilotCodingAgentPerms  map[string]*CopilotCodingAgentPermissions // org login → policy
	CopilotSpaces            map[int64]*CopilotSpace                   // space ID → space
	NextCopilotSpaceID       int64
	CodeQualitySetups        map[string]*CodeQualitySetup           // repo full name → setup
	CodeQualityFindings      map[string]map[int]*CodeQualityFinding // repo full name → finding number → finding

	// Current GitHub REST resource families introduced after the original
	// OpenAPI pin. They are first-class durable state, not route-only shims.
	SecretScanningCustomPatterns map[string]map[int]*SecretScanningCustomPattern // "org:<login>" or "repo:<full>" → id → pattern
	NextSecretScanningPatternID  int
	PRCreationCaps               map[string]*PRCreationCap           // repo full name → cap
	OrgPRCreationCaps            map[string]*PRCreationCap           // org login → cap
	PullRequestMergeAsync        map[string]*PullRequestMergeAsync   // uuid → async merge record
	PRCreationBypass             map[string]map[string]bool          // repo full name → login set
	IssueSuggestions             map[string]map[int]*IssueSuggestion // "owner/repo#issueID" → id → suggestion
	NextIssueSuggestionID        int
	PullRequestStacks            map[string]map[int]*PullRequestStack // repo full name → stack number → stack
	NextPullRequestStackID       int

	// org governance surfaces (code security configurations, custom
	// properties, issue types, issue fields, security campaigns, private
	// registries, hosted compute network configurations, immutable releases)
	CodeSecurityConfigs         map[string]map[int]*CodeSecurityConfiguration // org login → id → configuration
	CodeSecurityRepoAttachments map[string]map[int]int                        // org login → repo ID → configuration ID
	NextCodeSecurityConfigID    int
	OrgCustomProperties         map[string]map[string]*CustomProperty // org login → property name → definition
	RepoCustomPropertyValues    map[string]map[string]interface{}     // "owner/repo" → property name → value
	OrgIssueTypes               map[string]map[int]*IssueType         // org login → id → issue type
	IssueTypesByID              map[int]*IssueType                    // id → issue type (GQL-024 O(1) node-ID lookup; ids are globally unique)
	NextIssueTypeID             int
	OrgIssueFields              map[string]map[int]*IssueField // org login → id → issue field
	NextIssueFieldID            int
	NextIssueFieldOptionID      int
	IssueFieldValues            map[int]map[int]interface{}                         // issue ID → field ID → raw value
	OrgCampaigns                map[string]map[int]*Campaign                        // org login → campaign number → campaign
	OrgPrivateRegistries        map[string]map[string]*PrivateRegistryConfiguration // org login → name → configuration
	OrgNetworkConfigurations    map[string]map[string]*NetworkConfiguration         // org login → id → configuration
	OrgNetworkSettings          map[string]map[string]*NetworkSettingsResource      // org login → id → settings resource
	OrgImmutableReleases        map[string]*OrgImmutableReleasesSettings            // org login → enforcement policy
	RepoImmutableReleases       map[string]bool                                     // "owner/repo" → repo-level enablement

	// hosted-runners
	HostedRunners            map[int]*HostedRunner
	NextHostedRunnerID       int
	HostedRunnerCustomImages map[int]*HostedRunnerCustomImage
	NextHostedRunnerImageID  int
	// actions-oidc-properties
	OrgOIDCPropertyInclusions map[string][]string

	// agents-codescan: GitHub Copilot coding agent secrets/variables/tasks
	// and CodeQL databases/variant analyses.
	AgentsRepoSecrets           map[string]map[string]*Secret          // "owner/repo" → NAME → secret
	AgentsOrgSecrets            map[string]map[string]*OrgSecret       // org login → NAME → org secret
	AgentsRepoVariables         map[string]map[string]*ActionsVariable // "owner/repo" → NAME → variable
	AgentsOrgVariables          map[string]map[string]*ActionsVariable // org login → NAME → org variable
	AgentTasks                  map[string]*AgentTask                  // task ID (UUID) → task
	CodeScanningAutofixes       map[string]*CodeScanningAutofix        // autofixKey(repoKey, number) → autofix
	CodeQLDatabases             map[int]*CodeQLDatabase                // id → database
	CodeQLDatabasesByRepo       map[string]map[string]*CodeQLDatabase  // repoKey → language → database
	CodeQLVariantAnalyses       map[int]*CodeQLVariantAnalysis         // id → variant analysis
	NextCodeQLDatabaseID        int
	NextCodeQLVariantAnalysisID int
	// teams-people
	OrgInvitations         map[int]*OrgInvitation          // id → org invitation
	NextOrgInvitationID    int                             //
	OrgBlocks              map[string]map[int]time.Time    // orgLogin → blocked userID → blocked-at
	OrgInteractionLimits   map[string]*OrgInteractionLimit // orgLogin → active interaction limit
	OrgRoleTeamAssignments map[string]map[int][]int        // orgLogin → roleID → team IDs
	OrgRoleUserAssignments map[string]map[int][]int        // orgLogin → roleID → user IDs
	OrgAnnouncements       map[string]*EnterpriseAnnouncement
	OrgCustomRepoRoles     map[string]map[int]*OrgCustomRepositoryRole // orgLogin → role ID → role
	OrgCustomRoles         map[string]map[int]*OrgCustomOrganizationRole
	NextOrgCustomRoleID    int
	OrgSCIMUsers           map[string]map[string]*EnterpriseSCIMUser // orgLogin → SCIM ID → identity
	OrgExternalGroups      map[string]map[string]*OrgExternalIdentityGroup
	TeamExternalGroupIDs   map[int][]string // team ID → external group IDs
	NextOrgExternalGroupID int
	// org billing budgets (gh_org_billing.go)
	OrgBudgets map[string]map[string]*OrgBudget // org login → budget ID → budget
	// API insights (gh_api_insights.go)
	APIRequestRecords []*APIRequestRecord // ordered by ID (oldest first)
	NextAPIRequestID  int64
	// apiRequestRecordCap bounds both the in-memory log and its durable bucket;
	// defaults to maxAPIRequestRecords, kept as a field so tests can exercise
	// FIFO eviction and durable reclamation with a small cap.
	ApiRequestRecordCap int `json:"-"`
	// fine-grained personal access token administration (gh_org_pat_admin.go)
	OrgPATGrantRequests map[string]map[int]*OrgPATGrantRequest // org login → request ID → request
	OrgPATGrants        map[string]map[int]*OrgPATGrant        // org login → grant ID → grant
	NextPATRequestID    int
	NextPATGrantID      int
	NextPATTokenID      int
	// org codespaces access settings (gh_codespaces.go)
	OrgCodespacesAccess map[string]*OrgCodespacesAccess // org login → access settings
	// Dependabot repository access default level (gh_dependabot.go)
	DependabotRepoAccessDefaultLevel map[string]string // org login → "public" | "internal"
	// secret scanning pattern configurations + push protection (gh_secret_scanning.go)
	SecretScanningPatternConfigs   map[string]*OrgSecretScanningPatternConfig                     // org login → config
	SecretScanningPushPlaceholders map[string]map[string]*SecretScanningPushProtectionPlaceholder // repoKey → placeholder ID → placeholder
	SecretScanningPushBypasses     map[string][]*SecretScanningPushProtectionBypass               // repoKey → bypasses
	SecurityReviewRequests         map[string]map[int]*SecurityReviewRequest                      // "repo|kind" → number → request
	NextSecurityReviewRequestID    int
	NextSecurityReviewResponseID   int

	// repo-write surfaces
	PagesDeployments         map[int]map[int]*PagesDeploymentRecord // repoID → deployment ID → record
	NextPagesDeploymentID    int
	EnvBranchPolicies        map[int][]*DeploymentBranchPolicyRule // environment ID → ordered branch/tag policies
	NextEnvBranchPolicyID    int
	EnvProtectionRules       map[int][]*EnvCustomProtectionRule // environment ID → enabled custom protection rules
	NextEnvProtectionRuleID  int
	SubIssueLists            map[int][]int                 // parent issue ID → ordered sub-issue IDs
	SubIssueParent           map[int]int                   // sub-issue ID → parent issue ID
	IssueBlockedBy           map[int][]int                 // issue ID → IDs of the issues blocking it
	RepoImports              map[int]*RepoImport           // repoID → source import
	DependencySnapshots      map[int][]*DependencySnapshot // repoID → submitted snapshots (oldest first)
	NextDependencySnapshotID int
	SBOMExports              map[string]*SBOMExport // export uuid → SBOM report export

	// GitHub Classroom
	Classrooms                   map[int]*Classroom
	ClassroomAssignments         map[int]*ClassroomAssignment
	ClassroomAcceptedAssignments map[int]*ClassroomAcceptedAssignment
	NextClassroomID              int
	NextClassroomAssignmentID    int
	NextClassroomAcceptedID      int

	// repo-reads
	RepoActivities   map[int]*RepoActivity         // id → recorded ref update (push activity)
	NextRepoActivity int                           // next RepoActivity ID
	RepoCloneTraffic map[string]*RepoTrafficBucket // "repoID:YYYY-MM-DD" → clone counters

	// --- Actions hot-path indexes (actions_indexes.go) ---
	//
	// All of these are unexported on purpose: the replica-refresh field copy
	// skips unexported fields, so a snapshot swap can never smuggle stale
	// pointers in through them. jobsByPlanID/jobsByRequestID/planScopes/
	// planIDByScope mirror the replica-local Jobs map; workflowsByRunID and the
	// two concurrency-group indexes mirror the durable Workflows map and are
	// rebuilt wherever Workflows is reloaded (load, replica refresh).
	JobsByPlanID                map[string]*Job                       `json:"-"` // Job.PlanID → job
	jobsByRequestID             map[int64]*Job                        // Job.RequestID → job
	PlanScopes                  map[string]planScope                  `json:"-"` // Job.PlanID → plan scope identity (survives Message GC)
	PlanIDByScope               map[string]string                     `json:"-"` // plan scopeIdentifier → Job.PlanID
	WorkflowsByRunID            map[int]*Workflow                     `json:"-"` // Workflow.RunID → workflow
	workflowsByConcurrencyGroup map[string]map[string]*Workflow       // group → Workflow.ID → non-completed workflow
	jobsByConcurrencyGroup      map[string]map[*WorkflowJob]*Workflow // group → non-terminal job → owning workflow
}

func (st *Store) CurrentTime() time.Time {
	if st != nil {
		st.ClockMu.RLock()
		clockNow := st.ClockNow
		st.ClockMu.RUnlock()
		if clockNow != nil {
			return clockNow().UTC()
		}
	}
	return time.Now().UTC()
}

func (st *Store) PersistenceReady(ctx context.Context) error {
	st.Mu.RLock()
	persist := st.Persist
	recoveryRequired := st.PersistenceRecoveryRequired
	st.Mu.RUnlock()
	if recoveryRequired {
		return errors.New("persistence recovery is required")
	}
	return persist.Ready(ctx)
}

// Agent represents a registered runner agent.
type Agent struct {
	ID             int                 `json:"id"`
	Name           string              `json:"name"`
	Version        string              `json:"version"`
	Enabled        bool                `json:"enabled"`
	Status         string              `json:"status"`
	OSDescription  string              `json:"osDescription"`
	Labels         []Label             `json:"labels"`
	Authorization  *AgentAuthorization `json:"authorization,omitempty"`
	Ephemeral      bool                `json:"ephemeral,omitempty"`
	RunnerGroupID  int                 `json:"runnerGroupId,omitempty"`
	MaxParallelism int                 `json:"maxParallelism,omitempty"`
	ProvisionState string              `json:"provisioningState,omitempty"`
	CreatedOn      time.Time           `json:"createdOn"`
	// Scope is the repository or organization the agent registered against. It
	// is recorded here rather than encoded into the clientId because the runner
	// deserializes that field as a GUID and rejects anything else.
	Scope RunnerScope `json:"scope"`

	// AssignedJobID is the broker's process-local busy bookkeeping: the job
	// this agent currently holds (set when a job message is delivered, cleared
	// when a non-ephemeral agent's job completes or the lease is reclaimed).
	// EverAssigned records that the agent has held a job at least once; for an
	// EPHEMERAL agent that alone disqualifies it from another job — the flag
	// MUST survive the GC of the completed job's stub, or a used ephemeral
	// runner could be handed a second job (see agentTakesAJobLocked). Neither
	// field is serialized: the runner's TaskAgent contract has no such fields.
	AssignedJobID string `json:"-"`
	EverAssigned  bool   `json:"-"`
}

// Label is an agent label.
type Label struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// AgentAuthorization holds the agent's RSA public key and auth URL.
type AgentAuthorization struct {
	AuthorizationURL string          `json:"authorizationUrl,omitempty"`
	ClientID         string          `json:"clientId,omitempty"`
	PublicKey        *AgentPublicKey `json:"publicKey,omitempty"`
}

// AgentPublicKey is the RSA public key components.
type AgentPublicKey struct {
	Exponent string `json:"exponent"`
	Modulus  string `json:"modulus"`
}

// Session represents a runner's active session.
type Session struct {
	SessionID string                 `json:"sessionId"`
	OwnerName string                 `json:"ownerName"`
	Agent     *Agent                 `json:"agent"`
	MsgCh     chan *TaskAgentMessage `json:"-"`
}

// TaskAgentMessage is the message envelope sent to the runner.
type TaskAgentMessage struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	IV          string `json:"iv,omitempty"`
	Body        string `json:"body"`
	// Labels carries the job's runs-on requirements for broker routing;
	// JobID links the envelope to its engine job so delivery can record
	// which agent took it. Neither is serialized to the runner.
	Labels []string `json:"-"`
	JobID  string   `json:"-"`
}

// Job represents a queued/running/completed job.
type Job struct {
	ID          string    `json:"id"`
	RequestID   int64     `json:"requestId"`
	PlanID      string    `json:"planId"`
	TimelineID  string    `json:"timelineId"`
	Status      string    `json:"status"` // queued, running, completed
	Result      string    `json:"result"` // Succeeded, Failed, Cancelled
	Message     string    `json:"-"`      // JSON-encoded job request message (secret-bearing; cleared at run finalization)
	LockedUntil time.Time `json:"lockedUntil"`
	AgentID     int       `json:"agentId"`
	// CompletedAt is the retirement stamp for the janitor: set when the runner
	// reports completion or when the owning run finalizes, whichever comes
	// first. runnerTokenTTL after it, no valid credential can address this job
	// any more and its replica-local state is swept (actions_indexes.go).
	CompletedAt time.Time `json:"-"`
}

// NewStore creates an initialized store.
func NewStore() *Store {
	store := &Store{
		Logger:                       zerolog.Nop(),
		Agents:                       make(map[int]*Agent),
		Sessions:                     make(map[string]*Session),
		Jobs:                         make(map[string]*Job),
		Users:                        make(map[int]*User),
		UsersByLogin:                 make(map[string]*User),
		UsersByExternalID:            make(map[string]*User),
		Tokens:                       make(map[string]*Token),
		DeviceCodes:                  make(map[string]*DeviceCode),
		AuthCodes:                    make(map[string]*AuthCode),
		LoginSessions:                make(map[string]*LoginSession),
		OIDCLogoutClaims:             make(map[string]time.Time),
		Repos:                        make(map[int]*Repo),
		ReposByName:                  make(map[string]*Repo),
		GitStorages:                  make(map[string]gitStorage.Storer),
		PendingRepoCreations:         make(map[string]bool),
		Orgs:                         make(map[int]*Org),
		OrgsByLogin:                  make(map[string]*Org),
		Teams:                        make(map[int]*Team),
		TeamsBySlug:                  make(map[string]*Team),
		Memberships:                  make(map[string]*Membership),
		Issues:                       make(map[int]*Issue),
		IssuesByRepo:                 make(map[int]map[int]*Issue),
		Labels:                       make(map[int]*IssueLabel),
		Milestones:                   make(map[int]*Milestone),
		Comments:                     make(map[int]*Comment),
		CommentCounts:                make(map[string]int),
		CommentsByParent:             make(map[string][]*Comment),
		IssueEvents:                  make(map[int]*IssueEvent),
		PullRequests:                 make(map[int]*PullRequest),
		PullsByRepo:                  make(map[int]map[int]*PullRequest),
		PRReviews:                    make(map[int]*PullRequestReview),
		PRReviewsByPR:                make(map[int][]*PullRequestReview),
		Workflows:                    make(map[string]*Workflow),
		WorkflowFiles:                make(map[int64]*WorkflowFile),
		RepoSecrets:                  make(map[string]map[string]*Secret),
		RepoVariables:                make(map[string]map[string]*ActionsVariable),
		RepoCollaborators:            make(map[string]map[string]string),
		RepoAutolinks:                make(map[string]map[int]*RepoAutolink),
		RepoWikiPages:                make(map[string]map[string]*WikiPage),
		RepoInvitations:              make(map[string]map[int]*RepoInvitation),
		RepoDeployKeys:               make(map[string]map[int]*RepoDeployKey),
		RepoSubscriptions:            map[string]*RepoSubscription{},
		OrgSecrets:                   make(map[string]map[string]*OrgSecret),
		OrgVariables:                 make(map[string]map[string]*ActionsVariable),
		EnvSecrets:                   make(map[string]map[string]*Secret),
		EnvVariables:                 make(map[string]map[string]*ActionsVariable),
		TimelineRecords:              make(map[string][]*TimelineRecord),
		LogFiles:                     make(map[int][]byte),
		LogMasks:                     make(map[string][]string),
		WorkflowAttempts:             make(map[int][]*Workflow),
		RunnerGroups:                 make(map[int]*RunnerGroup),
		NextRunnerGroupID:            2,
		Hooks:                        make(map[string][]*Webhook),
		OrgHooks:                     make(map[string][]*Webhook),
		HookDeliveries:               make(map[int][]*WebhookDelivery),
		Apps:                         make(map[int]*App),
		AppsBySlug:                   make(map[string]*App),
		AppsByClientID:               make(map[string]*App),
		OAuthApps:                    make(map[string]*OAuthApp),
		Installations:                make(map[int]*Installation),
		InstallationTokens:           make(map[string]*InstallationToken),
		UserToServerTokens:           make(map[string]*UserToServerToken),
		RefreshTokens:                make(map[string]*RefreshToken),
		AppHookDeliveries:            make(map[int][]*WebhookDelivery),
		ManifestCodes:                make(map[string]int),
		CheckRuns:                    make(map[int64]*CheckRun),
		CheckSuites:                  make(map[int64]*CheckSuite),
		CheckSuitePrefs:              make(map[string][]*CheckSuitePref),
		CommitStatuses:               newCommitStatusStore(nil),
		CommitComments:               newCommitCommentStore(nil),
		Reactions:                    newReactionStore(nil),
		Releases:                     newReleaseStore(nil),
		Deployments:                  newDeploymentStore(nil),
		PRReviewComments:             NewPRReviewCommentStore(nil),
		Misc:                         newMiscStore(),
		ProjectsV2:                   NewProjectV2Store(nil),
		NotificationsState:           map[int]*UserNotificationsState{},
		Rulesets:                     map[int]*Ruleset{},
		RulesetSuites:                map[int]*RulesetSuite{},
		ProjectClassic:               map[int]*ProjectClassic{},
		ProjectColumns:               map[int]*ProjectColumn{},
		ProjectCards:                 map[int]*ProjectCard{},
		UserMigrations:               map[int]*UserMigration{},
		OrgMigrations:                map[int]*OrgMigration{},
		Codespaces:                   map[int]*Codespace{},
		CodespacesByName:             map[string]*Codespace{},
		CodespaceSecrets:             map[string]map[string]*CodespaceSecret{},
		LogLines:                     make(map[string][]string),
		Gists:                        make(map[string]*Gist),
		GistComments:                 make(map[int]*GistComment),
		StarredGists:                 make(map[int]map[string]bool),
		SecretScanningAlerts:         make(map[int]*SecretScanningAlert),
		SecretScanningAlertsByRepo:   make(map[string]map[int]*SecretScanningAlert),
		SecretScanningNextNumber:     make(map[string]int),
		CodeScanningAlerts:           make(map[int]*CodeScanningAlert),
		CodeScanningAlertsByRepo:     make(map[string]map[int]*CodeScanningAlert),
		CodeScanningNextNumber:       make(map[string]int),
		CodeScanningAnalyses:         make(map[int]*CodeScanningAnalysis),
		CodeScanningAnalysesByRepo:   make(map[string]map[int]*CodeScanningAnalysis),
		CodeScanningDefaultSetups:    make(map[string]*CodeScanningDefaultSetup),
		SARIFUploads:                 make(map[string]*SARIFUpload),
		DependabotAlerts:             make(map[int]*DependabotAlert),
		DependabotAlertsByRepo:       make(map[string]map[int]*DependabotAlert),
		DependabotNextNumber:         make(map[string]int),
		DependabotSecrets:            make(map[string]map[string]*DependabotSecret),
		DependabotOrgSecrets:         make(map[string]map[string]*DependabotOrgSecret),
		DependabotUserSecrets:        make(map[string]map[string]*DependabotUserSecret),
		DependabotRepositoryAccess:   map[string][]int{},
		SecurityAdvisories:           map[int]*SecurityAdvisory{},
		SecurityAdvisoriesByRepo:     map[string]map[string]*SecurityAdvisory{},
		SecurityAdvisoryReports:      map[int]*SecurityAdvisoryReport{},
		Packages:                     map[int]*Package{},
		PackageVersions:              map[int]*PackageVersion{},
		PackageFiles:                 map[int]*PackageFile{},
		PackagesByOwnerKey:           map[string]map[string]*Package{},
		PackageVersionsByPackage:     map[int]map[int]*PackageVersion{},
		PackageFilesByVersion:        map[int]map[int]*PackageFile{},
		Discussions:                  map[int]*Discussion{},
		DiscussionCategories:         map[int]*DiscussionCategory{},
		DiscussionComments:           map[int]*DiscussionComment{},
		OrgActionsPermissions:        map[string]*OrgActionsPermissions{},
		RepoActionsPermissions:       map[string]*RepoActionsPermissions{},
		JobsByPlanID:                 make(map[string]*Job),
		jobsByRequestID:              make(map[int64]*Job),
		PlanScopes:                   make(map[string]planScope),
		PlanIDByScope:                make(map[string]string),
		WorkflowsByRunID:             make(map[int]*Workflow),
		workflowsByConcurrencyGroup:  make(map[string]map[string]*Workflow),
		jobsByConcurrencyGroup:       make(map[string]map[*WorkflowJob]*Workflow),
		NextAgent:                    1,
		NextSecretScanningAlertID:    1,
		NextCodeScanningAlertID:      1,
		NextCodeScanningAnalysisID:   1,
		NextDependabotAlertID:        1,
		NextPackageID:                1,
		NextPackageVersionID:         1,
		NextPackageFileID:            1,
		NextMsg:                      1,
		NextLog:                      1,
		NextReqID:                    1,
		NextUser:                     1,
		NextRepo:                     1,
		NextOrg:                      1,
		NextTeam:                     1,
		NextIssue:                    1,
		NextLabel:                    1,
		NextMilestone:                1,
		NextComment:                  1,
		NextPR:                       1,
		NextPRReview:                 1,
		NextRunID:                    1,
		NextHookID:                   1,
		NextDeliveryID:               1,
		NextAppID:                    1,
		NextInstallationID:           1,
		NextCheckRunID:               1,
		NextCheckSuiteID:             1,
		NextRulesetID:                1,
		NextRulesetSuiteID:           1,
		NextProjectClassicID:         1,
		NextProjectColumnID:          1,
		NextProjectCardID:            1,
		NextUserMigrationID:          1,
		NextOrgMigrationID:           1,
		NextCodespaceID:              1,
		NextCodespaceSecretID:        1,
		NextAutolinkID:               1,
		NextInvitationID:             1,
		NextIssueEventID:             1,
		NextDeployKeyID:              1,
		NextSecurityAdvisoryID:       1,
		NextSecurityAdvisoryReportID: 1,
		NextDiscussionID:             1,
		NextDiscussionNumber:         make(map[int]int),
		NextDiscussionCategoryID:     1,
		NextDiscussionCommentID:      1,
		// enterprises
		EnterpriseTeams:                    map[int]*EnterpriseTeam{},
		EnterpriseTeamsBySlug:              map[string]*EnterpriseTeam{},
		EnterpriseCodeSecurityConfigs:      map[int]*EnterpriseCodeSecurityConfiguration{},
		EnterpriseCodeSecurityRepoConfigs:  map[int]int{},
		EnterpriseSettings:                 defaultEnterpriseSettings(),
		NextEnterpriseTeamID:               1,
		NextEnterpriseCodeSecurityConfigID: 1,

		// attestations + org artifact metadata
		Attestations:                   map[int]*Attestation{},
		NextAttestationID:              1,
		ArtifactStorageRecords:         map[int]*ArtifactStorageRecord{},
		NextArtifactStorageRecordID:    1,
		ArtifactDeploymentRecords:      map[int]*ArtifactDeploymentRecord{},
		NextArtifactDeploymentRecordID: 1,
		ArtifactDeploymentJobs:         map[int]*ArtifactDeploymentJob{},
		NextArtifactDeploymentJobID:    1,

		// copilot + code quality
		CopilotSeats:                 make(map[string]map[int]*CopilotSeat),
		CopilotContentExclusions:     make(map[string]*CopilotContentExclusion),
		CopilotCodingAgentPerms:      make(map[string]*CopilotCodingAgentPermissions),
		CopilotSpaces:                make(map[int64]*CopilotSpace),
		NextCopilotSpaceID:           1,
		CodeQualitySetups:            make(map[string]*CodeQualitySetup),
		CodeQualityFindings:          make(map[string]map[int]*CodeQualityFinding),
		SecretScanningCustomPatterns: make(map[string]map[int]*SecretScanningCustomPattern),
		NextSecretScanningPatternID:  1,
		PRCreationCaps:               make(map[string]*PRCreationCap),
		OrgPRCreationCaps:            make(map[string]*PRCreationCap),
		PullRequestMergeAsync:        make(map[string]*PullRequestMergeAsync),
		PRCreationBypass:             make(map[string]map[string]bool),
		IssueSuggestions:             make(map[string]map[int]*IssueSuggestion),
		NextIssueSuggestionID:        1,
		PullRequestStacks:            make(map[string]map[int]*PullRequestStack),
		NextPullRequestStackID:       1,

		// org governance surfaces
		CodeSecurityConfigs:         map[string]map[int]*CodeSecurityConfiguration{},
		CodeSecurityRepoAttachments: map[string]map[int]int{},
		NextCodeSecurityConfigID:    1,
		OrgCustomProperties:         map[string]map[string]*CustomProperty{},
		RepoCustomPropertyValues:    map[string]map[string]interface{}{},
		OrgIssueTypes:               map[string]map[int]*IssueType{},
		IssueTypesByID:              map[int]*IssueType{},
		NextIssueTypeID:             1,
		OrgIssueFields:              map[string]map[int]*IssueField{},
		NextIssueFieldID:            1,
		NextIssueFieldOptionID:      1,
		IssueFieldValues:            map[int]map[int]interface{}{},
		OrgCampaigns:                map[string]map[int]*Campaign{},
		OrgPrivateRegistries:        map[string]map[string]*PrivateRegistryConfiguration{},
		OrgNetworkConfigurations:    map[string]map[string]*NetworkConfiguration{},
		OrgNetworkSettings:          map[string]map[string]*NetworkSettingsResource{},
		OrgImmutableReleases:        map[string]*OrgImmutableReleasesSettings{},
		RepoImmutableReleases:       map[string]bool{},

		// hosted-runners
		HostedRunners:            map[int]*HostedRunner{},
		NextHostedRunnerID:       1,
		HostedRunnerCustomImages: map[int]*HostedRunnerCustomImage{},
		NextHostedRunnerImageID:  1,
		// actions-oidc-properties
		OrgOIDCPropertyInclusions: map[string][]string{},

		// agents-codescan
		AgentsRepoSecrets:           make(map[string]map[string]*Secret),
		AgentsOrgSecrets:            make(map[string]map[string]*OrgSecret),
		AgentsRepoVariables:         make(map[string]map[string]*ActionsVariable),
		AgentsOrgVariables:          make(map[string]map[string]*ActionsVariable),
		AgentTasks:                  make(map[string]*AgentTask),
		CodeScanningAutofixes:       make(map[string]*CodeScanningAutofix),
		CodeQLDatabases:             make(map[int]*CodeQLDatabase),
		CodeQLDatabasesByRepo:       make(map[string]map[string]*CodeQLDatabase),
		CodeQLVariantAnalyses:       make(map[int]*CodeQLVariantAnalysis),
		NextCodeQLDatabaseID:        1,
		NextCodeQLVariantAnalysisID: 1,
		// teams-people
		OrgInvitations:         map[int]*OrgInvitation{},
		NextOrgInvitationID:    1,
		OrgBlocks:              map[string]map[int]time.Time{},
		OrgInteractionLimits:   map[string]*OrgInteractionLimit{},
		OrgRoleTeamAssignments: map[string]map[int][]int{},
		OrgRoleUserAssignments: map[string]map[int][]int{},
		OrgAnnouncements:       map[string]*EnterpriseAnnouncement{},
		OrgCustomRepoRoles:     map[string]map[int]*OrgCustomRepositoryRole{},
		OrgCustomRoles:         map[string]map[int]*OrgCustomOrganizationRole{},
		NextOrgCustomRoleID:    1000,
		OrgSCIMUsers:           map[string]map[string]*EnterpriseSCIMUser{},
		OrgExternalGroups:      map[string]map[string]*OrgExternalIdentityGroup{},
		TeamExternalGroupIDs:   map[int][]string{},
		NextOrgExternalGroupID: 1,
		// org billing budgets
		OrgBudgets: map[string]map[string]*OrgBudget{},
		// API insights
		NextAPIRequestID:    1,
		ApiRequestRecordCap: maxAPIRequestRecords,
		// fine-grained personal access token administration
		OrgPATGrantRequests: map[string]map[int]*OrgPATGrantRequest{},
		OrgPATGrants:        map[string]map[int]*OrgPATGrant{},
		NextPATRequestID:    1,
		NextPATGrantID:      1,
		NextPATTokenID:      1,
		// org codespaces access settings
		OrgCodespacesAccess: map[string]*OrgCodespacesAccess{},
		// Dependabot repository access default level
		DependabotRepoAccessDefaultLevel: map[string]string{},
		// secret scanning pattern configurations + push protection
		SecretScanningPatternConfigs:   map[string]*OrgSecretScanningPatternConfig{},
		SecretScanningPushPlaceholders: map[string]map[string]*SecretScanningPushProtectionPlaceholder{},
		SecretScanningPushBypasses:     map[string][]*SecretScanningPushProtectionBypass{},
		SecurityReviewRequests:         map[string]map[int]*SecurityReviewRequest{},
		NextSecurityReviewRequestID:    1,
		NextSecurityReviewResponseID:   1,
		// repo-write surfaces
		PagesDeployments:         map[int]map[int]*PagesDeploymentRecord{},
		NextPagesDeploymentID:    1,
		EnvBranchPolicies:        map[int][]*DeploymentBranchPolicyRule{},
		NextEnvBranchPolicyID:    1,
		EnvProtectionRules:       map[int][]*EnvCustomProtectionRule{},
		NextEnvProtectionRuleID:  1,
		SubIssueLists:            map[int][]int{},
		SubIssueParent:           map[int]int{},
		IssueBlockedBy:           map[int][]int{},
		RepoImports:              map[int]*RepoImport{},
		DependencySnapshots:      map[int][]*DependencySnapshot{},
		NextDependencySnapshotID: 1,
		SBOMExports:              map[string]*SBOMExport{},
		// GitHub Classroom
		Classrooms:                   map[int]*Classroom{},
		ClassroomAssignments:         map[int]*ClassroomAssignment{},
		ClassroomAcceptedAssignments: map[int]*ClassroomAcceptedAssignment{},
		NextClassroomID:              1,
		NextClassroomAssignmentID:    1,
		NextClassroomAcceptedID:      1,

		// repo-reads
		RepoActivities:   make(map[int]*RepoActivity),
		NextRepoActivity: 1,
		RepoCloneTraffic: make(map[string]*RepoTrafficBucket),
	}
	store.CodespaceRuntimeDelete = store.deleteCodespaceRuntime
	store.CodespaceWorkspacePrepare = prepareCodespaceWorkspace
	store.RepoStorageOpen = gitstore.OpenOrInitGitStorage
	return store
}

// SetPersistence wires a Persistence layer onto the Store. Call once at
// startup before any concurrent access; subsequent Create/Update/Delete
// mutations will write through to the underlying SQLite db.
//
// If persist is non-nil, this also loads existing rows from disk into the
// in-memory maps. Idempotent — safe to call against an empty database.
//
// invariant: open-failure must be caught at the persistence-open
// site (MustNewPersistence) so the operator gets a fail-loud signal
// before we even get here.
func (st *Store) SetPersistence(p *Persistence) error {
	if p == nil {
		return nil
	}
	if !p.OwnedExclusively() {
		st.wirePersistence(p)
		// A dqlite peer may be writing while this replica starts. Use the same
		// before/after revision-stable reconciler as request-time refreshes;
		// otherwise startup could bless a cross-bucket partial snapshot as the
		// newest revision and never reload it.
		if err := st.refreshFromPersistence(true); err != nil {
			return err
		}
		return st.backfillLoginSessionUserIndex()
	}
	if err := st.setPersistence(p, true); err != nil {
		return err
	}
	return st.backfillLoginSessionUserIndex()
}

func (st *Store) setPersistence(p *Persistence, observeRevision bool) error {
	if p == nil {
		return nil
	}
	st.wirePersistence(p)
	if err := st.loadFromPersistence(); err != nil {
		return err
	}
	if observeRevision {
		p.localRevision.Store(st.persistenceRevision)
	}
	return nil
}

func (st *Store) wirePersistence(p *Persistence) {
	st.Mu.Lock()
	st.Persist = p
	st.Reactions.Persist = p
	st.Releases.Persist = p
	st.Deployments.Persist = p
	st.PRReviewComments.Persist = p
	st.ProjectsV2.Persist = p
	st.Misc.Persist = p
	st.CommitStatuses.Persist = p
	st.CommitComments.Persist = p
	st.Mu.Unlock()
	// Object-store git bytes have no advisory locking of their own; the durable
	// store is what every replica already shares, so it is what arbitrates
	// concurrent ref updates.
	if gitstore.IsS3GitStorage() {
		gitstore.SetGitObjectLocker(p)
	}
}

// loadFromPersistence repopulates the in-memory maps from disk.
//
// The loadBucket registrations below are the authoritative durable-state
// inventory.
//
// Agent connections and ephemeral service codes deliberately stay in memory.
// Browser sessions are durable because browser authentication must survive
// service replacement and requests may reach any replica.

// maxParallelStorageOpen bounds the restart-time git-storage open fan-out. The
// open is I/O-bound (filesystem or S3), so a modest concurrency collapses
// restart latency without swamping the backend.
const maxParallelStorageOpen = 16

// openRepoStoragesConcurrently reopens every repo's git storage in parallel and
// assigns the handles once they are all ready. The repo records are already in
// the store maps; only the storage handles remain (STORE-054). Each worker
// writes a distinct result index and the handles are consumed on the calling
// goroutine after the workers join, so st.GitStorages is mutated only here,
// single-threaded and lock-free.
func (st *Store) openRepoStoragesConcurrently(fullNames []string) error {
	if len(fullNames) == 0 {
		return nil
	}
	type opened struct {
		stor gitStorage.Storer
		Err  error `json:"-"`
	}
	results := make([]opened, len(fullNames))
	sem := make(chan struct{}, maxParallelStorageOpen)
	var wg sync.WaitGroup
	for i, fullName := range fullNames {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, fullName string) {
			defer wg.Done()
			defer func() { <-sem }()
			stor, err := gitstore.OpenOrInitGitStorage(context.Background(), fullName)
			results[i] = opened{stor: stor, Err: err}
		}(i, fullName)
	}
	wg.Wait()
	for i, res := range results {
		if res.Err != nil {
			return fmt.Errorf("reopen git storage %s: %w", fullNames[i], res.Err)
		}
		st.GitStorages[fullNames[i]] = res.stor
	}
	return nil
}

func (st *Store) loadFromPersistence() error {
	if st.Persist == nil {
		return nil
	}
	// Deletion intents are read first: they explain rows that would otherwise
	// look like corruption, and they are what lets an interrupted cascade be
	// finished rather than left as a permanent boot failure.
	pendingDeletions, err := st.listPendingDeletions()
	if err != nil {
		return err
	}
	orphanedRepoRows := NewPersistBatch(st.Persist)
	var orphanedRepoNames []string
	// Git-storage handles are opened after the record load, concurrently: the
	// per-repo open is filesystem/S3 I/O, so a serial loop makes restart
	// latency scale with the repository count (STORE-054).
	var reposToOpen []string
	if err := st.loadBucket("users", func(raw []byte) error {
		var u User
		if err := LoadJSON(raw, &u); err != nil {
			return err
		}
		st.Users[u.ID] = &u
		st.UsersByLogin[u.Login] = &u
		for _, identity := range u.ExternalIdentities {
			if key := ExternalIdentityKey(identity.Issuer, identity.Subject); key != "" {
				st.UsersByExternalID[key] = &u
			}
		}
		if u.ID >= st.NextUser {
			st.NextUser = u.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	loginSessions, err := st.Persist.List(LoginSessionsBucket)
	if err != nil {
		return fmt.Errorf("load %s: %w", LoginSessionsBucket, err)
	}
	for id, raw := range loginSessions {
		var session LoginSession
		if err := LoadJSON(raw, &session); err != nil {
			return fmt.Errorf("decode %s row: %w", LoginSessionsBucket, err)
		}
		if session.ExpiresAt.After(st.CurrentTime()) && st.Users[session.UserID] != nil {
			st.LoginSessions[id] = &session
		}
	}
	tokenRows, err := st.Persist.List("tokens")
	if err != nil {
		return fmt.Errorf("load tokens: %w", err)
	}
	for persistedKey, raw := range tokenRows {
		var t Token
		if err := LoadJSON(raw, &t); err != nil {
			return fmt.Errorf("decode tokens row: %w", err)
		}
		st.Tokens[persistedKey] = &t
	}
	if err := st.loadBucket("gists", func(raw []byte) error {
		var g Gist
		if err := LoadJSON(raw, &g); err != nil {
			return err
		}
		if st.Users[g.OwnerID] == nil {
			return fmt.Errorf("gist %s: owner id %d not found in loaded users", g.ID, g.OwnerID)
		}
		if g.Files == nil {
			g.Files = map[string]*GistFile{}
		}
		if g.History == nil {
			g.History = []*GistHistory{}
		}
		st.Gists[g.ID] = &g
		if n, err := strconv.Atoi(strings.TrimPrefix(g.NodeID, "G_kwDOB")); err == nil && n >= st.NextGistID {
			st.NextGistID = n + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("gist_comments", func(raw []byte) error {
		var c GistComment
		if err := LoadJSON(raw, &c); err != nil {
			return err
		}
		if st.Gists[c.GistID] == nil {
			return fmt.Errorf("gist comment %d: gist %s not found in loaded gists", c.ID, c.GistID)
		}
		if st.Users[c.UserID] == nil {
			return fmt.Errorf("gist comment %d: user id %d not found in loaded users", c.ID, c.UserID)
		}
		st.GistComments[c.ID] = &c
		if c.ID >= st.NextGistCommentID {
			st.NextGistCommentID = c.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("starred_gists", func(raw []byte) error {
		var row persistedStarredGists
		if err := LoadJSON(raw, &row); err != nil {
			return err
		}
		if st.Users[row.UserID] == nil {
			return fmt.Errorf("starred_gists user id %d not found in loaded users", row.UserID)
		}
		for gistID, starred := range row.Stars {
			if !starred {
				delete(row.Stars, gistID)
				continue
			}
			if st.Gists[gistID] == nil {
				return fmt.Errorf("starred_gists user %d: gist %s not found in loaded gists", row.UserID, gistID)
			}
		}
		if len(row.Stars) > 0 {
			st.StarredGists[row.UserID] = row.Stars
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("apps", func(raw []byte) error {
		var a App
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		if st.Users[a.OwnerID] == nil {
			return fmt.Errorf("app %d (%s): owner id %d not found in loaded users", a.ID, a.Slug, a.OwnerID)
		}
		a.Permissions = NormalizeAppPermissions(a.Permissions)
		st.Apps[a.ID] = &a
		st.AppsBySlug[a.Slug] = &a
		st.AppsByClientID[a.ClientID] = &a
		if a.ID >= st.NextAppID {
			st.NextAppID = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("oauth_apps", func(raw []byte) error {
		var a OAuthApp
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.OAuthApps[a.ClientID] = &a
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("installations", func(raw []byte) error {
		var inst Installation
		if err := LoadJSON(raw, &inst); err != nil {
			return err
		}
		inst.Permissions = NormalizeAppPermissions(inst.Permissions)
		st.Installations[inst.ID] = &inst
		if inst.ID >= st.NextInstallationID {
			st.NextInstallationID = inst.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("installation_tokens", func(raw []byte) error {
		var t InstallationToken
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		t.Permissions = NormalizeAppPermissions(t.Permissions)
		st.InstallationTokens[t.Token] = &t
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("user_to_server_tokens", func(raw []byte) error {
		var t UserToServerToken
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		st.UserToServerTokens[t.Token] = &t
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("refresh_tokens", func(raw []byte) error {
		var t RefreshToken
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		st.RefreshTokens[t.Token] = &t
		return nil
	}); err != nil {
		return err
	}
	// Load organizations before repositories so persisted repository ownership
	// is validated against the real owner table.
	if err := st.loadBucket("orgs", func(raw []byte) error {
		var o Org
		if err := LoadJSON(raw, &o); err != nil {
			return err
		}
		st.Orgs[o.ID] = &o
		st.OrgsByLogin[o.Login] = &o
		if o.ID >= st.NextOrg {
			st.NextOrg = o.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("repos", func(raw []byte) error {
		var r Repo
		if err := LoadJSON(raw, &r); err != nil {
			return err
		}
		ownerLogin, _, ok := strings.Cut(r.FullName, "/")
		if !ok || ownerLogin == "" || r.Name == "" {
			return fmt.Errorf("repo %q: invalid full_name/name", r.FullName)
		}
		if r.OwnerID == 0 {
			return fmt.Errorf("repo %s: missing owner_id", r.FullName)
		}
		switch r.OwnerType {
		case "User":
			owner := st.Users[r.OwnerID]
			if owner == nil {
				return fmt.Errorf("repo %s: user owner id=%d not found in loaded users", r.FullName, r.OwnerID)
			}
			if !strings.EqualFold(ownerLogin, owner.Login) {
				return fmt.Errorf("repo %s: user owner id=%d login %q does not match full_name owner %q", r.FullName, r.OwnerID, owner.Login, ownerLogin)
			}
			r.Owner = owner
		case "Organization":
			org := st.Orgs[r.OwnerID]
			if org == nil {
				// An organization delete that was interrupted after the
				// organization row went away leaves its repositories behind.
				// Without the recorded intent this is indistinguishable from
				// corruption and every later boot fails on the same row.
				if _, deleting := pendingDeletions[PendingOrgDeletionKey(ownerLogin)]; deleting {
					orphanedRepoRows.Delete("repos", strconv.Itoa(r.ID))
					orphanedRepoNames = append(orphanedRepoNames, r.FullName)
					return nil
				}
				return fmt.Errorf("repo %s: organization owner id=%d not found in loaded organizations", r.FullName, r.OwnerID)
			}
			if !strings.EqualFold(ownerLogin, org.Login) {
				return fmt.Errorf("repo %s: organization owner id=%d login %q does not match full_name owner %q", r.FullName, r.OwnerID, org.Login, ownerLogin)
			}
			r.Owner = nil
		default:
			return fmt.Errorf("repo %s: invalid owner_type %q", r.FullName, r.OwnerType)
		}
		// Per-repo number counters are recomputed from loaded issues/PRs/
		// milestones below (their loaders bump these past every seen number).
		r.NextIssueNumber = 1
		r.NextMilestoneNumber = 1
		st.Repos[r.ID] = &r
		st.ReposByName[r.FullName] = &r
		if r.ID >= st.NextRepo {
			st.NextRepo = r.ID + 1
		}
		// Defer opening (or creating) this repo's git storage to the concurrent
		// pass below so git operations work immediately after restart without a
		// serial per-repo I/O wait here.
		reposToOpen = append(reposToOpen, r.FullName)
		return nil
	}); err != nil {
		return err
	}
	if err := st.openRepoStoragesConcurrently(reposToOpen); err != nil {
		return err
	}
	for _, fullName := range orphanedRepoNames {
		if err := deleteRepoGitStorage(fullName); err != nil {
			return fmt.Errorf("purge git storage of deleted organization repository %s: %w", fullName, err)
		}
	}
	if err := orphanedRepoRows.Commit(); err != nil {
		return fmt.Errorf("purge repositories of a deleted organization: %w", err)
	}
	// Load teams and memberships.
	if err := st.loadBucket("teams", func(raw []byte) error {
		var t Team
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		st.Teams[t.ID] = &t
		// Rebuild TeamsBySlug by looking up the org.
		if org := st.Orgs[t.OrgID]; org != nil {
			st.TeamsBySlug[TeamSlugKey(org.Login, t.Slug)] = &t
		}
		if t.ID >= st.NextTeam {
			st.NextTeam = t.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("memberships", func(raw []byte) error {
		var m Membership
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		if org := st.Orgs[m.OrgID]; org != nil {
			st.Memberships[MembershipKey(org.Login, m.UserID)] = &m
		}
		return nil
	}); err != nil {
		return err
	}
	// Load issues, labels, milestones, comments.
	if err := st.loadBucket("labels", func(raw []byte) error {
		var l IssueLabel
		if err := LoadJSON(raw, &l); err != nil {
			return err
		}
		st.Labels[l.ID] = &l
		if l.ID >= st.NextLabel {
			st.NextLabel = l.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("milestones", func(raw []byte) error {
		var m Milestone
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		st.Milestones[m.ID] = &m
		if m.ID >= st.NextMilestone {
			st.NextMilestone = m.ID + 1
		}
		if repo := st.Repos[m.RepoID]; repo != nil && m.Number >= repo.NextMilestoneNumber {
			repo.NextMilestoneNumber = m.Number + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("issues", func(raw []byte) error {
		var i Issue
		if err := LoadJSON(raw, &i); err != nil {
			return err
		}
		st.Issues[i.ID] = &i
		st.indexIssueLocked(&i)
		if i.ID >= st.NextIssue {
			st.NextIssue = i.ID + 1
		}
		if repo := st.Repos[i.RepoID]; repo != nil && i.Number >= repo.NextIssueNumber {
			repo.NextIssueNumber = i.Number + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("comments", func(raw []byte) error {
		var c Comment
		if err := LoadJSON(raw, &c); err != nil {
			return err
		}
		st.Comments[c.ID] = &c
		st.CommentCounts[CommentCountKey(c.ParentType, c.IssueID)]++
		st.indexCommentLocked(&c)
		if c.ID >= st.NextComment {
			st.NextComment = c.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("issue_events", func(raw []byte) error {
		var e IssueEvent
		if err := LoadJSON(raw, &e); err != nil {
			return err
		}
		st.IssueEvents[e.ID] = &e
		if e.ID >= st.NextIssueEventID {
			st.NextIssueEventID = e.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("pull_requests", func(raw []byte) error {
		var pr PullRequest
		if err := LoadJSON(raw, &pr); err != nil {
			return err
		}
		st.PullRequests[pr.ID] = &pr
		st.IndexPullLocked(&pr)
		if pr.ID >= st.NextPR {
			st.NextPR = pr.ID + 1
		}
		// PRs share the per-repo issue-number sequence with issues.
		if repo := st.Repos[pr.RepoID]; repo != nil && pr.Number >= repo.NextIssueNumber {
			repo.NextIssueNumber = pr.Number + 1
		}
		return nil
	}); err != nil {
		return err
	}

	// Runs that were mid-flight at shutdown are only this replica's to cancel
	// when this replica owns the database outright. Against a shared quorum the
	// run may still be executing on a live peer, and cancelling it here would
	// kill another replica's work every time this one starts.
	adoptRuns := st.Persist.OwnedExclusively()
	cancelledRuns := NewPersistBatch(st.Persist)

	for _, loadFn := range []struct {
		Name string `json:"-"`
		fn   func(string, []byte) error
	}{
		{"hooks", func(key string, raw []byte) error {
			var hooks []*Webhook
			if err := LoadJSON(raw, &hooks); err != nil {
				return err
			}
			// RepoKey is json:"-" (it duplicates the bucket key), so
			// backfill it — deliveries and hook lookups key on it.
			for _, h := range hooks {
				h.RepoKey = key
				if h.ID >= st.NextHookID {
					st.NextHookID = h.ID + 1
				}
			}
			st.Hooks[key] = hooks
			return nil
		}},
		{"org_hooks", func(key string, raw []byte) error {
			var hooks []*Webhook
			if err := LoadJSON(raw, &hooks); err != nil {
				return err
			}
			// OrgLogin is json:"-" (it duplicates the bucket key), so
			// backfill it — deliveries and hook lookups key on it.
			for _, h := range hooks {
				h.OrgLogin = key
				if h.ID >= st.NextHookID {
					st.NextHookID = h.ID + 1
				}
			}
			st.OrgHooks[key] = hooks
			return nil
		}},
		{"hook_deliveries", func(_ string, raw []byte) error {
			var deliveries []*WebhookDelivery
			if err := LoadJSON(raw, &deliveries); err != nil {
				return err
			}
			for _, d := range deliveries {
				st.HookDeliveries[d.HookID] = append(st.HookDeliveries[d.HookID], d)
				if d.ID >= st.NextDeliveryID {
					st.NextDeliveryID = d.ID + 1
				}
			}
			return nil
		}},
		{"app_hook_deliveries", func(key string, raw []byte) error {
			// App deliveries are bucketed by app ID; a delivery's HookID is
			// NOT the app ID (app-level deliveries use a synthetic hook id),
			// so file the slice under the bucket key.
			appID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("app_hook_deliveries key %q: %w", key, err)
			}
			var deliveries []*WebhookDelivery
			if err := LoadJSON(raw, &deliveries); err != nil {
				return err
			}
			for _, d := range deliveries {
				st.AppHookDeliveries[appID] = append(st.AppHookDeliveries[appID], d)
				if d.ID >= st.NextDeliveryID {
					st.NextDeliveryID = d.ID + 1
				}
			}
			return nil
		}},
		{"check_suite_prefs", func(key string, raw []byte) error {
			var prefs []*CheckSuitePref
			if err := LoadJSON(raw, &prefs); err != nil {
				return err
			}
			st.CheckSuitePrefs[key] = prefs
			return nil
		}},
		{"commit_statuses", func(key string, raw []byte) error {
			var statuses []*CommitStatus
			if err := LoadJSON(raw, &statuses); err != nil {
				return err
			}
			st.CommitStatuses.byKey[key] = statuses
			for _, cs := range statuses {
				if cs.ID >= st.CommitStatuses.NextID {
					st.CommitStatuses.NextID = cs.ID + 1
				}
			}
			return nil
		}},
		{"commit_comments", func(_ string, raw []byte) error {
			var c CommitComment
			if err := LoadJSON(raw, &c); err != nil {
				return err
			}
			st.CommitComments.ByID[c.ID] = &c
			st.CommitComments.ByRepo[c.RepoID] = append(st.CommitComments.ByRepo[c.RepoID], &c)
			ck := commitKey(c.RepoID, c.CommitID)
			st.CommitComments.byCommit[ck] = append(st.CommitComments.byCommit[ck], &c)
			if c.ID >= st.CommitComments.NextID {
				st.CommitComments.NextID = c.ID + 1
			}
			return nil
		}},
		{"repo_secrets", func(key string, raw []byte) error {
			var secrets map[string]*Secret
			if err := LoadJSON(raw, &secrets); err != nil {
				return err
			}
			st.RepoSecrets[key] = secrets
			return nil
		}},
		{"repo_variables", func(key string, raw []byte) error {
			var vars map[string]*ActionsVariable
			if err := LoadJSON(raw, &vars); err != nil {
				return err
			}
			st.RepoVariables[key] = vars
			return nil
		}},
		{"repo_autolinks", func(key string, raw []byte) error {
			var autolinks map[int]*RepoAutolink
			if err := LoadJSON(raw, &autolinks); err != nil {
				return err
			}
			for _, a := range autolinks {
				a.RepoKey = key
				if a.ID >= st.NextAutolinkID {
					st.NextAutolinkID = a.ID + 1
				}
			}
			st.RepoAutolinks[key] = autolinks
			return nil
		}},
		{"repo_wiki_pages", func(key string, raw []byte) error {
			var pages map[string]*WikiPage
			if err := LoadJSON(raw, &pages); err != nil {
				return err
			}
			for _, p := range pages {
				p.RepoKey = key
			}
			st.RepoWikiPages[key] = pages
			return nil
		}},
		{"repo_invitations", func(key string, raw []byte) error {
			var invitations map[int]*RepoInvitation
			if err := LoadJSON(raw, &invitations); err != nil {
				return err
			}
			for _, inv := range invitations {
				inv.RepoKey = key
				if inv.ID >= st.NextInvitationID {
					st.NextInvitationID = inv.ID + 1
				}
			}
			st.RepoInvitations[key] = invitations
			return nil
		}},
		{"repo_collaborators", func(key string, raw []byte) error {
			var collabs map[string]string
			if err := LoadJSON(raw, &collabs); err != nil {
				return err
			}
			st.RepoCollaborators[key] = collabs
			return nil
		}},
		{"repo_deploy_keys", func(key string, raw []byte) error {
			var keys map[int]*RepoDeployKey
			if err := LoadJSON(raw, &keys); err != nil {
				return err
			}
			for _, k := range keys {
				if k.ID >= st.NextDeployKeyID {
					st.NextDeployKeyID = k.ID + 1
				}
			}
			st.RepoDeployKeys[key] = keys
			return nil
		}},
		{"repo_subscriptions", func(key string, raw []byte) error {
			var sub RepoSubscription
			if err := LoadJSON(raw, &sub); err != nil {
				return err
			}
			st.RepoSubscriptions[key] = &sub
			return nil
		}},
		{"org_secrets", func(key string, raw []byte) error {
			var secrets map[string]*OrgSecret
			if err := LoadJSON(raw, &secrets); err != nil {
				return err
			}
			st.OrgSecrets[key] = secrets
			return nil
		}},
		{"org_variables", func(key string, raw []byte) error {
			var vars map[string]*ActionsVariable
			if err := LoadJSON(raw, &vars); err != nil {
				return err
			}
			st.OrgVariables[key] = vars
			return nil
		}},
		{"env_secrets", func(key string, raw []byte) error {
			var secrets map[string]*Secret
			if err := LoadJSON(raw, &secrets); err != nil {
				return err
			}
			st.EnvSecrets[key] = secrets
			return nil
		}},
		{"env_variables", func(key string, raw []byte) error {
			var vars map[string]*ActionsVariable
			if err := LoadJSON(raw, &vars); err != nil {
				return err
			}
			st.EnvVariables[key] = vars
			return nil
		}},
		{"runner_groups", func(_ string, raw []byte) error {
			var g RunnerGroup
			if err := LoadJSON(raw, &g); err != nil {
				return err
			}
			st.RunnerGroups[g.ID] = &g
			if g.ID >= st.NextRunnerGroupID {
				st.NextRunnerGroupID = g.ID + 1
			}
			return nil
		}},
		{"actions_crypto", func(key string, raw []byte) error {
			if key != "keypair" {
				return nil
			}
			var kp SecretsKeyPair
			if err := LoadJSON(raw, &kp); err != nil {
				return err
			}
			st.actionsKeyPair = &kp
			return nil
		}},
		{"check_suites", func(_ string, raw []byte) error {
			var s CheckSuite
			if err := LoadJSON(raw, &s); err != nil {
				return err
			}
			st.CheckSuites[s.ID] = &s
			if s.ID >= st.NextCheckSuiteID {
				st.NextCheckSuiteID = s.ID + 1
			}
			return nil
		}},
		{"check_runs", func(_ string, raw []byte) error {
			var cr CheckRun
			if err := LoadJSON(raw, &cr); err != nil {
				return err
			}
			st.CheckRuns[cr.ID] = &cr
			if cr.ID >= st.NextCheckRunID {
				st.NextCheckRunID = cr.ID + 1
			}
			return nil
		}},
		{"timeline_records", func(planID string, raw []byte) error {
			var records []*TimelineRecord
			if err := LoadJSON(raw, &records); err != nil {
				return err
			}
			st.TimelineRecords[planID] = records
			return nil
		}},
		{"workflows", func(_ string, raw []byte) error {
			var wf Workflow
			if err := LoadJSON(raw, &wf); err != nil {
				return err
			}
			if adoptRuns && wf.Status != WorkflowStatusCompleted {
				normalizeReloadedWorkflow(&wf)
				cancelledRuns.Put("workflows", wf.ID, &wf)
			}
			st.Workflows[wf.ID] = &wf
			if wf.RunID >= st.NextRunID {
				st.NextRunID = wf.RunID + 1
			}
			return nil
		}},
		{"workflow_attempts", func(key string, raw []byte) error {
			runID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("workflow_attempts key %q: %w", key, err)
			}
			var attempts []*Workflow
			if err := LoadJSON(raw, &attempts); err != nil {
				return err
			}
			changed := false
			for _, wf := range attempts {
				if adoptRuns && wf.Status != WorkflowStatusCompleted {
					normalizeReloadedWorkflow(wf)
					changed = true
				}
				if wf.RunID >= st.NextRunID {
					st.NextRunID = wf.RunID + 1
				}
			}
			st.WorkflowAttempts[runID] = attempts
			if changed {
				cancelledRuns.Put("workflow_attempts", key, attempts)
			}
			return nil
		}},
		{"workflow_files", func(_ string, raw []byte) error {
			var wf WorkflowFile
			if err := LoadJSON(raw, &wf); err != nil {
				return err
			}
			if st.WorkflowFiles == nil {
				st.WorkflowFiles = map[int64]*WorkflowFile{}
			}
			st.WorkflowFiles[wf.ID] = &wf
			return nil
		}},
		{"pr_reviews", func(_ string, raw []byte) error {
			var r PullRequestReview
			if err := LoadJSON(raw, &r); err != nil {
				return err
			}
			st.PRReviews[r.ID] = &r
			st.PRReviewsByPR[r.PRID] = append(st.PRReviewsByPR[r.PRID], &r)
			if r.ID >= st.NextPRReview {
				st.NextPRReview = r.ID + 1
			}
			return nil
		}},
		{"releases", func(_ string, raw []byte) error {
			var r Release
			if err := LoadJSON(raw, &r); err != nil {
				return err
			}
			st.Releases.ByID[r.ID] = &r
			st.Releases.ByRepo[r.RepoID] = append(st.Releases.ByRepo[r.RepoID], &r)
			if r.ID >= st.Releases.NextID {
				st.Releases.NextID = r.ID + 1
			}
			return nil
		}},
		{"release_assets", func(_ string, raw []byte) error {
			var a ReleaseAsset
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.Releases.assetByID[a.ID] = &a
			if rel := st.Releases.ByID[a.ReleaseID]; rel != nil {
				rel.Assets = append(rel.Assets, &a)
			}
			if a.ID >= st.Releases.nextAsset {
				st.Releases.nextAsset = a.ID + 1
			}
			return nil
		}},
		{"deployments", func(_ string, raw []byte) error {
			var d Deployment
			if err := LoadJSON(raw, &d); err != nil {
				return err
			}
			st.Deployments.deployments[d.ID] = &d
			st.Deployments.ByRepo[d.RepoID] = append(st.Deployments.ByRepo[d.RepoID], &d)
			if d.ID >= st.Deployments.nextDepID {
				st.Deployments.nextDepID = d.ID + 1
			}
			return nil
		}},
		{"deployment_statuses", func(_ string, raw []byte) error {
			var s DeploymentStatus
			if err := LoadJSON(raw, &s); err != nil {
				return err
			}
			st.Deployments.Statuses[s.ID] = &s
			// Relink onto the owning deployment (Deployment.Statuses is
			// json:"-"; deployments load before statuses). Insertion order
			// is map-random here — a post-pass below sorts by ID.
			if d := st.Deployments.deployments[s.DeploymentID]; d != nil {
				d.Statuses = append(d.Statuses, &s)
			}
			if s.ID >= st.Deployments.nextStatusID {
				st.Deployments.nextStatusID = s.ID + 1
			}
			return nil
		}},
		{"environments", func(key string, raw []byte) error {
			var e Environment
			if err := LoadJSON(raw, &e); err != nil {
				return err
			}
			// The bucket key IS the "repoID:name" map key — use it directly.
			st.Deployments.environments[key] = &e
			st.Deployments.envsByRepo[e.RepoID] = append(st.Deployments.envsByRepo[e.RepoID], &e)
			if e.ID >= st.Deployments.nextEnvID {
				st.Deployments.nextEnvID = e.ID + 1
			}
			return nil
		}},
		{"pr_review_comments", func(_ string, raw []byte) error {
			rec := prReviewCommentRecord{PRReviewComment: &PRReviewComment{}}
			if err := LoadJSON(raw, &rec); err != nil {
				return err
			}
			c := rec.restore()
			if c.ThreadID == 0 && c.InReplyToID == 0 {
				// Row predates the thread-id record field: a root comment is
				// its own thread.
				c.ThreadID = c.ID
			}
			st.PRReviewComments.ByID[c.ID] = c
			st.PRReviewComments.byPR[c.PullRequestID] = append(st.PRReviewComments.byPR[c.PullRequestID], c)
			if c.ThreadID != 0 {
				st.PRReviewComments.threadRoots[c.ID] = c.ThreadID
			}
			if c.ID >= st.PRReviewComments.NextID {
				st.PRReviewComments.NextID = c.ID + 1
			}
			return nil
		}},
		{"reactions", func(_ string, raw []byte) error {
			var reactions []*Reaction
			if err := LoadJSON(raw, &reactions); err != nil {
				return err
			}
			for _, r := range reactions {
				st.Reactions.ByID[r.ID] = r
				st.Reactions.byParent[reactionParentKey(r.ParentType, r.ParentID)] = append(st.Reactions.byParent[reactionParentKey(r.ParentType, r.ParentID)], r)
				if r.ID >= st.Reactions.NextID {
					st.Reactions.NextID = r.ID + 1
				}
			}
			return nil
		}},
		{"projects_v2", func(_ string, raw []byte) error {
			var p ProjectV2
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.ProjectsV2.projects[p.ID] = &p
			if p.ID >= st.ProjectsV2.nextProjectID {
				st.ProjectsV2.nextProjectID = p.ID + 1
			}
			return nil
		}},
		{"project_v2_items", func(_ string, raw []byte) error {
			var it ProjectV2Item
			if err := LoadJSON(raw, &it); err != nil {
				return err
			}
			st.ProjectsV2.items[it.ID] = &it
			if it.ContentID != 0 {
				st.ProjectsV2.itemsByOwner[it.ContentID] = append(st.ProjectsV2.itemsByOwner[it.ContentID], &it)
			}
			if it.ID >= st.ProjectsV2.nextItemID {
				st.ProjectsV2.nextItemID = it.ID + 1
			}
			return nil
		}},
		{"project_v2_fields", func(_ string, raw []byte) error {
			var f ProjectV2Field
			if err := LoadJSON(raw, &f); err != nil {
				return err
			}
			st.ProjectsV2.fields[f.ID] = &f
			st.ProjectsV2.FieldsByProj[f.ProjectID] = append(st.ProjectsV2.FieldsByProj[f.ProjectID], &f)
			if f.ID >= st.ProjectsV2.nextFieldID {
				st.ProjectsV2.nextFieldID = f.ID + 1
			}
			// Option IDs are hex renderings of nextOptionSeed; resume the
			// seed past every loaded option so new options can't collide.
			for _, opt := range f.Options {
				// Upper-bound the parsed value before narrowing to int so a
				// tampered persisted ID can't truncate on a 32-bit build.
				if n, err := strconv.ParseInt(opt.ID, 16, 64); err == nil && n >= 0 && n < 1<<31 && int(n) >= st.ProjectsV2.nextOptionSeed {
					st.ProjectsV2.nextOptionSeed = int(n) + 1
				}
			}
			return nil
		}},
	} {
		rows, err := st.Persist.List(loadFn.Name)
		if err != nil {
			return fmt.Errorf("load %s: %w", loadFn.Name, err)
		}
		for k, raw := range rows {
			if err := loadFn.fn(k, raw); err != nil {
				return fmt.Errorf("decode %s row: %w", loadFn.Name, err)
			}
		}
	}
	if err := cancelledRuns.Commit(); err != nil {
		return fmt.Errorf("record interrupted workflow runs as cancelled: %w", err)
	}

	// Deployment statuses were relinked in map-iteration order; restore
	// creation (ID) order, which is what AddStatus produces.
	for _, d := range st.Deployments.deployments {
		sort.Slice(d.Statuses, func(i, j int) bool { return d.Statuses[i].ID < d.Statuses[j].ID })
	}

	for _, loadFn := range []struct {
		Name string `json:"-"`
		fn   func(string, []byte) error
	}{
		{"misc", func(key string, raw []byte) error {
			switch key {
			case "oidc_claim_keys":
				var keys map[string][]string
				if err := LoadJSON(raw, &keys); err != nil {
					return err
				}
				st.Misc.OidcClaimKeys = keys
			case "follows":
				var follows map[string]map[string]bool
				if err := LoadJSON(raw, &follows); err != nil {
					return err
				}
				st.Misc.Follows = follows
			case "blocked_users":
				var blocked map[int]map[int]bool
				if err := LoadJSON(raw, &blocked); err != nil {
					return err
				}
				st.Misc.blockedUsers = blocked
			case "social_accounts":
				var accounts map[int][]map[string]interface{}
				if err := LoadJSON(raw, &accounts); err != nil {
					return err
				}
				st.Misc.socialAccounts = accounts
			case "ssh_signing_keys":
				var keys map[int][]map[string]interface{}
				if err := LoadJSON(raw, &keys); err != nil {
					return err
				}
				st.Misc.sshSigningKeys = keys
			}
			return nil
		}},
		{"user_keys", func(_ string, raw []byte) error {
			var k UserKey
			if err := LoadJSON(raw, &k); err != nil {
				return err
			}
			if err := CacheParsedKey(&k); err != nil {
				st.Logger.Warn().Err(err).Msg("persisted SSH key does not parse and will never authenticate")
			}
			st.Misc.UserKeys[k.ID] = &k
			st.Misc.KeysByUser[k.UserID] = append(st.Misc.KeysByUser[k.UserID], &k)
			if k.ID >= st.Misc.NextKeyID {
				st.Misc.NextKeyID = k.ID + 1
			}
			return nil
		}},
		{"pages_sites", func(key string, raw []byte) error {
			repoID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("pages_sites key %q: %w", key, err)
			}
			var site PagesSite
			if err := LoadJSON(raw, &site); err != nil {
				return err
			}
			st.Misc.PagesByRepo[repoID] = &site
			return nil
		}},
		{"branch_protection", func(key string, raw []byte) error {
			var bp BranchProtection
			if err := LoadJSON(raw, &bp); err != nil {
				return err
			}
			st.Misc.BranchProtection[key] = &bp
			return nil
		}},
		{"gpg_keys", func(_ string, raw []byte) error {
			var k GPGKey
			if err := LoadJSON(raw, &k); err != nil {
				return err
			}
			st.Misc.GpgKeys[k.ID] = &k
			st.Misc.GpgKeysByUser[k.UserID] = append(st.Misc.GpgKeysByUser[k.UserID], &k)
			if k.ID >= st.Misc.NextGPGKeyID {
				st.Misc.NextGPGKeyID = k.ID + 1
			}
			return nil
		}},
		{"pages_builds", func(key string, raw []byte) error {
			var builds []*PagesBuild
			if err := LoadJSON(raw, &builds); err != nil {
				return err
			}
			st.Misc.PagesBuilds[key] = builds
			for _, b := range builds {
				if b.ID == 0 {
					b.ID = pagesBuildIDFromURL(b.URL)
				}
				if b.ID >= st.Misc.NextPagesBuildID {
					st.Misc.NextPagesBuildID = b.ID + 1
				}
			}
			return nil
		}},
		{"audit_log", func(_ string, raw []byte) error {
			var e AuditEntry
			if err := LoadJSON(raw, &e); err != nil {
				return err
			}
			st.Misc.AuditLog = append(st.Misc.AuditLog, &e)
			// nextAuditID is pre-incremented before use; resume AT the max.
			if e.ID > st.Misc.NextAuditID {
				st.Misc.NextAuditID = e.ID
			}
			return nil
		}},
		{"admin_audit_log", func(_ string, raw []byte) error {
			var e AuditLogEvent
			if err := LoadJSON(raw, &e); err != nil {
				return err
			}
			if e.Timestamp != "" {
				if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
					e.CreatedAt = ts
				}
			}
			st.Misc.AuditLogEvents = append(st.Misc.AuditLogEvents, &e)
			if e.ID > st.Misc.NextAdminAuditID {
				st.Misc.NextAdminAuditID = e.ID
			}
			return nil
		}},
		{"marketplace_plans", func(_ string, raw []byte) error {
			var p MarketplacePlan
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.Misc.marketplacePlans[p.ID] = &p
			if p.ID >= st.Misc.nextMarketplacePlanID {
				st.Misc.nextMarketplacePlanID = p.ID + 1
			}
			return nil
		}},
		{"marketplace_listings", func(_ string, raw []byte) error {
			var listing MarketplaceListing
			if err := LoadJSON(raw, &listing); err != nil {
				return err
			}
			st.Misc.marketplaceListings[strings.ToLower(listing.Slug)] = &listing
			if listing.WebhookID >= st.NextHookID {
				st.NextHookID = listing.WebhookID + 1
			}
			return nil
		}},
		{"marketplace_deliveries", func(key string, raw []byte) error {
			var deliveries []*WebhookDelivery
			if err := LoadJSON(raw, &deliveries); err != nil {
				return err
			}
			st.Misc.marketplaceDeliveries[strings.ToLower(key)] = deliveries
			for _, delivery := range deliveries {
				if delivery.ID >= st.Misc.nextMarketplaceDeliveryID {
					st.Misc.nextMarketplaceDeliveryID = delivery.ID + 1
				}
			}
			return nil
		}},
		{"notifications_state", func(key string, raw []byte) error {
			var state UserNotificationsState
			if err := LoadJSON(raw, &state); err != nil {
				return err
			}
			userID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("notifications_state key %q: %w", key, err)
			}
			st.NotificationsState[userID] = &state
			return nil
		}},
		{"repo_rulesets", func(key string, raw []byte) error {
			var rs Ruleset
			if err := LoadJSON(raw, &rs); err != nil {
				return err
			}
			if rs.Versions == nil {
				rs.Versions = map[int]RulesetVersion{}
			}
			st.Rulesets[rs.ID] = &rs
			if rs.ID >= st.NextRulesetID {
				st.NextRulesetID = rs.ID + 1
			}
			return nil
		}},
		{"ruleset_suites", func(key string, raw []byte) error {
			var suite RulesetSuite
			if err := LoadJSON(raw, &suite); err != nil {
				return err
			}
			st.RulesetSuites[suite.ID] = &suite
			if suite.ID >= st.NextRulesetSuiteID {
				st.NextRulesetSuiteID = suite.ID + 1
			}
			return nil
		}},
		{"projects_classic", func(_ string, raw []byte) error {
			var p ProjectClassic
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.ProjectClassic[p.ID] = &p
			if p.ID >= st.NextProjectClassicID {
				st.NextProjectClassicID = p.ID + 1
			}
			return nil
		}},
		{"project_columns", func(_ string, raw []byte) error {
			var c ProjectColumn
			if err := LoadJSON(raw, &c); err != nil {
				return err
			}
			st.ProjectColumns[c.ID] = &c
			if c.ID >= st.NextProjectColumnID {
				st.NextProjectColumnID = c.ID + 1
			}
			return nil
		}},
		{"project_cards", func(_ string, raw []byte) error {
			var c ProjectCard
			if err := LoadJSON(raw, &c); err != nil {
				return err
			}
			st.ProjectCards[c.ID] = &c
			if c.ID >= st.NextProjectCardID {
				st.NextProjectCardID = c.ID + 1
			}
			return nil
		}},
		{"secret_scanning_alerts", func(_ string, raw []byte) error {
			var a SecretScanningAlert
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.SecretScanningAlerts[a.ID] = &a
			if st.SecretScanningAlertsByRepo[a.RepoKey] == nil {
				st.SecretScanningAlertsByRepo[a.RepoKey] = make(map[int]*SecretScanningAlert)
			}
			st.SecretScanningAlertsByRepo[a.RepoKey][a.Number] = &a
			if a.Number >= st.SecretScanningNextNumber[a.RepoKey] {
				st.SecretScanningNextNumber[a.RepoKey] = a.Number + 1
			}
			if a.ID >= st.NextSecretScanningAlertID {
				st.NextSecretScanningAlertID = a.ID + 1
			}
			return nil
		}},
		{"code_scanning_alerts", func(_ string, raw []byte) error {
			var a CodeScanningAlert
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.CodeScanningAlerts[a.ID] = &a
			if st.CodeScanningAlertsByRepo[a.RepoKey] == nil {
				st.CodeScanningAlertsByRepo[a.RepoKey] = make(map[int]*CodeScanningAlert)
			}
			st.CodeScanningAlertsByRepo[a.RepoKey][a.Number] = &a
			if a.Number >= st.CodeScanningNextNumber[a.RepoKey] {
				st.CodeScanningNextNumber[a.RepoKey] = a.Number + 1
			}
			if a.ID >= st.NextCodeScanningAlertID {
				st.NextCodeScanningAlertID = a.ID + 1
			}
			return nil
		}},
		{"code_scanning_analyses", func(_ string, raw []byte) error {
			var a CodeScanningAnalysis
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.CodeScanningAnalyses[a.ID] = &a
			if st.CodeScanningAnalysesByRepo[a.RepoKey] == nil {
				st.CodeScanningAnalysesByRepo[a.RepoKey] = make(map[int]*CodeScanningAnalysis)
			}
			st.CodeScanningAnalysesByRepo[a.RepoKey][a.ID] = &a
			if a.ID >= st.NextCodeScanningAnalysisID {
				st.NextCodeScanningAnalysisID = a.ID + 1
			}
			return nil
		}},
		{"code_scanning_default_setups", func(_ string, raw []byte) error {
			var setup CodeScanningDefaultSetup
			if err := LoadJSON(raw, &setup); err != nil {
				return err
			}
			st.CodeScanningDefaultSetups[setup.RepoKey] = &setup
			return nil
		}},
		{"sarif_uploads", func(key string, raw []byte) error {
			var up SARIFUpload
			if err := LoadJSON(raw, &up); err != nil {
				return err
			}
			st.SARIFUploads[key] = &up
			return nil
		}},
		{"dependabot_alerts", func(_ string, raw []byte) error {
			var a DependabotAlert
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.DependabotAlerts[a.ID] = &a
			if st.DependabotAlertsByRepo[a.RepoKey] == nil {
				st.DependabotAlertsByRepo[a.RepoKey] = make(map[int]*DependabotAlert)
			}
			st.DependabotAlertsByRepo[a.RepoKey][a.Number] = &a
			if a.Number >= st.DependabotNextNumber[a.RepoKey] {
				st.DependabotNextNumber[a.RepoKey] = a.Number + 1
			}
			if a.ID >= st.NextDependabotAlertID {
				st.NextDependabotAlertID = a.ID + 1
			}
			return nil
		}},
		{"dependabot_secrets", func(key string, raw []byte) error {
			var m map[string]*DependabotSecret
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.DependabotSecrets[key] = m
			return nil
		}},
		{"dependabot_org_secrets", func(key string, raw []byte) error {
			var m map[string]*DependabotOrgSecret
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.DependabotOrgSecrets[key] = m
			return nil
		}},
		{"dependabot_user_secrets", func(key string, raw []byte) error {
			var m map[string]*DependabotUserSecret
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.DependabotUserSecrets[key] = m
			return nil
		}},
		{"dependabot_repo_access", func(key string, raw []byte) error {
			var ids []int
			if err := LoadJSON(raw, &ids); err != nil {
				return err
			}
			st.DependabotRepositoryAccess[key] = ids
			return nil
		}},
		{"user_migrations", func(_ string, raw []byte) error {
			var r userMigrationRecord
			if err := LoadJSON(raw, &r); err != nil {
				return err
			}
			m := recordToUserMigration(&r)
			st.UserMigrations[m.ID] = m
			if m.ID >= st.NextUserMigrationID {
				st.NextUserMigrationID = m.ID + 1
			}
			return nil
		}},
		{"org_migrations", func(_ string, raw []byte) error {
			var r orgMigrationRecord
			if err := LoadJSON(raw, &r); err != nil {
				return err
			}
			m := recordToOrgMigration(&r)
			st.OrgMigrations[m.ID] = m
			if m.ID >= st.NextOrgMigrationID {
				st.NextOrgMigrationID = m.ID + 1
			}
			return nil
		}},
		{"discussion_categories", func(_ string, raw []byte) error {
			var cat DiscussionCategory
			if err := LoadJSON(raw, &cat); err != nil {
				return err
			}
			st.DiscussionCategories[cat.ID] = &cat
			if cat.ID >= st.NextDiscussionCategoryID {
				st.NextDiscussionCategoryID = cat.ID + 1
			}
			return nil
		}},
		{"discussions", func(_ string, raw []byte) error {
			var d Discussion
			if err := LoadJSON(raw, &d); err != nil {
				return err
			}
			st.Discussions[d.ID] = &d
			if d.ID >= st.NextDiscussionID {
				st.NextDiscussionID = d.ID + 1
			}
			if d.Number >= st.NextDiscussionNumber[d.RepoID] {
				st.NextDiscussionNumber[d.RepoID] = d.Number + 1
			}
			return nil
		}},
		{"discussion_comments", func(_ string, raw []byte) error {
			var c DiscussionComment
			if err := LoadJSON(raw, &c); err != nil {
				return err
			}
			st.DiscussionComments[c.ID] = &c
			if c.ID >= st.NextDiscussionCommentID {
				st.NextDiscussionCommentID = c.ID + 1
			}
			return nil
		}},
		{"org_actions_permissions", func(key string, raw []byte) error {
			var p OrgActionsPermissions
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.OrgActionsPermissions[key] = &p
			return nil
		}},
		{"repo_actions_permissions", func(key string, raw []byte) error {
			var p RepoActionsPermissions
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.RepoActionsPermissions[key] = &p
			return nil
		}},
		{"packages", func(_ string, raw []byte) error {
			var p Package
			if err := LoadJSON(raw, &p); err != nil {
				return err
			}
			st.Packages[p.ID] = &p
			// Soft-deleted packages stay out of the by-owner key map so
			// lists/gets skip them while restore can still find them.
			if !p.Deleted {
				if st.PackagesByOwnerKey[p.OwnerKey] == nil {
					st.PackagesByOwnerKey[p.OwnerKey] = map[string]*Package{}
				}
				st.PackagesByOwnerKey[p.OwnerKey][PackageKey(p.PackageType, p.Name)] = &p
			}
			if p.ID >= st.NextPackageID {
				st.NextPackageID = p.ID + 1
			}
			return nil
		}},
		{"package_versions", func(_ string, raw []byte) error {
			var v PackageVersion
			if err := LoadJSON(raw, &v); err != nil {
				return err
			}
			st.PackageVersions[v.ID] = &v
			if st.PackageVersionsByPackage[v.PackageID] == nil {
				st.PackageVersionsByPackage[v.PackageID] = map[int]*PackageVersion{}
			}
			st.PackageVersionsByPackage[v.PackageID][v.ID] = &v
			if v.ID >= st.NextPackageVersionID {
				st.NextPackageVersionID = v.ID + 1
			}
			return nil
		}},
		{"package_files", func(_ string, raw []byte) error {
			var f PackageFile
			if err := LoadJSON(raw, &f); err != nil {
				return err
			}
			st.PackageFiles[f.ID] = &f
			if st.PackageFilesByVersion[f.VersionID] == nil {
				st.PackageFilesByVersion[f.VersionID] = map[int]*PackageFile{}
			}
			st.PackageFilesByVersion[f.VersionID][f.ID] = &f
			if f.ID >= st.NextPackageFileID {
				st.NextPackageFileID = f.ID + 1
			}
			return nil
		}},
		{"security_advisories", func(_ string, raw []byte) error {
			var a SecurityAdvisory
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.SecurityAdvisories[a.ID] = &a
			if repo := st.Repos[a.RepoID]; repo != nil {
				if st.SecurityAdvisoriesByRepo[repo.FullName] == nil {
					st.SecurityAdvisoriesByRepo[repo.FullName] = map[string]*SecurityAdvisory{}
				}
				st.SecurityAdvisoriesByRepo[repo.FullName][a.GHSAID] = &a
			}
			if a.ID >= st.NextSecurityAdvisoryID {
				st.NextSecurityAdvisoryID = a.ID + 1
			}
			return nil
		}},
		{"security_advisory_reports", func(_ string, raw []byte) error {
			var r SecurityAdvisoryReport
			if err := LoadJSON(raw, &r); err != nil {
				return err
			}
			st.SecurityAdvisoryReports[r.ID] = &r
			if r.ID >= st.NextSecurityAdvisoryReportID {
				st.NextSecurityAdvisoryReportID = r.ID + 1
			}
			return nil
		}},
		// org billing budgets
		{"org_budgets", func(key string, raw []byte) error {
			var m map[string]*OrgBudget
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgBudgets[key] = m
			return nil
		}},
		// API insights
		{"api_insights_requests", func(_ string, raw []byte) error {
			var rec APIRequestRecord
			if err := LoadJSON(raw, &rec); err != nil {
				return err
			}
			st.APIRequestRecords = append(st.APIRequestRecords, &rec)
			if rec.ID >= st.NextAPIRequestID {
				st.NextAPIRequestID = rec.ID + 1
			}
			return nil
		}},
		// fine-grained personal access token administration
		{"org_pat_grant_requests", func(key string, raw []byte) error {
			var m map[int]*OrgPATGrantRequest
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgPATGrantRequests[key] = m
			for _, req := range m {
				if req.ID >= st.NextPATRequestID {
					st.NextPATRequestID = req.ID + 1
				}
				if req.TokenID >= st.NextPATTokenID {
					st.NextPATTokenID = req.TokenID + 1
				}
			}
			return nil
		}},
		{"org_pat_grants", func(key string, raw []byte) error {
			var m map[int]*OrgPATGrant
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgPATGrants[key] = m
			for _, g := range m {
				if g.ID >= st.NextPATGrantID {
					st.NextPATGrantID = g.ID + 1
				}
				if g.TokenID >= st.NextPATTokenID {
					st.NextPATTokenID = g.TokenID + 1
				}
			}
			return nil
		}},
		// org codespaces access settings
		{"org_codespaces_access", func(key string, raw []byte) error {
			var a OrgCodespacesAccess
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.OrgCodespacesAccess[key] = &a
			return nil
		}},
		// Dependabot repository access default level
		{"dependabot_repo_access_default_level", func(key string, raw []byte) error {
			var level string
			if err := LoadJSON(raw, &level); err != nil {
				return err
			}
			st.DependabotRepoAccessDefaultLevel[key] = level
			return nil
		}},
		// secret scanning pattern configurations + push protection
		{"secret_scanning_pattern_configs", func(key string, raw []byte) error {
			var cfg OrgSecretScanningPatternConfig
			if err := LoadJSON(raw, &cfg); err != nil {
				return err
			}
			st.SecretScanningPatternConfigs[key] = &cfg
			return nil
		}},
		{"secret_scanning_push_placeholders", func(key string, raw []byte) error {
			var m map[string]*SecretScanningPushProtectionPlaceholder
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.SecretScanningPushPlaceholders[key] = m
			return nil
		}},
		{"secret_scanning_push_bypasses", func(key string, raw []byte) error {
			var list []*SecretScanningPushProtectionBypass
			if err := LoadJSON(raw, &list); err != nil {
				return err
			}
			st.SecretScanningPushBypasses[key] = list
			return nil
		}},
	} {
		rows, err := st.Persist.List(loadFn.Name)
		if err != nil {
			return fmt.Errorf("load %s: %w", loadFn.Name, err)
		}
		for k, raw := range rows {
			if err := loadFn.fn(k, raw); err != nil {
				return fmt.Errorf("decode %s row: %w", loadFn.Name, err)
			}
		}
	}

	// Audit entries arrive in map-iteration order; the in-memory log is
	// newest-first (recordAuditEvent prepends), so sort by ID descending.
	sort.Slice(st.Misc.AuditLog, func(i, j int) bool { return st.Misc.AuditLog[i].ID > st.Misc.AuditLog[j].ID })
	sort.Slice(st.Misc.AuditLogEvents, func(i, j int) bool { return st.Misc.AuditLogEvents[i].ID > st.Misc.AuditLogEvents[j].ID })

	// API request records arrive in map-iteration order; the in-memory log
	// is oldest-first (RecordAPIRequest appends), so sort by ID ascending.
	sort.Slice(st.APIRequestRecords, func(i, j int) bool { return st.APIRequestRecords[i].ID < st.APIRequestRecords[j].ID })
	recordCap := st.ApiRequestRecordCap
	if recordCap <= 0 {
		recordCap = maxAPIRequestRecords
	}
	if overflow := len(st.APIRequestRecords) - recordCap; overflow > 0 {
		// Reclaim durable rows beyond the cap that a pre-fix instance leaked, so
		// the bucket converges to maxAPIRequestRecords instead of only being
		// trimmed in memory on every load (STORE-024).
		for _, excess := range st.APIRequestRecords[:overflow] {
			if err := st.Persist.Delete("api_insights_requests", strconv.FormatInt(excess.ID, 10)); err != nil {
				return fmt.Errorf("prune excess api insights request %d: %w", excess.ID, err)
			}
		}
		st.APIRequestRecords = append([]*APIRequestRecord(nil), st.APIRequestRecords[overflow:]...)
	}

	if v, err := st.Persist.GetCounter("next_run_id"); err != nil {
		return fmt.Errorf("load counter next_run_id: %w", err)
	} else if int(v) > st.NextRunID {
		st.NextRunID = int(v)
	}
	if v, err := st.Persist.GetCounter("next_log_id"); err != nil {
		return fmt.Errorf("load counter next_log_id: %w", err)
	} else if int(v) > st.NextLog {
		st.NextLog = int(v)
	}

	// enterprises
	if err := st.loadBucket("enterprise_teams", func(raw []byte) error {
		var t EnterpriseTeam
		if err := LoadJSON(raw, &t); err != nil {
			return err
		}
		st.EnterpriseTeams[t.ID] = &t
		st.EnterpriseTeamsBySlug[t.Slug] = &t
		if t.ID >= st.NextEnterpriseTeamID {
			st.NextEnterpriseTeamID = t.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	// teams-people
	if err := st.loadBucket("org_invitations", func(raw []byte) error {
		var inv OrgInvitation
		if err := LoadJSON(raw, &inv); err != nil {
			return err
		}
		st.OrgInvitations[inv.ID] = &inv
		if inv.ID >= st.NextOrgInvitationID {
			st.NextOrgInvitationID = inv.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	// hosted-runners
	if err := st.loadBucket("hosted_runners", func(raw []byte) error {
		var hr HostedRunner
		if err := LoadJSON(raw, &hr); err != nil {
			return err
		}
		st.HostedRunners[hr.ID] = &hr
		if hr.ID >= st.NextHostedRunnerID {
			st.NextHostedRunnerID = hr.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_code_security_configs", func(raw []byte) error {
		var c EnterpriseCodeSecurityConfiguration
		if err := LoadJSON(raw, &c); err != nil {
			return err
		}
		st.EnterpriseCodeSecurityConfigs[c.ID] = &c
		if c.ID >= st.NextEnterpriseCodeSecurityConfigID {
			st.NextEnterpriseCodeSecurityConfigID = c.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("hosted_runner_custom_images", func(raw []byte) error {
		var img HostedRunnerCustomImage
		if err := LoadJSON(raw, &img); err != nil {
			return err
		}
		st.HostedRunnerCustomImages[img.ID] = &img
		if img.ID >= st.NextHostedRunnerImageID {
			st.NextHostedRunnerImageID = img.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_code_security_attachments", func(raw []byte) error {
		var a EnterpriseCodeSecurityAttachment
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.EnterpriseCodeSecurityRepoConfigs[a.RepoID] = a.ConfigID
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_settings", func(raw []byte) error {
		var s EnterpriseSettings
		if err := LoadJSON(raw, &s); err != nil {
			return err
		}
		st.EnterpriseSettings = normalizeEnterpriseSettings(&s)
		for _, hook := range st.EnterpriseSettings.GHESGlobalHooks {
			if hook.ID >= st.NextHookID {
				st.NextHookID = hook.ID + 1
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// projects-v2 views
	if err := st.loadBucket("project_v2_views", func(raw []byte) error {
		var v ProjectV2View
		if err := LoadJSON(raw, &v); err != nil {
			return err
		}
		st.ProjectsV2.views[v.ID] = &v
		st.ProjectsV2.viewsByProj[v.ProjectID] = append(st.ProjectsV2.viewsByProj[v.ProjectID], &v)
		if v.ID >= st.ProjectsV2.nextViewID {
			st.ProjectsV2.nextViewID = v.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	// Rows arrive in map-iteration order; restore per-project creation
	// (ID) order for fields, views, and per-content item slices. Iteration
	// IDs share the option-seed space with single-select option IDs, so
	// resume the seed past them too.
	for _, views := range st.ProjectsV2.viewsByProj {
		sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	}
	for _, fields := range st.ProjectsV2.FieldsByProj {
		sort.Slice(fields, func(i, j int) bool { return fields[i].ID < fields[j].ID })
	}
	for _, items := range st.ProjectsV2.itemsByOwner {
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	}
	for _, f := range st.ProjectsV2.fields {
		if f.Iteration == nil {
			continue
		}
		for _, iter := range f.Iteration.Iterations {
			// Upper-bound the parsed value before narrowing to int (see above).
			if n, err := strconv.ParseInt(iter.ID, 16, 64); err == nil && n >= 0 && n < 1<<31 && int(n) >= st.ProjectsV2.nextOptionSeed {
				st.ProjectsV2.nextOptionSeed = int(n) + 1
			}
		}
	}

	// org governance surfaces
	// agents-codescan: GitHub Copilot coding agent secrets/variables/tasks
	// and CodeQL databases/variant analyses.
	for _, loadFn := range []struct {
		Name string `json:"-"`
		fn   func(key string, raw []byte) error
	}{
		{"code_security_configurations", func(key string, raw []byte) error {
			var m map[int]*CodeSecurityConfiguration
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.CodeSecurityConfigs[key] = m
			for id := range m {
				if id >= st.NextCodeSecurityConfigID {
					st.NextCodeSecurityConfigID = id + 1
				}
			}
			return nil
		}},
		{"code_security_repo_attachments", func(key string, raw []byte) error {
			var m map[int]int
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.CodeSecurityRepoAttachments[key] = m
			return nil
		}},
		{"org_custom_properties", func(key string, raw []byte) error {
			var m map[string]*CustomProperty
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgCustomProperties[key] = m
			return nil
		}},
		{"repo_custom_property_values", func(key string, raw []byte) error {
			var m map[string]interface{}
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.RepoCustomPropertyValues[key] = m
			return nil
		}},
		{"org_issue_types", func(key string, raw []byte) error {
			var m map[int]*IssueType
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgIssueTypes[key] = m
			for id, it := range m {
				st.IssueTypesByID[id] = it
				if id >= st.NextIssueTypeID {
					st.NextIssueTypeID = id + 1
				}
			}
			return nil
		}},
		{"org_issue_fields", func(key string, raw []byte) error {
			var m map[int]*IssueField
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgIssueFields[key] = m
			for id, f := range m {
				if id >= st.NextIssueFieldID {
					st.NextIssueFieldID = id + 1
				}
				for _, opt := range f.Options {
					if opt.ID >= st.NextIssueFieldOptionID {
						st.NextIssueFieldOptionID = opt.ID + 1
					}
				}
			}
			return nil
		}},
		{"issue_field_values", func(key string, raw []byte) error {
			issueID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("issue_field_values key %q: %w", key, err)
			}
			var m map[int]interface{}
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.IssueFieldValues[issueID] = m
			return nil
		}},
		{"org_campaigns", func(key string, raw []byte) error {
			var m map[int]*Campaign
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgCampaigns[key] = m
			return nil
		}},
		{"org_private_registries", func(key string, raw []byte) error {
			var m map[string]*privateRegistryConfigurationPersist
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			out := make(map[string]*PrivateRegistryConfiguration, len(m))
			for name, p := range m {
				out[name] = privateRegistryFromPersist(p)
			}
			st.OrgPrivateRegistries[key] = out
			return nil
		}},
		{"org_network_configurations", func(key string, raw []byte) error {
			var m map[string]*NetworkConfiguration
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgNetworkConfigurations[key] = m
			return nil
		}},
		{"org_network_settings", func(key string, raw []byte) error {
			var m map[string]*NetworkSettingsResource
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.OrgNetworkSettings[key] = m
			return nil
		}},
		{"org_immutable_releases", func(key string, raw []byte) error {
			var s OrgImmutableReleasesSettings
			if err := LoadJSON(raw, &s); err != nil {
				return err
			}
			st.OrgImmutableReleases[key] = &s
			return nil
		}},
		{"repo_immutable_releases", func(key string, raw []byte) error {
			var enabled bool
			if err := LoadJSON(raw, &enabled); err != nil {
				return err
			}
			st.RepoImmutableReleases[key] = enabled
			return nil
		}},
		{"agents_repo_secrets", func(key string, raw []byte) error {
			var m map[string]*Secret
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.AgentsRepoSecrets[key] = m
			return nil
		}},
		{"agents_org_secrets", func(key string, raw []byte) error {
			var m map[string]*OrgSecret
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.AgentsOrgSecrets[key] = m
			return nil
		}},
		{"agents_repo_variables", func(key string, raw []byte) error {
			var m map[string]*ActionsVariable
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.AgentsRepoVariables[key] = m
			return nil
		}},
		{"agents_org_variables", func(key string, raw []byte) error {
			var m map[string]*ActionsVariable
			if err := LoadJSON(raw, &m); err != nil {
				return err
			}
			st.AgentsOrgVariables[key] = m
			return nil
		}},
		{"agent_tasks", func(key string, raw []byte) error {
			var task AgentTask
			if err := LoadJSON(raw, &task); err != nil {
				return err
			}
			st.AgentTasks[key] = &task
			return nil
		}},
		{"code_scanning_autofixes", func(key string, raw []byte) error {
			var a CodeScanningAutofix
			if err := LoadJSON(raw, &a); err != nil {
				return err
			}
			st.CodeScanningAutofixes[key] = &a
			return nil
		}},
		{"codeql_databases", func(_ string, raw []byte) error {
			var db CodeQLDatabase
			if err := LoadJSON(raw, &db); err != nil {
				return err
			}
			st.CodeQLDatabases[db.ID] = &db
			if st.CodeQLDatabasesByRepo[db.RepoKey] == nil {
				st.CodeQLDatabasesByRepo[db.RepoKey] = make(map[string]*CodeQLDatabase)
			}
			st.CodeQLDatabasesByRepo[db.RepoKey][db.Language] = &db
			if db.ID >= st.NextCodeQLDatabaseID {
				st.NextCodeQLDatabaseID = db.ID + 1
			}
			return nil
		}},
		{"codeql_variant_analyses", func(_ string, raw []byte) error {
			var va CodeQLVariantAnalysis
			if err := LoadJSON(raw, &va); err != nil {
				return err
			}
			st.CodeQLVariantAnalyses[va.ID] = &va
			if va.ID >= st.NextCodeQLVariantAnalysisID {
				st.NextCodeQLVariantAnalysisID = va.ID + 1
			}
			return nil
		}},
	} {
		rows, err := st.Persist.List(loadFn.Name)
		if err != nil {
			return fmt.Errorf("load %s: %w", loadFn.Name, err)
		}
		for k, raw := range rows {
			if err := loadFn.fn(k, raw); err != nil {
				return fmt.Errorf("decode %s row: %w", loadFn.Name, err)
			}
		}
	}

	// attestations
	if err := st.loadBucket("attestations", func(raw []byte) error {
		var a Attestation
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.Attestations[a.ID] = &a
		if a.ID >= st.NextAttestationID {
			st.NextAttestationID = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}

	// org artifact metadata records
	if err := st.loadBucket("artifact_storage_records", func(raw []byte) error {
		var rec ArtifactStorageRecord
		if err := LoadJSON(raw, &rec); err != nil {
			return err
		}
		st.ArtifactStorageRecords[rec.ID] = &rec
		if rec.ID >= st.NextArtifactStorageRecordID {
			st.NextArtifactStorageRecordID = rec.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("artifact_deployment_records", func(raw []byte) error {
		var rec ArtifactDeploymentRecord
		if err := LoadJSON(raw, &rec); err != nil {
			return err
		}
		st.ArtifactDeploymentRecords[rec.ID] = &rec
		if rec.ID >= st.NextArtifactDeploymentRecordID {
			st.NextArtifactDeploymentRecordID = rec.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("artifact_deployment_jobs", func(raw []byte) error {
		var job ArtifactDeploymentJob
		if err := LoadJSON(raw, &job); err != nil {
			return err
		}
		st.ArtifactDeploymentJobs[job.ID] = &job
		if job.ID >= st.NextArtifactDeploymentJobID {
			st.NextArtifactDeploymentJobID = job.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}

	// copilot + code quality
	if err := st.loadBucket("copilot_seats", func(raw []byte) error {
		var seat CopilotSeat
		if err := LoadJSON(raw, &seat); err != nil {
			return err
		}
		if st.CopilotSeats[seat.OrgLogin] == nil {
			st.CopilotSeats[seat.OrgLogin] = map[int]*CopilotSeat{}
		}
		st.CopilotSeats[seat.OrgLogin][seat.UserID] = &seat
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("copilot_content_exclusions", func(raw []byte) error {
		var ce CopilotContentExclusion
		if err := LoadJSON(raw, &ce); err != nil {
			return err
		}
		st.CopilotContentExclusions[ce.OrgLogin] = &ce
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("copilot_coding_agent_permissions", func(raw []byte) error {
		var p CopilotCodingAgentPermissions
		if err := LoadJSON(raw, &p); err != nil {
			return err
		}
		st.CopilotCodingAgentPerms[p.OrgLogin] = &p
		return nil
	}); err != nil {
		return err
	}
	// User-surface: GitHub Marketplace subscriptions are independent for
	// every listing and purchasing account.
	if err := st.loadBucket("marketplace_purchases", func(raw []byte) error {
		var p MarketplacePurchase
		if err := LoadJSON(raw, &p); err != nil {
			return err
		}
		if p.ListingSlug == "" || p.AccountType == "" {
			return fmt.Errorf("marketplace purchase is missing listing or account identity")
		}
		st.Misc.MarketplacePurchases[MarketplacePurchaseKey(p.ListingSlug, p.AccountType, p.AccountID)] = &p
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("copilot_spaces", func(raw []byte) error {
		var space CopilotSpace
		if err := LoadJSON(raw, &space); err != nil {
			return err
		}
		st.CopilotSpaces[space.ID] = &space
		if space.ID >= st.NextCopilotSpaceID {
			st.NextCopilotSpaceID = space.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("code_quality_setups", func(raw []byte) error {
		var setup CodeQualitySetup
		if err := LoadJSON(raw, &setup); err != nil {
			return err
		}
		st.CodeQualitySetups[setup.RepoFullName] = &setup
		return nil
	}); err != nil {
		return err
	}
	if rows, err := st.Persist.List("code_quality_findings"); err != nil {
		return fmt.Errorf("load code_quality_findings: %w", err)
	} else {
		for repoKey, raw := range rows {
			findings := map[int]*CodeQualityFinding{}
			if err := LoadJSON(raw, &findings); err != nil {
				return fmt.Errorf("decode code_quality_findings row: %w", err)
			}
			st.CodeQualityFindings[repoKey] = findings
		}
	}
	if rows, err := st.Persist.List("secret_scanning_custom_patterns"); err != nil {
		return fmt.Errorf("load secret_scanning_custom_patterns: %w", err)
	} else {
		for scope, raw := range rows {
			patterns := map[int]*SecretScanningCustomPattern{}
			if err := LoadJSON(raw, &patterns); err != nil {
				return fmt.Errorf("decode secret_scanning_custom_patterns row: %w", err)
			}
			st.SecretScanningCustomPatterns[scope] = patterns
			for id := range patterns {
				if id >= st.NextSecretScanningPatternID {
					st.NextSecretScanningPatternID = id + 1
				}
			}
		}
	}
	if rows, err := st.Persist.List("pr_creation_caps"); err != nil {
		return fmt.Errorf("load pr_creation_caps: %w", err)
	} else {
		for repoKey, raw := range rows {
			var cap PRCreationCap
			if err := LoadJSON(raw, &cap); err != nil {
				return fmt.Errorf("decode pr_creation_caps row: %w", err)
			}
			st.PRCreationCaps[repoKey] = &cap
		}
	}
	if rows, err := st.Persist.List("org_pr_creation_caps"); err != nil {
		return fmt.Errorf("load org_pr_creation_caps: %w", err)
	} else {
		for orgLogin, raw := range rows {
			var cap PRCreationCap
			if err := LoadJSON(raw, &cap); err != nil {
				return fmt.Errorf("decode org_pr_creation_caps row: %w", err)
			}
			st.OrgPRCreationCaps[orgLogin] = &cap
		}
	}
	if rows, err := st.Persist.List("pr_creation_bypass"); err != nil {
		return fmt.Errorf("load pr_creation_bypass: %w", err)
	} else {
		for repoKey, raw := range rows {
			users := map[string]bool{}
			if err := LoadJSON(raw, &users); err != nil {
				return fmt.Errorf("decode pr_creation_bypass row: %w", err)
			}
			st.PRCreationBypass[repoKey] = users
		}
	}
	if rows, err := st.Persist.List("issue_suggestions"); err != nil {
		return fmt.Errorf("load issue_suggestions: %w", err)
	} else {
		for key, raw := range rows {
			suggestions := map[int]*IssueSuggestion{}
			if err := LoadJSON(raw, &suggestions); err != nil {
				return fmt.Errorf("decode issue_suggestions row: %w", err)
			}
			st.IssueSuggestions[key] = suggestions
			for id := range suggestions {
				if id >= st.NextIssueSuggestionID {
					st.NextIssueSuggestionID = id + 1
				}
			}
		}
	}
	if rows, err := st.Persist.List("pull_request_stacks"); err != nil {
		return fmt.Errorf("load pull_request_stacks: %w", err)
	} else {
		for repoKey, raw := range rows {
			stacks := map[int]*PullRequestStack{}
			if err := LoadJSON(raw, &stacks); err != nil {
				return fmt.Errorf("decode pull_request_stacks row: %w", err)
			}
			st.PullRequestStacks[repoKey] = stacks
			for _, stack := range stacks {
				if stack.ID >= st.NextPullRequestStackID {
					st.NextPullRequestStackID = stack.ID + 1
				}
			}
		}
	}
	// actions-oidc-properties (keyed by org login, so List directly)
	if rows, err := st.Persist.List("org_oidc_property_inclusions"); err != nil {
		return fmt.Errorf("load org_oidc_property_inclusions: %w", err)
	} else {
		for org, raw := range rows {
			var names []string
			if err := LoadJSON(raw, &names); err != nil {
				return fmt.Errorf("decode org_oidc_property_inclusions row: %w", err)
			}
			st.OrgOIDCPropertyInclusions[org] = names
		}
	}
	if rows, err := st.Persist.List("org_blocks"); err != nil {
		return fmt.Errorf("load org_blocks: %w", err)
	} else {
		for orgLogin, raw := range rows {
			blocks := map[int]time.Time{}
			if err := LoadJSON(raw, &blocks); err != nil {
				return fmt.Errorf("decode org_blocks row: %w", err)
			}
			st.OrgBlocks[orgLogin] = blocks
		}
	}
	if rows, err := st.Persist.List("org_interaction_limits"); err != nil {
		return fmt.Errorf("load org_interaction_limits: %w", err)
	} else {
		for orgLogin, raw := range rows {
			var lim OrgInteractionLimit
			if err := LoadJSON(raw, &lim); err != nil {
				return fmt.Errorf("decode org_interaction_limits row: %w", err)
			}
			st.OrgInteractionLimits[orgLogin] = &lim
		}
	}
	if rows, err := st.Persist.List("org_announcements"); err != nil {
		return fmt.Errorf("load org_announcements: %w", err)
	} else {
		for orgLogin, raw := range rows {
			var announcement EnterpriseAnnouncement
			if err := LoadJSON(raw, &announcement); err != nil {
				return fmt.Errorf("decode org_announcements row: %w", err)
			}
			st.OrgAnnouncements[orgLogin] = &announcement
		}
	}
	if rows, err := st.Persist.List("org_scim_users"); err != nil {
		return fmt.Errorf("load org_scim_users: %w", err)
	} else {
		for orgLogin, raw := range rows {
			users := map[string]*EnterpriseSCIMUser{}
			if err := LoadJSON(raw, &users); err != nil {
				return fmt.Errorf("decode org_scim_users row: %w", err)
			}
			st.OrgSCIMUsers[orgLogin] = users
		}
	}
	if rows, err := st.Persist.List("org_external_groups"); err != nil {
		return fmt.Errorf("load org_external_groups: %w", err)
	} else {
		for orgLogin, raw := range rows {
			groups := map[string]*OrgExternalIdentityGroup{}
			if err := LoadJSON(raw, &groups); err != nil {
				return fmt.Errorf("decode org_external_groups row: %w", err)
			}
			st.OrgExternalGroups[orgLogin] = groups
			for _, group := range groups {
				if group.NumericID >= st.NextOrgExternalGroupID {
					st.NextOrgExternalGroupID = group.NumericID + 1
				}
			}
		}
	}
	if rows, err := st.Persist.List("team_external_group_ids"); err != nil {
		return fmt.Errorf("load team_external_group_ids: %w", err)
	} else {
		for teamIDRaw, raw := range rows {
			teamID, err := strconv.Atoi(teamIDRaw)
			if err != nil {
				return fmt.Errorf("decode team_external_group_ids key %q: %w", teamIDRaw, err)
			}
			var groupIDs []string
			if err := LoadJSON(raw, &groupIDs); err != nil {
				return fmt.Errorf("decode team_external_group_ids row: %w", err)
			}
			st.TeamExternalGroupIDs[teamID] = groupIDs
		}
	}
	if rows, err := st.Persist.List("security_review_requests"); err != nil {
		return fmt.Errorf("load security_review_requests: %w", err)
	} else {
		for scope, raw := range rows {
			requests := map[int]*SecurityReviewRequest{}
			if err := LoadJSON(raw, &requests); err != nil {
				return fmt.Errorf("decode security_review_requests row: %w", err)
			}
			st.SecurityReviewRequests[scope] = requests
			for _, request := range requests {
				if request.ID >= st.NextSecurityReviewRequestID {
					st.NextSecurityReviewRequestID = request.ID + 1
				}
				for _, response := range request.Responses {
					if response.ID >= st.NextSecurityReviewResponseID {
						st.NextSecurityReviewResponseID = response.ID + 1
					}
				}
			}
		}
	}
	for bucket, dst := range map[string]map[string]map[int]json.RawMessage{
		"org_custom_repo_roles": {},
		"org_custom_roles":      {},
	} {
		rows, err := st.Persist.List(bucket)
		if err != nil {
			return fmt.Errorf("load %s: %w", bucket, err)
		}
		for orgLogin, raw := range rows {
			var records map[int]json.RawMessage
			if err := LoadJSON(raw, &records); err != nil {
				return fmt.Errorf("decode %s row: %w", bucket, err)
			}
			dst[orgLogin] = records
			for id, record := range records {
				switch bucket {
				case "org_custom_repo_roles":
					var role OrgCustomRepositoryRole
					if err := LoadJSON(record, &role); err != nil {
						return fmt.Errorf("decode %s role: %w", bucket, err)
					}
					if st.OrgCustomRepoRoles[orgLogin] == nil {
						st.OrgCustomRepoRoles[orgLogin] = map[int]*OrgCustomRepositoryRole{}
					}
					st.OrgCustomRepoRoles[orgLogin][id] = &role
				case "org_custom_roles":
					var role OrgCustomOrganizationRole
					if err := LoadJSON(record, &role); err != nil {
						return fmt.Errorf("decode %s role: %w", bucket, err)
					}
					if st.OrgCustomRoles[orgLogin] == nil {
						st.OrgCustomRoles[orgLogin] = map[int]*OrgCustomOrganizationRole{}
					}
					st.OrgCustomRoles[orgLogin][id] = &role
				}
				if id >= st.NextOrgCustomRoleID {
					st.NextOrgCustomRoleID = id + 1
				}
			}
		}
	}
	for bucket, dst := range map[string]map[string]map[int][]int{
		"org_role_team_assignments": st.OrgRoleTeamAssignments,
		"org_role_user_assignments": st.OrgRoleUserAssignments,
	} {
		rows, err := st.Persist.List(bucket)
		if err != nil {
			return fmt.Errorf("load %s: %w", bucket, err)
		}
		for orgLogin, raw := range rows {
			assignments := map[int][]int{}
			if err := LoadJSON(raw, &assignments); err != nil {
				return fmt.Errorf("decode %s row: %w", bucket, err)
			}
			dst[orgLogin] = assignments
		}
	}

	// repo-write surfaces
	for _, loadFn := range []struct {
		Name string `json:"-"`
		fn   func(string, []byte) error
	}{
		{"pages_deployments", func(key string, raw []byte) error {
			repoID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("pages_deployments key %q: %w", key, err)
			}
			var byID map[int]*PagesDeploymentRecord
			if err := LoadJSON(raw, &byID); err != nil {
				return err
			}
			st.PagesDeployments[repoID] = byID
			for id := range byID {
				if id >= st.NextPagesDeploymentID {
					st.NextPagesDeploymentID = id + 1
				}
			}
			return nil
		}},
		{"env_branch_policies", func(key string, raw []byte) error {
			envID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("env_branch_policies key %q: %w", key, err)
			}
			var policies []*DeploymentBranchPolicyRule
			if err := LoadJSON(raw, &policies); err != nil {
				return err
			}
			st.EnvBranchPolicies[envID] = policies
			for _, p := range policies {
				if p.ID >= st.NextEnvBranchPolicyID {
					st.NextEnvBranchPolicyID = p.ID + 1
				}
			}
			return nil
		}},
		{"env_protection_rules", func(key string, raw []byte) error {
			envID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("env_protection_rules key %q: %w", key, err)
			}
			var rules []*EnvCustomProtectionRule
			if err := LoadJSON(raw, &rules); err != nil {
				return err
			}
			st.EnvProtectionRules[envID] = rules
			for _, rule := range rules {
				if rule.ID >= st.NextEnvProtectionRuleID {
					st.NextEnvProtectionRuleID = rule.ID + 1
				}
			}
			return nil
		}},
		{"sub_issues", func(key string, raw []byte) error {
			parentID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("sub_issues key %q: %w", key, err)
			}
			var children []int
			if err := LoadJSON(raw, &children); err != nil {
				return err
			}
			st.SubIssueLists[parentID] = children
			for _, childID := range children {
				st.SubIssueParent[childID] = parentID
			}
			return nil
		}},
		{"issue_blocked_by", func(key string, raw []byte) error {
			issueID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("issue_blocked_by key %q: %w", key, err)
			}
			var blockers []int
			if err := LoadJSON(raw, &blockers); err != nil {
				return err
			}
			st.IssueBlockedBy[issueID] = blockers
			return nil
		}},
		{"repo_imports", func(key string, raw []byte) error {
			repoID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("repo_imports key %q: %w", key, err)
			}
			var imp RepoImport
			if err := LoadJSON(raw, &imp); err != nil {
				return err
			}
			st.RepoImports[repoID] = &imp
			return nil
		}},
		{"dependency_snapshots", func(key string, raw []byte) error {
			repoID, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("dependency_snapshots key %q: %w", key, err)
			}
			var snapshots []*DependencySnapshot
			if err := LoadJSON(raw, &snapshots); err != nil {
				return err
			}
			st.DependencySnapshots[repoID] = snapshots
			for _, snap := range snapshots {
				if snap.ID >= st.NextDependencySnapshotID {
					st.NextDependencySnapshotID = snap.ID + 1
				}
			}
			return nil
		}},
		{"sbom_exports", func(key string, raw []byte) error {
			var exp SBOMExport
			if err := LoadJSON(raw, &exp); err != nil {
				return err
			}
			st.SBOMExports[key] = &exp
			return nil
		}},
	} {
		rows, err := st.Persist.List(loadFn.Name)
		if err != nil {
			return fmt.Errorf("load %s: %w", loadFn.Name, err)
		}
		for k, raw := range rows {
			if err := loadFn.fn(k, raw); err != nil {
				return fmt.Errorf("decode %s row: %w", loadFn.Name, err)
			}
		}
	}

	// GitHub Classroom
	if err := st.loadBucket("classrooms", func(raw []byte) error {
		var c Classroom
		if err := LoadJSON(raw, &c); err != nil {
			return err
		}
		st.Classrooms[c.ID] = &c
		if c.ID >= st.NextClassroomID {
			st.NextClassroomID = c.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("classroom_assignments", func(raw []byte) error {
		var a ClassroomAssignment
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.ClassroomAssignments[a.ID] = &a
		if a.ID >= st.NextClassroomAssignmentID {
			st.NextClassroomAssignmentID = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("classroom_accepted_assignments", func(raw []byte) error {
		var a ClassroomAcceptedAssignment
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.ClassroomAcceptedAssignments[a.ID] = &a
		if a.ID >= st.NextClassroomAcceptedID {
			st.NextClassroomAcceptedID = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	// repo-reads
	if err := st.loadBucket("repo_activity", func(raw []byte) error {
		var a RepoActivity
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		st.RepoActivities[a.ID] = &a
		if a.ID >= st.NextRepoActivity {
			st.NextRepoActivity = a.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("repo_traffic_clones", func(raw []byte) error {
		var b RepoTrafficBucket
		if err := LoadJSON(raw, &b); err != nil {
			return err
		}
		st.RepoCloneTraffic[repoTrafficKey(b.RepoID, b.Day)] = &b
		return nil
	}); err != nil {
		return err
	}

	// codespaces
	var interruptedCodespaces []*Codespace
	if err := st.loadBucket("codespaces", func(raw []byte) error {
		var cs Codespace
		if err := LoadJSON(raw, &cs); err != nil {
			return err
		}
		// A codespace persisted as "Provisioning" is an orphan of an interrupted
		// creation: reserveCodespace commits the durable record before the
		// container is started, so a crash in that window leaves the record in
		// "Provisioning" with no container — and no container survives a restart.
		// Reconcile it to "Shutdown", the resumable stopped state whose start
		// path re-provisions, so a crashed creation self-heals on boot instead of
		// stranding the codespace in a state it can never leave (STORE-041).
		if cs.State == "Provisioning" {
			cs.State = "Shutdown"
			cs.UpdatedAt = st.CurrentTime()
			interruptedCodespaces = append(interruptedCodespaces, &cs)
		}
		st.Codespaces[cs.ID] = &cs
		st.CodespacesByName[cs.Name] = &cs
		if cs.ID >= st.NextCodespaceID {
			st.NextCodespaceID = cs.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if len(interruptedCodespaces) > 0 {
		// One transaction: the reconciled states are durable before boot
		// completes, so the heal is not re-done (and not lost) on the next start.
		batch := NewPersistBatch(st.Persist)
		for _, cs := range interruptedCodespaces {
			batch.Put("codespaces", strconv.Itoa(cs.ID), cs)
		}
		if err := batch.Commit(); err != nil {
			return fmt.Errorf("reconcile interrupted codespace provisioning: %w", err)
		}
	}
	codespaceSecretRows, err := st.Persist.List("codespace_secrets")
	if err != nil {
		return fmt.Errorf("load codespace_secrets: %w", err)
	}
	for scope, raw := range codespaceSecretRows {
		var m map[string]*CodespaceSecret
		if err := LoadJSON(raw, &m); err != nil {
			return fmt.Errorf("decode codespace_secrets row: %w", err)
		}
		st.CodespaceSecrets[scope] = m
	}

	if err := st.applyDurableIDCounters(); err != nil {
		return err
	}
	if err := st.finishInterruptedDeletions(); err != nil {
		return err
	}
	if err := st.finishInterruptedRenames(); err != nil {
		return err
	}
	revision, err := st.Persist.StateRevision()
	if err != nil {
		return fmt.Errorf("load persistence state revision: %w", err)
	}
	st.persistenceRevision = revision

	// The Workflows map was just repopulated from disk; the derived run-id and
	// concurrency-group indexes must be recomputed from it.
	st.rebuildWorkflowIndexesLocked()

	return nil
}

// PendingDeletionsBucket records that a cascading delete has started. The
// intent is committed before any bytes are destroyed and removed in the same
// transaction as the last metadata row, so a delete interrupted anywhere in
// between is finished on the next start instead of leaving a repository or
// organization half-removed.
const PendingDeletionsBucket = "pending_deletions"

type PendingDeletion struct {
	Kind                string                    `json:"kind"`
	Name                string                    `json:"name"`
	StartedAt           time.Time                 `json:"started_at"`
	ObjectKeys          []string                  `json:"object_keys,omitempty"`
	LocalFiles          []string                  `json:"local_files,omitempty"`
	CodespaceRuntimes   []pendingCodespaceRuntime `json:"codespace_runtimes,omitempty"`
	ReleaseAssetObjects []string                  `json:"release_asset_objects,omitempty"`
	ReleaseAssetFiles   []string                  `json:"release_asset_files,omitempty"`
	ActionsObjectKeys   []string                  `json:"actions_object_keys,omitempty"`
	ActionsDirectories  []string                  `json:"actions_directories,omitempty"`
}

// pendingCodespaceRuntime is the minimum immutable state needed to retry a
// runtime cleanup after the codespace metadata itself has been committed away.
type pendingCodespaceRuntime struct {
	ContainerID    string `json:"container_id,omitempty"`
	WorkspaceMount string `json:"workspace_mount,omitempty"`
}

func PendingRepoDeletionKey(fullName string) string { return "repo:" + fullName }

func PendingOrgDeletionKey(login string) string { return "org:" + login }

func PendingUserDeletionKey(login string) string { return "user:" + login }

// PendingRenamesBucket records a repository rename whose slow object-store
// prefix copy runs outside the store lock (STORE-013). The intent survives from
// just before the copy until the old prefix has been purged, so a crash at any
// point is recoverable: if the metadata already moved to `To`, the leftover
// `From` prefix is purged; if it did not, the partial `To` copy is purged.
const PendingRenamesBucket = "pending_renames"

type PendingRename struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
}

func PendingRepoRenameKey(to string) string { return "repo:" + to }

func (st *Store) listPendingDeletions() (map[string]PendingDeletion, error) {
	rows, err := st.Persist.List(PendingDeletionsBucket)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", PendingDeletionsBucket, err)
	}
	out := make(map[string]PendingDeletion, len(rows))
	for key, raw := range rows {
		var record PendingDeletion
		if err := LoadJSON(raw, &record); err != nil {
			return nil, fmt.Errorf("decode %s row %s: %w", PendingDeletionsBucket, key, err)
		}
		out[key] = record
	}
	return out, nil
}

// finishInterruptedDeletions completes every cascade that a previous process
// started but did not finish. It runs after loading, without the store lock,
// so it can reuse the same delete paths a request would take.
func (st *Store) finishInterruptedDeletions() error {
	pending, err := st.listPendingDeletions()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		record := pending[key]
		switch record.Kind {
		case "repo":
			owner, name, ok := SplitRepoFullName(record.Name)
			if !ok {
				return fmt.Errorf("resume repository delete %q: not an owner/name pair", record.Name)
			}
			existed, err := st.DeleteRepo(owner, name)
			if err != nil {
				return fmt.Errorf("resume repository delete %s: %w", record.Name, err)
			}
			if existed {
				continue
			}
			if err := st.PurgeDeletedRepoBytes(record.Name, record); err != nil {
				return fmt.Errorf("resume repository delete %s: %w", record.Name, err)
			}
		case "org":
			existed, err := st.DeleteOrgWithError(record.Name)
			if err != nil {
				return fmt.Errorf("resume organization delete %s: %w", record.Name, err)
			}
			if existed {
				continue
			}
			if err := st.Persist.Delete(PendingDeletionsBucket, key); err != nil {
				return fmt.Errorf("resume organization delete %s: clear intent: %w", record.Name, err)
			}
		default:
			return fmt.Errorf("%s row %s has unknown kind %q", PendingDeletionsBucket, key, record.Kind)
		}
	}
	return nil
}

// finishInterruptedRenames purges the object-store prefix a rename left behind
// after a crash (STORE-013). It runs at startup after loading, without the
// store lock. If the metadata already moved to the new name, the stale old
// prefix is purged; otherwise the partial new-prefix copy — which nothing
// references — is purged. Either way the intent is cleared.
func (st *Store) finishInterruptedRenames() error {
	rows, err := st.Persist.List(PendingRenamesBucket)
	if err != nil {
		return fmt.Errorf("load %s: %w", PendingRenamesBucket, err)
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var rec PendingRename
		if err := LoadJSON(rows[key], &rec); err != nil {
			return fmt.Errorf("decode %s row %s: %w", PendingRenamesBucket, key, err)
		}
		st.Mu.Lock()
		_, toLive := st.ReposByName[rec.To]
		st.Mu.Unlock()
		stale := rec.To // partial copy the rename never published
		if toLive {
			stale = rec.From // rename published; the old prefix is the leftover
		}
		if err := st.deleteRepoPrefixBytes(stale); err != nil {
			return fmt.Errorf("resume repository rename %s -> %s: purge %s: %w", rec.From, rec.To, stale, err)
		}
		if err := st.Persist.Delete(PendingRenamesBucket, key); err != nil {
			return fmt.Errorf("resume repository rename %s -> %s: clear intent: %w", rec.From, rec.To, err)
		}
	}
	return nil
}

// idCounterBuckets maps each record bucket whose keys are allocated
// identifiers to the counter that hands out the next one. Rebuilding a counter
// from the surviving rows alone re-issues a deleted entity's identifier, and
// for attestations, package files and artifact records that identifier is also
// the object-store key — the new entity would inherit the deleted one's bytes.
func (st *Store) idCounterBuckets() map[string]*int {
	return map[string]*int{
		"apps":                             &st.NextAppID,
		"artifact_deployment_records":      &st.NextArtifactDeploymentRecordID,
		"artifact_storage_records":         &st.NextArtifactStorageRecordID,
		"attestations":                     &st.NextAttestationID,
		"classroom_accepted_assignments":   &st.NextClassroomAcceptedID,
		"classroom_assignments":            &st.NextClassroomAssignmentID,
		"classrooms":                       &st.NextClassroomID,
		"code_scanning_alerts":             &st.NextCodeScanningAlertID,
		"code_scanning_analyses":           &st.NextCodeScanningAnalysisID,
		"codeql_databases":                 &st.NextCodeQLDatabaseID,
		"codeql_variant_analyses":          &st.NextCodeQLVariantAnalysisID,
		"codespaces":                       &st.NextCodespaceID,
		"comments":                         &st.NextComment,
		"dependabot_alerts":                &st.NextDependabotAlertID,
		"discussion_categories":            &st.NextDiscussionCategoryID,
		"discussion_comments":              &st.NextDiscussionCommentID,
		"discussions":                      &st.NextDiscussionID,
		"enterprise_code_security_configs": &st.NextEnterpriseCodeSecurityConfigID,
		"enterprise_teams":                 &st.NextEnterpriseTeamID,
		"gist_comments":                    &st.NextGistCommentID,
		"hosted_runner_custom_images":      &st.NextHostedRunnerImageID,
		"hosted_runners":                   &st.NextHostedRunnerID,
		"issue_events":                     &st.NextIssueEventID,
		"issues":                           &st.NextIssue,
		"labels":                           &st.NextLabel,
		"milestones":                       &st.NextMilestone,
		"org_invitations":                  &st.NextOrgInvitationID,
		"org_migrations":                   &st.NextOrgMigrationID,
		"orgs":                             &st.NextOrg,
		"package_files":                    &st.NextPackageFileID,
		"package_versions":                 &st.NextPackageVersionID,
		"packages":                         &st.NextPackageID,
		"pr_reviews":                       &st.NextPRReview,
		"project_cards":                    &st.NextProjectCardID,
		"project_columns":                  &st.NextProjectColumnID,
		"projects_classic":                 &st.NextProjectClassicID,
		"pull_requests":                    &st.NextPR,
		"repo_activity":                    &st.NextRepoActivity,
		"repo_rulesets":                    &st.NextRulesetID,
		"ruleset_suites":                   &st.NextRulesetSuiteID,
		"repos":                            &st.NextRepo,
		"runner_groups":                    &st.NextRunnerGroupID,
		"secret_scanning_alerts":           &st.NextSecretScanningAlertID,
		"security_advisories":              &st.NextSecurityAdvisoryID,
		"security_advisory_reports":        &st.NextSecurityAdvisoryReportID,
		"teams":                            &st.NextTeam,
		"user_migrations":                  &st.NextUserMigrationID,
		"users":                            &st.NextUser,
		"workflow_attempts":                &st.NextRunID,
	}
}

// idCounterBuckets64 is idCounterBuckets for the counters stored as int64.
func (st *Store) idCounterBuckets64() map[string]*int64 {
	return map[string]*int64{
		"api_insights_requests": &st.NextAPIRequestID,
		"check_runs":            &st.NextCheckRunID,
		"check_suites":          &st.NextCheckSuiteID,
		"copilot_spaces":        &st.NextCopilotSpaceID,
	}
}

func (st *Store) applyDurableIDCounters() error {
	for bucket, next := range st.idCounterBuckets() {
		stored, err := st.Persist.KeyHighWater(bucket)
		if err != nil {
			return fmt.Errorf("load identifier counter for %s: %w", bucket, err)
		}
		if int(stored) > *next {
			*next = int(stored)
		}
	}
	for bucket, next := range st.idCounterBuckets64() {
		stored, err := st.Persist.KeyHighWater(bucket)
		if err != nil {
			return fmt.Errorf("load identifier counter for %s: %w", bucket, err)
		}
		if stored > *next {
			*next = stored
		}
	}
	return nil
}

func (st *Store) loadBucket(name string, fn func(raw []byte) error) error {
	rows, err := st.Persist.List(name)
	if err != nil {
		return fmt.Errorf("load %s: %w", name, err)
	}
	for _, raw := range rows {
		if err := fn(raw); err != nil {
			return fmt.Errorf("decode %s row: %w", name, err)
		}
	}
	return nil
}

// forgetExternalIdentitiesLocked removes every (issuer, subject) binding for
// user from the federated-identity index. Callers hold st.Mu. It mirrors the
// UsersByLogin cleanup at each site that drops a User, so the index cannot
// resurrect a deleted account when its provider logs in again.
func (st *Store) ForgetExternalIdentitiesLocked(user *User) {
	if user == nil {
		return
	}
	for _, identity := range user.ExternalIdentities {
		key := ExternalIdentityKey(identity.Issuer, identity.Subject)
		if key != "" && st.UsersByExternalID[key] == user {
			delete(st.UsersByExternalID, key)
		}
	}
}

// SeedDefaultUser creates the default admin user and token.
func (st *Store) SeedDefaultUser() {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := st.CurrentTime()
	u := &User{
		ID:           st.ReserveGlobalID("next_user", &st.NextUser),
		NodeID:       "U_kgDOBdefault",
		Login:        "admin",
		Name:         "Admin",
		Email:        "admin@bleephub.local",
		AvatarURL:    "",
		Bio:          "",
		Type:         "User",
		SiteAdmin:    true,
		StarredRepos: map[string]bool{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.Users[u.ID] = u
	st.UsersByLogin[u.Login] = u

	t := &Token{
		Value:     AdminToken(),
		UserID:    u.ID,
		Scopes:    "repo, workflow, read:org, admin:org, admin:org_hook, gist",
		CreatedAt: now,
	}
	st.Tokens[st.tokenMapKey(t.Value)] = t
	// One transaction: the admin user and its token commit together, so a
	// crash cannot seed an admin nobody can authenticate as (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("users", strconv.Itoa(u.ID), u)
	batch.Put("tokens", t.Value, t)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "users", Key: strconv.Itoa(u.ID), Err: err})
	}
}

// LookupToken returns the token and associated user, or nil if not found.
func (st *Store) LookupToken(tokenStr string) (*Token, *User) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	t, _ := st.tokenByValueLocked(tokenStr)
	if t == nil {
		return nil, nil
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(st.CurrentTime()) {
		return nil, nil
	}
	return t, st.Users[t.UserID]
}

// LookupUserByLogin returns the user with the given login, or nil.
func (st *Store) LookupUserByLogin(login string) *User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.UsersByLogin[login]
}

// GetUserByID returns the user with the given ID, or nil.
func (st *Store) GetUserByID(id int) *User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.Users[id]
}

// LookupUserBySSHKey resolves a registered account SSH authentication key.
// It compares parsed SSH wire encodings so comments and spacing differences in
// authorized-key text cannot create a different credential identity. The
// parsed form is cached when the key is registered or loaded; keys whose text
// never parsed (logged then) carry no cached form and cannot match.
func (st *Store) LookupUserBySSHKey(key ssh.PublicKey) *User {
	wire := key.Marshal()
	// Resolve the matching user id under Misc.Mu, then release it before
	// taking st.Mu via GetUserByID. Calling GetUserByID while holding
	// Misc.Mu would invert the Store→Misc lock order the reentrancy gate
	// depends on.
	userID := 0
	found := false
	st.Misc.Mu.RLock()
	for uid, keys := range st.Misc.KeysByUser {
		for _, registered := range keys {
			if registered.parsed != nil && bytes.Equal(registered.parsed.Marshal(), wire) {
				userID = uid
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	st.Misc.Mu.RUnlock()
	if !found {
		return nil
	}
	return st.GetUserByID(userID)
}

// CountFollowers returns how many users follow the given login.
func (st *Store) CountFollowers(login string) int {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	n := 0
	for _, follows := range st.Misc.Follows {
		if follows[login] {
			n++
		}
	}
	return n
}

// CountFollowing returns how many users the given login follows.
func (st *Store) CountFollowing(login string) int {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return len(st.Misc.Follows[login])
}

// CountPublicRepos returns the number of non-private repositories owned
// by the given account login (user or organization).
func (st *Store) CountPublicRepos(login string) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := login + "/"
	n := 0
	for name, r := range st.ReposByName {
		if strings.HasPrefix(name, prefix) && !r.Private {
			n++
		}
	}
	return n
}

// CountOpenIssues returns the number of open issues plus open pull
// requests in a repository — GitHub's open_issues_count counts both
// because PRs are issues internally.
func (st *Store) CountOpenIssues(repoID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	n := 0
	for _, issue := range st.IssuesByRepo[repoID] {
		if issue.State == "OPEN" {
			n++
		}
	}
	for _, pr := range st.PullsByRepo[repoID] {
		if pr.State == "OPEN" {
			n++
		}
	}
	return n
}

// CreateToken generates a new token for the given user.
func (st *Store) CreateToken(userID int, scopes string) *Token {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.CreateTokenLocked(userID, scopes)
}

// generateTokenValue creates a ghp_-prefixed random token string (classic PAT).
// Real GitHub uses ghp_ for classic PATs; bleephub matches the prefix so SDK
// clients that branch on prefix recognise the token shape.
func generateTokenValue() (string, error) {
	h, err := RandomHex(20)
	if err != nil {
		return "", fmt.Errorf("generate personal access token: %w", err)
	}
	return fmt.Sprintf("ghp_%s", h), nil
}

// generateGistID creates a random 20-character hexadecimal gist ID.
func generateGistID() (string, error) {
	h, err := RandomHex(10)
	if err != nil {
		return "", fmt.Errorf("generate gist id: %w", err)
	}
	return h, nil
}

func cloneGist(g *Gist) *Gist {
	if g == nil {
		return nil
	}
	clone := *g
	clone.Files = make(map[string]*GistFile, len(g.Files))
	for name, file := range g.Files {
		if file == nil {
			clone.Files[name] = nil
			continue
		}
		fileClone := *file
		clone.Files[name] = &fileClone
	}
	clone.History = make([]*GistHistory, 0, len(g.History))
	for _, history := range g.History {
		if history == nil {
			clone.History = append(clone.History, nil)
			continue
		}
		historyClone := *history
		historyClone.ChangeStatus = make(map[string]int, len(history.ChangeStatus))
		for key, value := range history.ChangeStatus {
			historyClone.ChangeStatus[key] = value
		}
		clone.History = append(clone.History, &historyClone)
	}
	clone.ForkIDs = append([]string(nil), g.ForkIDs...)
	return &clone
}

func cloneGistStars(stars map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(stars))
	for id, starred := range stars {
		clone[id] = starred
	}
	return clone
}

func (st *Store) commitGistBatchLocked(batch *PersistBatch) {
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "gists", Err: err})
	}
}

// CreateGist creates a new gist owned by the given user.

func (st *Store) CreateGistE(owner *User, description string, public bool, files map[string]*GistFile) (*Gist, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	id, err := generateGistID()
	if err != nil {
		return nil, err
	}
	for st.Gists[id] != nil {
		id, err = generateGistID()
		if err != nil {
			return nil, err
		}
	}
	now := st.CurrentTime()
	g := &Gist{
		ID:          id,
		NodeID:      fmt.Sprintf("G_kwDOB%06d", st.NextGistID),
		Description: description,
		Public:      public,
		OwnerID:     owner.ID,
		Files:       cloneGist(&Gist{Files: files}).Files,
		CreatedAt:   now,
		UpdatedAt:   now,
		Comments:    0,
		History: []*GistHistory{{
			Version:     id,
			CommittedAt: now,
			ChangeStatus: map[string]int{
				"total":     0,
				"additions": 0,
				"deletions": 0,
			},
		}},
	}
	batch := NewPersistBatch(st.Persist)
	batch.Put("gists", g.ID, g)
	if err := batch.Commit(); err != nil {
		return nil, err
	}
	st.Gists[id] = g
	st.NextGistID++
	return cloneGist(g), nil
}

// GetGist returns the gist with the given ID, or nil.
func (st *Store) GetGist(id string) *Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneGist(st.Gists[id])
}

// UpdateGist replaces the gist fields and records a history entry.

func (st *Store) UpdateGistE(id string, description *string, files map[string]*GistFile, deleteFiles []string) (*Gist, bool, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	current := st.Gists[id]
	if current == nil {
		return nil, false, nil
	}
	g := cloneGist(current)

	additions, deletions := 0, 0
	if description != nil {
		g.Description = *description
	}
	for name, f := range files {
		if _, existed := g.Files[name]; existed {
			deletions += len(g.Files[name].Content)
		} else {
			additions += len(f.Content)
		}
		if f == nil {
			g.Files[name] = nil
		} else {
			fileClone := *f
			g.Files[name] = &fileClone
		}
	}
	for _, name := range deleteFiles {
		if f, ok := g.Files[name]; ok {
			deletions += len(f.Content)
			delete(g.Files, name)
		}
	}
	g.UpdatedAt = st.CurrentTime()

	version, err := generateGistID()
	if err != nil {
		return nil, false, err
	}
	g.History = append(g.History, &GistHistory{
		Version:     version,
		CommittedAt: g.UpdatedAt,
		ChangeStatus: map[string]int{
			"total":     additions + deletions,
			"additions": additions,
			"deletions": deletions,
		},
	})
	batch := NewPersistBatch(st.Persist)
	batch.Put("gists", g.ID, g)
	if err := batch.Commit(); err != nil {
		return nil, false, err
	}
	st.Gists[id] = g
	return cloneGist(g), true, nil
}

// DeleteGist deletes a gist and all its comments.
func (st *Store) DeleteGist(id string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Gists[id] == nil {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	batch.Delete("gists", id)
	var commentIDs []int
	for cid, comment := range st.GistComments {
		if comment.GistID == id {
			commentIDs = append(commentIDs, cid)
			batch.Delete("gist_comments", strconv.Itoa(cid))
		}
	}
	updatedStars := make(map[int]map[string]bool)
	for userID, stars := range st.StarredGists {
		if !stars[id] {
			continue
		}
		clone := cloneGistStars(stars)
		delete(clone, id)
		updatedStars[userID] = clone
		key := strconv.Itoa(userID)
		if len(clone) == 0 {
			batch.Delete("starred_gists", key)
		} else {
			batch.Put("starred_gists", key, persistedStarredGists{UserID: userID, Stars: clone})
		}
	}
	st.commitGistBatchLocked(batch)

	delete(st.Gists, id)
	for _, commentID := range commentIDs {
		delete(st.GistComments, commentID)
	}
	for userID, stars := range updatedStars {
		if len(stars) == 0 {
			delete(st.StarredGists, userID)
		} else {
			st.StarredGists[userID] = stars
		}
	}
	return true
}

// ListGistsForUser returns gists owned by the user, optionally filtered by since.
func (st *Store) ListGistsForUser(userID int, since time.Time) []*Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*Gist
	for _, g := range st.Gists {
		if g.OwnerID == userID && !g.UpdatedAt.Before(since) {
			out = append(out, cloneGist(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// ListGistCommits returns the revision history for a gist.
func (st *Store) ListGistCommits(gistID string) []*GistHistory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	g := st.Gists[gistID]
	if g == nil {
		return nil
	}
	out := make([]*GistHistory, len(g.History))
	copy(out, g.History)
	SortHistory(out)
	return snapshotGistHistory(out)
}

// GetGistAtRevision returns the gist state at a specific revision.
func (st *Store) GetGistAtRevision(gistID, sha string) *Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	g := st.Gists[gistID]
	if g == nil {
		return nil
	}
	for _, h := range g.History {
		if h.Version == sha {
			cp := *g
			cp.UpdatedAt = h.CommittedAt
			return &cp
		}
	}
	return nil
}

// ListPublicGists returns all public gists, newest first.
func (st *Store) ListPublicGists(since time.Time) []*Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*Gist
	for _, g := range st.Gists {
		if g.Public && !g.UpdatedAt.Before(since) {
			out = append(out, cloneGist(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// ListStarredGists returns gists starred by the user.
func (st *Store) ListStarredGists(userID int) []*Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	stars, ok := st.StarredGists[userID]
	if !ok {
		return nil
	}
	var out []*Gist
	for id := range stars {
		if g := st.Gists[id]; g != nil {
			out = append(out, cloneGist(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// StarGist stars a gist for the user.
func (st *Store) StarGist(userID int, gistID string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Gists[gistID] == nil {
		return false
	}
	stars := cloneGistStars(st.StarredGists[userID])
	stars[gistID] = true
	batch := NewPersistBatch(st.Persist)
	batch.Put("starred_gists", strconv.Itoa(userID), persistedStarredGists{UserID: userID, Stars: stars})
	st.commitGistBatchLocked(batch)
	st.StarredGists[userID] = stars
	return true
}

// UnstarGist unstars a gist for the user.
func (st *Store) UnstarGist(userID int, gistID string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Gists[gistID] == nil {
		return false
	}
	stars := cloneGistStars(st.StarredGists[userID])
	delete(stars, gistID)
	batch := NewPersistBatch(st.Persist)
	if len(stars) == 0 {
		batch.Delete("starred_gists", strconv.Itoa(userID))
	} else {
		batch.Put("starred_gists", strconv.Itoa(userID), persistedStarredGists{UserID: userID, Stars: stars})
	}
	st.commitGistBatchLocked(batch)
	if len(stars) == 0 {
		delete(st.StarredGists, userID)
	} else {
		st.StarredGists[userID] = stars
	}
	return true
}

// IsGistStarred reports whether the user has starred the gist.
func (st *Store) IsGistStarred(userID int, gistID string) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if st.Gists[gistID] == nil {
		return false
	}
	return st.StarredGists[userID][gistID]
}

// ForkGist forks a gist for the given user.

func (st *Store) ForkGistE(user *User, gistID string) (*Gist, bool, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	current := st.Gists[gistID]
	if current == nil {
		return nil, false, nil
	}
	orig := cloneGist(current)
	files := make(map[string]*GistFile, len(orig.Files))
	for name, f := range orig.Files {
		cp := *f
		files[name] = &cp
	}
	now := st.CurrentTime()
	id, err := generateGistID()
	if err != nil {
		return nil, false, err
	}
	for st.Gists[id] != nil {
		id, err = generateGistID()
		if err != nil {
			return nil, false, err
		}
	}
	fork := &Gist{
		ID:          id,
		NodeID:      fmt.Sprintf("G_kwDOB%06d", st.NextGistID),
		Description: orig.Description,
		Public:      orig.Public,
		OwnerID:     user.ID,
		Files:       files,
		CreatedAt:   now,
		UpdatedAt:   now,
		Comments:    0,
		ForkOfID:    orig.ID,
	}
	orig.ForkIDs = append(orig.ForkIDs, id)
	batch := NewPersistBatch(st.Persist)
	batch.Put("gists", orig.ID, orig)
	batch.Put("gists", fork.ID, fork)
	if err := batch.Commit(); err != nil {
		return nil, false, err
	}
	st.Gists[orig.ID] = orig
	st.Gists[id] = fork
	st.NextGistID++
	return cloneGist(fork), true, nil
}

// ListGistForks returns forks of a gist.
func (st *Store) ListGistForks(gistID string) []*Gist {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	orig := st.Gists[gistID]
	if orig == nil {
		return nil
	}
	var out []*Gist
	for _, fid := range orig.ForkIDs {
		if f := st.Gists[fid]; f != nil {
			out = append(out, cloneGist(f))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// CreateGistComment adds a comment to a gist.
func (st *Store) CreateGistComment(gistID string, user *User, body string) *GistComment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	g := st.Gists[gistID]
	if g == nil {
		return nil
	}
	now := st.CurrentTime()
	c := &GistComment{
		ID:                st.NextGistCommentID,
		NodeID:            fmt.Sprintf("GC_kwDOB%06d", st.NextGistCommentID),
		GistID:            gistID,
		UserID:            user.ID,
		Body:              body,
		CreatedAt:         now,
		UpdatedAt:         now,
		AuthorAssociation: "OWNER",
	}
	updatedGist := cloneGist(g)
	updatedGist.Comments++
	batch := NewPersistBatch(st.Persist)
	batch.Put("gist_comments", strconv.Itoa(c.ID), c)
	batch.Put("gists", updatedGist.ID, updatedGist)
	st.commitGistBatchLocked(batch)
	st.GistComments[c.ID] = c
	st.Gists[gistID] = updatedGist
	st.NextGistCommentID++
	commentClone := *c
	return &commentClone
}

// GetGistComment returns a comment by ID.
func (st *Store) GetGistComment(id int) *GistComment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	comment := st.GistComments[id]
	if comment == nil {
		return nil
	}
	clone := *comment
	return &clone
}

// UpdateGistComment updates a comment body.
func (st *Store) UpdateGistComment(id int, body string) (*GistComment, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	current := st.GistComments[id]
	if current == nil {
		return nil, false
	}
	c := *current
	c.Body = body
	c.UpdatedAt = st.CurrentTime()
	batch := NewPersistBatch(st.Persist)
	batch.Put("gist_comments", strconv.Itoa(c.ID), &c)
	st.commitGistBatchLocked(batch)
	st.GistComments[id] = &c
	result := c
	return &result, true
}

// DeleteGistComment deletes a comment and decrements the gist comment count.
func (st *Store) DeleteGistComment(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.GistComments[id]
	if c == nil {
		return false
	}
	batch := NewPersistBatch(st.Persist)
	var updatedGist *Gist
	if gist := st.Gists[c.GistID]; gist != nil {
		updatedGist = cloneGist(gist)
		updatedGist.Comments--
		batch.Put("gists", updatedGist.ID, updatedGist)
	}
	batch.Delete("gist_comments", strconv.Itoa(id))
	st.commitGistBatchLocked(batch)
	if updatedGist != nil {
		st.Gists[updatedGist.ID] = updatedGist
	}
	delete(st.GistComments, id)
	return true
}

// ListGistComments returns comments for a gist, oldest first.
func (st *Store) ListGistComments(gistID string) []*GistComment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*GistComment
	for _, c := range st.GistComments {
		if c.GistID == gistID {
			clone := *c
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return snapshotSlice(out)
}

// ListUsers returns all users.
func (st *Store) ListUsers() []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*User, 0, len(st.Users))
	for _, u := range st.Users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotUsers(out)
}

// ListBlockedUsers returns the logins of users blocked by userID.
func (st *Store) ListBlockedUsers(userID int) []string {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	blocked := st.Misc.blockedUsers[userID]
	out := make([]string, 0, len(blocked))
	for id := range blocked {
		if u := st.Users[id]; u != nil {
			out = append(out, u.Login)
		}
	}
	sort.Strings(out)
	return out
}

// IsUserBlocked reports whether userID has blocked targetID.
func (st *Store) IsUserBlocked(userID, targetID int) bool {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return st.Misc.blockedUsers[userID][targetID]
}

// BlockUser blocks targetID for userID.
func (st *Store) BlockUser(userID, targetID int) bool {
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if st.Misc.blockedUsers[userID] == nil {
		st.Misc.blockedUsers[userID] = map[int]bool{}
	}
	st.Misc.blockedUsers[userID][targetID] = true
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "blocked_users", st.Misc.blockedUsers)
	}
	return true
}

// UnblockUser unblocks targetID for userID.
func (st *Store) UnblockUser(userID, targetID int) bool {
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if st.Misc.blockedUsers[userID] == nil {
		return false
	}
	if !st.Misc.blockedUsers[userID][targetID] {
		return false
	}
	delete(st.Misc.blockedUsers[userID], targetID)
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "blocked_users", st.Misc.blockedUsers)
	}
	return true
}

// ListUserBlocks returns users blocked by userID.
func (st *Store) ListUserBlocks(userID int) []*User {
	// Snapshot the blocked ids under Misc.Mu, then resolve users via the
	// st.Mu-guarded accessor after releasing it. st.Users is guarded by
	// st.Mu everywhere else, so reading it under Misc.Mu alone races
	// CreateUser/upsertExternalUser.
	st.Misc.Mu.RLock()
	targetIDs := make([]int, 0, len(st.Misc.blockedUsers[userID]))
	for targetID := range st.Misc.blockedUsers[userID] {
		targetIDs = append(targetIDs, targetID)
	}
	st.Misc.Mu.RUnlock()
	var out []*User
	for _, targetID := range targetIDs {
		if u := st.GetUserByID(targetID); u != nil {
			out = append(out, u)
		}
	}
	return snapshotUsers(out)
}

// IsUserFollowing reports whether userID follows targetID.
func (st *Store) IsUserFollowing(userID, targetID int) bool {
	// Resolve users via the st.Mu-guarded accessor first (Store→Misc order),
	// then read the follow graph under Misc.Mu. Dereferencing st.Users under
	// Misc.Mu alone races concurrent user writes.
	user := st.GetUserByID(userID)
	target := st.GetUserByID(targetID)
	if user == nil || target == nil {
		return false
	}
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return st.Misc.Follows[user.Login][target.Login]
}

// ListUserSocialAccounts returns social accounts for a user.
func (st *Store) ListUserSocialAccounts(userID int) []map[string]interface{} {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	out := make([]map[string]interface{}, len(st.Misc.socialAccounts[userID]))
	copy(out, st.Misc.socialAccounts[userID])
	return out
}

// SetUserSocialAccounts replaces a user's social accounts.
func (st *Store) SetUserSocialAccounts(userID int, accounts []string) bool {
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	var out []map[string]interface{}
	for _, a := range accounts {
		if a == "" {
			continue
		}
		out = append(out, map[string]interface{}{
			"provider": "generic",
			"url":      a,
		})
	}
	st.Misc.socialAccounts[userID] = out
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "social_accounts", st.Misc.socialAccounts)
	}
	return true
}

// ListUserSSHSigningKeys returns SSH signing keys for a user.
func (st *Store) ListUserSSHSigningKeys(userID int) []map[string]interface{} {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	out := make([]map[string]interface{}, len(st.Misc.sshSigningKeys[userID]))
	copy(out, st.Misc.sshSigningKeys[userID])
	return out
}

// AddUserSSHSigningKey adds an SSH signing key for a user.
func (st *Store) AddUserSSHSigningKey(userID int, key string) map[string]interface{} {
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	id := st.Misc.nextSSHSigningKeyID
	st.Misc.nextSSHSigningKeyID++
	entry := map[string]interface{}{
		"id":         id,
		"key":        key,
		"title":      "",
		"created_at": st.CurrentTime().Format(time.RFC3339),
	}
	st.Misc.sshSigningKeys[userID] = append(st.Misc.sshSigningKeys[userID], entry)
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("misc", "ssh_signing_keys", st.Misc.sshSigningKeys)
	}
	return entry
}

// SshSigningKeyEntryID extracts the numeric key ID from an SSH signing key
// entry. Freshly created entries store an int; entries reloaded from
// persistence decode JSON numbers as float64, so both shapes must resolve.
func SshSigningKeyEntryID(entry map[string]interface{}) int {
	switch v := entry["id"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// DeleteUserSSHSigningKey deletes an SSH signing key for a user.
func (st *Store) DeleteUserSSHSigningKey(userID, keyID int) bool {
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	keys := st.Misc.sshSigningKeys[userID]
	for i, k := range keys {
		if SshSigningKeyEntryID(k) == keyID {
			st.Misc.sshSigningKeys[userID] = append(keys[:i], keys[i+1:]...)
			if st.Misc.Persist != nil {
				st.Misc.Persist.MustPut("misc", "ssh_signing_keys", st.Misc.sshSigningKeys)
			}
			return true
		}
	}
	return false
}
