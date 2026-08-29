package store

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SecretScanningLocation describes where a secret was detected.
type SecretScanningLocation struct {
	Type    string                        `json:"type"`
	Details SecretScanningLocationDetails `json:"details"`
}

// SecretScanningLocationDetails holds the commit-level details for a location.
type SecretScanningLocationDetails struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	StartColumn int    `json:"start_column"`
	EndColumn   int    `json:"end_column"`
	BlobSHA     string `json:"blob_sha"`
	BlobURL     string `json:"blob_url"`
	CommitSHA   string `json:"commit_sha"`
	CommitURL   string `json:"commit_url"`
	HTMLURL     string `json:"html_url"`
}

// SecretScanningState is the lifecycle state of a secret-scanning alert;
// GitHub only ever emits these two.
type SecretScanningState string

const (
	SecretScanningStateOpen     SecretScanningState = "open"
	SecretScanningStateResolved SecretScanningState = "resolved"
)

// SecretScanningResolution is the reason recorded when an alert is resolved;
// only these six values are accepted.
type SecretScanningResolution string

const (
	SecretScanningResolutionFalsePositive  SecretScanningResolution = "false_positive"
	SecretScanningResolutionWontFix        SecretScanningResolution = "wont_fix"
	SecretScanningResolutionRevoked        SecretScanningResolution = "revoked"
	SecretScanningResolutionUsedInTests    SecretScanningResolution = "used_in_tests"
	SecretScanningResolutionPatternDeleted SecretScanningResolution = "pattern_deleted"
	SecretScanningResolutionPatternEdited  SecretScanningResolution = "pattern_edited"
)

// SecretScanningAlert is a repo-scoped secret scanning alert.
type SecretScanningAlert struct {
	ID                    int                      `json:"id"`
	NodeID                string                   `json:"node_id"`
	Number                int                      `json:"number"`
	RepoKey               string                   `json:"repo_key"`
	SecretType            string                   `json:"secret_type"`
	SecretTypeDisplayName string                   `json:"secret_type_display_name"`
	State                 SecretScanningState      `json:"state"`
	Resolution            SecretScanningResolution `json:"resolution"`
	ResolutionComment     string                   `json:"resolution_comment"`
	Locations             []SecretScanningLocation `json:"locations"`
	HTMLURL               string                   `json:"html_url"`
	URL                   string                   `json:"url"`
	LocationsURL          string                   `json:"locations_url"`
	CreatedAt             time.Time                `json:"created_at"`
	UpdatedAt             time.Time                `json:"updated_at"`
	ResolvedAt            *time.Time               `json:"resolved_at"`
}

// CreateSecretScanningAlert seeds a new alert. The real API has no create
// endpoint; this is bleephub's internal seeding path.
func (st *Store) CreateSecretScanningAlert(repoKey, secretType string, locations []SecretScanningLocation) *SecretScanningAlert {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	return st.CreateSecretScanningAlertLocked(repoKey, secretType, locations)
}

func (st *Store) CreateSecretScanningAlertLocked(repoKey, secretType string, locations []SecretScanningLocation) *SecretScanningAlert {
	if st.SecretScanningAlertsByRepo[repoKey] == nil {
		st.SecretScanningAlertsByRepo[repoKey] = make(map[int]*SecretScanningAlert)
	}
	if st.SecretScanningNextNumber[repoKey] == 0 {
		st.SecretScanningNextNumber[repoKey] = 1
	}

	now := st.CurrentTime()
	number := st.SecretScanningNextNumber[repoKey]
	st.SecretScanningNextNumber[repoKey] = number + 1

	a := &SecretScanningAlert{
		ID:                    st.NextSecretScanningAlertID,
		NodeID:                fmt.Sprintf("SSA_%d", st.NextSecretScanningAlertID),
		Number:                number,
		RepoKey:               repoKey,
		SecretType:            secretType,
		SecretTypeDisplayName: secretTypeDisplayName(secretType),
		State:                 "open",
		Locations:             locations,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	st.NextSecretScanningAlertID++

	st.SecretScanningAlerts[a.ID] = a
	st.SecretScanningAlertsByRepo[repoKey][number] = a
	st.persistSecretScanningAlert(a)
	return a
}

// CreateSecretScanningAlertIfNew records an alert unless the repo already has
// the same secret type at the same blob location.
func (st *Store) CreateSecretScanningAlertIfNew(repoKey, secretType string, locations []SecretScanningLocation) *SecretScanningAlert {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	for _, existing := range st.SecretScanningAlertsByRepo[repoKey] {
		if existing.SecretType != secretType || len(existing.Locations) != len(locations) {
			continue
		}
		if slices.EqualFunc(existing.Locations, locations, sameSecretScanningLocation) {
			return existing
		}
	}
	return st.CreateSecretScanningAlertLocked(repoKey, secretType, locations)
}

func sameSecretScanningLocation(a, b SecretScanningLocation) bool {
	return a.Type == b.Type &&
		a.Details.Path == b.Details.Path &&
		a.Details.StartLine == b.Details.StartLine &&
		a.Details.EndLine == b.Details.EndLine &&
		a.Details.StartColumn == b.Details.StartColumn &&
		a.Details.EndColumn == b.Details.EndColumn &&
		a.Details.BlobSHA == b.Details.BlobSHA
}

func secretTypeDisplayName(secretType string) string {
	switch secretType {
	case "github_personal_access_token":
		return "GitHub Personal Access Token"
	case "aws_access_key_id":
		return "AWS Access Key ID"
	case "google_api_key":
		return "Google API Key"
	case "slack_incoming_webhook_url":
		return "Slack Incoming Webhook URL"
	default:
		return secretType
	}
}

// cloneSecretScanningAlert returns a detached deep copy (STORE-021). Locations
// and ResolvedAt are its only reference fields, so copying the struct plus those suffices.
func cloneSecretScanningAlert(a *SecretScanningAlert) *SecretScanningAlert {
	if a == nil {
		return nil
	}
	clone := *a
	if a.Locations != nil {
		clone.Locations = append([]SecretScanningLocation(nil), a.Locations...)
	}
	if a.ResolvedAt != nil {
		resolved := *a.ResolvedAt
		clone.ResolvedAt = &resolved
	}
	return &clone
}

func (st *Store) GetSecretScanningAlert(repoKey string, number int) *SecretScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneSecretScanningAlert(st.SecretScanningAlertsByRepo[repoKey][number])
}

// sortAlertList orders alert records by created/updated time, defaulting to
// created descending. Shared by the secret-scanning, code-scanning, and
// dependabot org-level list endpoints.
func sortAlertList[T any](out []*T, sortField, direction string, createdAt, updatedAt func(*T) time.Time) {
	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}
	sort.SliceStable(out, func(i, j int) bool {
		var less bool
		switch sortField {
		case "updated":
			less = updatedAt(out[i]).Before(updatedAt(out[j]))
		default:
			less = createdAt(out[i]).Before(createdAt(out[j]))
		}
		if direction == "asc" {
			return less
		}
		return !less
	})
}

// ListSecretScanningAlerts returns repo alerts filtered/sorted per GitHub's
// list endpoint.
func (st *Store) ListSecretScanningAlerts(repoKey, state, secretType, resolution, sortField, direction string) []*SecretScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	byRepo := st.SecretScanningAlertsByRepo[repoKey]
	out := make([]*SecretScanningAlert, 0, len(byRepo))
	for _, a := range byRepo {
		if state != "" && a.State != SecretScanningState(state) {
			continue
		}
		if secretType != "" && a.SecretType != secretType {
			continue
		}
		if resolution != "" && a.Resolution != SecretScanningResolution(resolution) {
			continue
		}
		out = append(out, a)
	}

	sortAlertList(out, sortField, direction,
		func(a *SecretScanningAlert) time.Time { return a.CreatedAt },
		func(a *SecretScanningAlert) time.Time { return a.UpdatedAt })
	return snapshotSecretScanningAlerts(out)
}

// UpdateSecretScanningAlert applies a state/resolution transition. The caller's
// `a` is a detached clone, so the mutation is applied to the live alert
// re-fetched by key here and a fresh snapshot is written back into `a`.
func (st *Store) UpdateSecretScanningAlert(a *SecretScanningAlert, state, resolution, resolutionComment string) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	live := st.SecretScanningAlertsByRepo[a.RepoKey][a.Number]
	if live == nil {
		return fmt.Errorf("secret scanning alert %s#%d not found", a.RepoKey, a.Number)
	}
	if err := validateSecretScanningTransition(string(live.State), state, resolution); err != nil {
		return err
	}

	now := st.CurrentTime()
	if state != "" {
		live.State = SecretScanningState(state)
	}
	if state == "resolved" {
		live.Resolution = SecretScanningResolution(resolution)
		live.ResolutionComment = resolutionComment
		live.ResolvedAt = &now
	} else if state == "open" {
		live.Resolution = ""
		live.ResolutionComment = ""
		live.ResolvedAt = nil
	}
	live.UpdatedAt = now
	st.persistSecretScanningAlert(live)
	*a = *cloneSecretScanningAlert(live)
	return nil
}

// BulkUpdateSecretScanningAlerts resolves every alert matching the repo
// filters.
func (st *Store) BulkUpdateSecretScanningAlerts(repoKey, stateFilter, secretTypeFilter, resolutionFilter, newResolution, resolutionComment string) ([]*SecretScanningAlert, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	byRepo := st.SecretScanningAlertsByRepo[repoKey]
	now := st.CurrentTime()
	var updated []*SecretScanningAlert
	for _, a := range byRepo {
		if stateFilter != "" && a.State != SecretScanningState(stateFilter) {
			continue
		}
		if secretTypeFilter != "" && a.SecretType != secretTypeFilter {
			continue
		}
		if resolutionFilter != "" && a.Resolution != SecretScanningResolution(resolutionFilter) {
			continue
		}
		if err := validateSecretScanningTransition(string(a.State), "resolved", newResolution); err != nil {
			return nil, err
		}
		next := *a
		next.State = "resolved"
		next.Resolution = SecretScanningResolution(newResolution)
		next.ResolutionComment = resolutionComment
		next.ResolvedAt = &now
		next.UpdatedAt = now
		updated = append(updated, &next)
	}

	// Stage the complete result before touching memory or persistence: map
	// iteration order must not decide which subset survives a failed bulk op.
	batch := NewPersistBatch(st.Persist)
	for _, a := range updated {
		batch.Put("secret_scanning_alerts", strconv.Itoa(a.ID), a)
	}
	if err := batch.Commit(); err != nil {
		return nil, err
	}
	for _, a := range updated {
		st.SecretScanningAlerts[a.ID] = a
		st.SecretScanningAlertsByRepo[repoKey][a.Number] = a
	}
	sort.SliceStable(updated, func(i, j int) bool { return updated[i].Number < updated[j].Number })
	return updated, nil
}

func validateSecretScanningTransition(currentState, newState, resolution string) error {
	if newState != "" && newState != "open" && newState != "resolved" {
		return fmt.Errorf("invalid state %q", newState)
	}
	if newState == "resolved" {
		if !isValidResolution(resolution) {
			return fmt.Errorf("invalid resolution %q", resolution)
		}
	}
	if newState == "open" && currentState == "resolved" {
		return nil
	}
	if newState == "resolved" && (currentState == "open" || currentState == "resolved") {
		return nil
	}
	if newState == currentState {
		return nil
	}
	return fmt.Errorf("invalid transition from %q to %q", currentState, newState)
}

func isValidResolution(r string) bool {
	switch SecretScanningResolution(r) {
	// pattern_deleted/edited are system-set response-only values a client PATCH
	// cannot supply (422); the four below are the settable enum.
	case SecretScanningResolutionFalsePositive, SecretScanningResolutionWontFix,
		SecretScanningResolutionRevoked, SecretScanningResolutionUsedInTests:
		return true
	}
	return false
}

func (st *Store) persistSecretScanningAlert(a *SecretScanningAlert) {
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_alerts", strconv.Itoa(a.ID), a)
	}
}

// ListSecretScanningAlertsByOrg returns alerts for the org's repos, filtered and
// sorted per GitHub's org-alerts query parameters. Unknown filter values yield no
// matches rather than a 400, matching GitHub's lenient behavior.
func (st *Store) ListSecretScanningAlertsByOrg(orgID int, state, secretType, resolution, sortField, direction string) []*SecretScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var out []*SecretScanningAlert
	for repoKey, byNumber := range st.SecretScanningAlertsByRepo {
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != orgID {
			continue
		}
		for _, a := range byNumber {
			if state != "" && a.State != SecretScanningState(state) {
				continue
			}
			if secretType != "" && a.SecretType != secretType {
				continue
			}
			if resolution != "" && a.Resolution != SecretScanningResolution(resolution) {
				continue
			}
			out = append(out, a)
		}
	}

	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}
	sortAlertList(out, sortField, direction,
		func(a *SecretScanningAlert) time.Time { return a.CreatedAt },
		func(a *SecretScanningAlert) time.Time { return a.UpdatedAt })
	return snapshotSecretScanningAlerts(out)
}

// ListSecretScanningAlertsByUser returns alerts for the user's repos, newest
// first.
func (st *Store) ListSecretScanningAlertsByUser(userID int) []*SecretScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var out []*SecretScanningAlert
	for repoKey, byNumber := range st.SecretScanningAlertsByRepo {
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerID != userID {
			continue
		}
		for _, a := range byNumber {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return snapshotSecretScanningAlerts(out)
}

// secretScanningProviderPatterns is the catalog of partner patterns the
// pattern-configurations surface exposes.
var secretScanningProviderPatterns = []struct {
	patternID   string
	slug        string
	displayName string
}{
	{"ghp", "github_personal_access_token", "GitHub Personal Access Token"},
	{"gho", "github_oauth_access_token", "GitHub OAuth Access Token"},
	{"ghu", "github_user_to_server_token", "GitHub User-to-Server Token"},
	{"ghs", "github_server_to_server_token", "GitHub Server-to-Server Token"},
	{"ghr", "github_refresh_token", "GitHub Refresh Token"},
	{"aws", "aws_access_key_id", "AWS Access Key ID"},
	{"google", "google_api_key", "Google API Key"},
	{"slack", "slack_incoming_webhook_url", "Slack Incoming Webhook URL"},
}

func IsSecretScanningProviderPattern(tokenType string) bool {
	for _, p := range secretScanningProviderPatterns {
		if p.patternID == tokenType {
			return true
		}
	}
	return false
}

// OrgSecretScanningPatternConfig holds an org's push-protection pattern
// settings and the optimistic-concurrency version updates must present.
type OrgSecretScanningPatternConfig struct {
	Version          string            `json:"version"`
	ProviderSettings map[string]string `json:"provider_settings"` // token_type → not-set | disabled | enabled
	CustomSettings   map[string]string `json:"custom_settings"`   // token_type → disabled | enabled
	UpdatedAt        time.Time         `json:"updated_at"`
}

// ListSecretScanningPatternConfigurations returns the org's pattern overrides
// for GitHub's pattern-configurations endpoint, reflecting stored
// push-protection settings and computing alert totals from real alerts.
func (st *Store) ListSecretScanningPatternConfigurations(orgLogin string) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	orgLogin = st.canonicalOrgLoginLocked(orgLogin)
	cfg := st.SecretScanningPatternConfigs[orgLogin]
	org := st.OrgsByLogin[orgLogin]

	alertTotals := map[string]int{}
	falsePositives := map[string]int{}
	orgAlertTotal := 0
	if org != nil {
		for repoKey, byNumber := range st.SecretScanningAlertsByRepo {
			repo := st.ReposByName[repoKey]
			if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != org.ID {
				continue
			}
			for _, a := range byNumber {
				alertTotals[a.SecretType]++
				orgAlertTotal++
				if a.Resolution == "false_positive" {
					falsePositives[a.SecretType]++
				}
			}
		}
	}

	overrides := make([]map[string]interface{}, 0, len(secretScanningProviderPatterns))
	for _, p := range secretScanningProviderPatterns {
		setting := "not-set"
		if cfg != nil && cfg.ProviderSettings[p.patternID] != "" {
			setting = cfg.ProviderSettings[p.patternID]
		}
		total := alertTotals[p.slug] + alertTotals[p.patternID]
		fps := falsePositives[p.slug] + falsePositives[p.patternID]
		totalPct := 0.0
		if orgAlertTotal > 0 {
			totalPct = float64(total) / float64(orgAlertTotal) * 100
		}
		fpRate := 0.0
		if total > 0 {
			fpRate = float64(fps) / float64(total)
		}
		overrides = append(overrides, map[string]interface{}{
			"token_type":             p.patternID,
			"custom_pattern_version": nil,
			"slug":                   p.slug,
			"display_name":           p.displayName,
			"alert_total":            total,
			"alert_total_percentage": totalPct,
			"false_positives":        fps,
			"false_positive_rate":    fpRate,
			"bypass_rate":            0,
			"default_setting":        "enabled",
			"enterprise_setting":     nil,
			"setting":                setting,
		})
	}
	var version interface{}
	if cfg != nil {
		version = cfg.Version
	}
	return map[string]interface{}{
		"pattern_config_version":     version,
		"provider_pattern_overrides": overrides,
		"custom_pattern_overrides":   []map[string]interface{}{},
	}
}

// UpdateSecretScanningPatternConfig applies push-protection setting changes and
// returns the new version. A non-nil expectedVersion that mismatches the
// current version reports a conflict without changing anything.
func (st *Store) UpdateSecretScanningPatternConfig(orgLogin string, expectedVersion *string, provider, custom map[string]string) (string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	cfg := st.SecretScanningPatternConfigs[orgLogin]
	current := ""
	if cfg != nil {
		current = cfg.Version
	}
	if expectedVersion != nil && *expectedVersion != current {
		return "", false
	}
	if cfg == nil {
		cfg = &OrgSecretScanningPatternConfig{
			ProviderSettings: map[string]string{},
			CustomSettings:   map[string]string{},
		}
		st.SecretScanningPatternConfigs[orgLogin] = cfg
	}
	for tokenType, setting := range provider {
		if setting == "not-set" {
			delete(cfg.ProviderSettings, tokenType)
			continue
		}
		cfg.ProviderSettings[tokenType] = setting
	}
	for tokenType, setting := range custom {
		cfg.CustomSettings[tokenType] = setting
	}
	cfg.Version = uuid.New().String()
	cfg.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_pattern_configs", orgLogin, cfg)
	}
	return cfg.Version, true
}

// SecretScanningPushProtectionEnabled reports whether the org has enabled push
// protection for a provider pattern on this repo.
func (st *Store) SecretScanningPushProtectionEnabled(repo *Repo, patternID string) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	if repo == nil || repo.OwnerType != "Organization" {
		return false
	}
	org := st.Orgs[repo.OwnerID]
	if org == nil {
		return false
	}
	cfg := st.SecretScanningPatternConfigs[org.Login]
	return cfg != nil && cfg.ProviderSettings[patternID] == "enabled"
}

// SecretScanningPushProtectionPlaceholder is the identity a pusher presents
// when requesting a push protection bypass.
type SecretScanningPushProtectionPlaceholder struct {
	ID        string    `json:"id"`
	RepoKey   string    `json:"repo_key"`
	TokenType string    `json:"token_type"`
	CreatedAt time.Time `json:"created_at"`
}

// SecretScanningPushProtectionBypass is a granted push protection bypass.
type SecretScanningPushProtectionBypass struct {
	PlaceholderID string    `json:"placeholder_id"`
	RepoKey       string    `json:"repo_key"`
	Reason        string    `json:"reason"`
	TokenType     string    `json:"token_type"`
	ExpireAt      time.Time `json:"expire_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateSecretScanningPushProtectionPlaceholder records a blocked push's
// placeholder.
func (st *Store) CreateSecretScanningPushProtectionPlaceholder(repoKey, tokenType string) *SecretScanningPushProtectionPlaceholder {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	ph := &SecretScanningPushProtectionPlaceholder{
		ID:        uuid.New().String(),
		RepoKey:   repoKey,
		TokenType: tokenType,
		CreatedAt: st.CurrentTime(),
	}
	if st.SecretScanningPushPlaceholders[repoKey] == nil {
		st.SecretScanningPushPlaceholders[repoKey] = map[string]*SecretScanningPushProtectionPlaceholder{}
	}
	st.SecretScanningPushPlaceholders[repoKey][ph.ID] = ph
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_push_placeholders", repoKey, st.SecretScanningPushPlaceholders[repoKey])
	}
	return ph
}

const secretScanningPushProtectionBypassTTL = 2 * time.Hour

// CreateSecretScanningPushProtectionBypass consumes a placeholder and grants
// the bypass, returning nil when the placeholder does not exist for the repo.
func (st *Store) CreateSecretScanningPushProtectionBypass(repoKey, placeholderID, reason string) *SecretScanningPushProtectionBypass {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	ph := st.SecretScanningPushPlaceholders[repoKey][placeholderID]
	if ph == nil {
		return nil
	}
	delete(st.SecretScanningPushPlaceholders[repoKey], placeholderID)
	now := st.CurrentTime()
	bypass := &SecretScanningPushProtectionBypass{
		PlaceholderID: placeholderID,
		RepoKey:       repoKey,
		Reason:        reason,
		TokenType:     ph.TokenType,
		ExpireAt:      now.Add(secretScanningPushProtectionBypassTTL),
		CreatedAt:     now,
	}
	st.SecretScanningPushBypasses[repoKey] = append(st.SecretScanningPushBypasses[repoKey], bypass)
	// One transaction: consuming the placeholder and recording the bypass must
	// not disagree across a crash.
	batch := NewPersistBatch(st.Persist)
	batch.Put("secret_scanning_push_placeholders", repoKey, st.SecretScanningPushPlaceholders[repoKey])
	batch.Put("secret_scanning_push_bypasses", repoKey, st.SecretScanningPushBypasses[repoKey])
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "secret_scanning_push_bypasses", Err: err})
	}
	return bypass
}

// HasActiveSecretScanningPushProtectionBypass reports whether a granted bypass
// still permits a protected write for this token type.
func (st *Store) HasActiveSecretScanningPushProtectionBypass(repoKey, tokenType string, now time.Time) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	bypasses := st.SecretScanningPushBypasses[repoKey]
	if len(bypasses) == 0 {
		return false
	}
	active := bypasses[:0]
	found := false
	for _, bypass := range bypasses {
		if bypass == nil || !bypass.ExpireAt.After(now) {
			continue
		}
		active = append(active, bypass)
		if bypass.TokenType == tokenType {
			found = true
		}
	}
	if len(active) != len(bypasses) {
		if len(active) == 0 {
			// Delete rather than MustPut the nil slice, which would marshal to
			// `null` and reload as a permanent tombstone.
			delete(st.SecretScanningPushBypasses, repoKey)
			if st.Persist != nil {
				st.Persist.MustDelete("secret_scanning_push_bypasses", repoKey)
			}
		} else {
			st.SecretScanningPushBypasses[repoKey] = active
			if st.Persist != nil {
				st.Persist.MustPut("secret_scanning_push_bypasses", repoKey, active)
			}
		}
	}
	return found
}

// SecretScanningScanHistory derives the repo's scan history from recorded
// alert state: each alert-producing event is a completed incremental scan, the
// earliest is the backfill, and an org pattern-config update is a
// pattern-update scan. No activity yields an empty history.
func (st *Store) SecretScanningScanHistory(repo *Repo) (incremental, patternUpdate, backfill []*SecretScanningScanRecord) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var alertTimes []time.Time
	seen := map[time.Time]bool{}
	for _, a := range st.SecretScanningAlertsByRepo[repo.FullName] {
		t := a.CreatedAt.UTC().Truncate(time.Second)
		if !seen[t] {
			seen[t] = true
			alertTimes = append(alertTimes, t)
		}
	}
	sort.Slice(alertTimes, func(i, j int) bool { return alertTimes[i].Before(alertTimes[j]) })

	incremental = []*SecretScanningScanRecord{}
	backfill = []*SecretScanningScanRecord{}
	patternUpdate = []*SecretScanningScanRecord{}
	for i, t := range alertTimes {
		rec := &SecretScanningScanRecord{Type: "incremental", Status: "completed", StartedAt: t, CompletedAt: t}
		if i == 0 {
			backfill = append(backfill, &SecretScanningScanRecord{Type: "backfill", Status: "completed", StartedAt: t, CompletedAt: t})
		}
		incremental = append(incremental, rec)
	}

	if repo.OwnerType == "Organization" {
		ownerLogin, _, _ := strings.Cut(repo.FullName, "/")
		if cfg := st.SecretScanningPatternConfigs[ownerLogin]; cfg != nil {
			t := cfg.UpdatedAt.UTC().Truncate(time.Second)
			patternUpdate = append(patternUpdate, &SecretScanningScanRecord{Type: "pattern_update", Status: "completed", StartedAt: t, CompletedAt: t})
		}
	}
	return incremental, patternUpdate, backfill
}

// SecretScanningScanRecord is one scan in the repository's scan history.
type SecretScanningScanRecord struct {
	Type        string
	Status      string
	StartedAt   time.Time
	CompletedAt time.Time
}
