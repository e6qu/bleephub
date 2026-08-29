package store

// GitHub Copilot org subscription policy, per-seat activity, and the usage
// ledger the metrics endpoints aggregate. Seat assignment lives in
// store_copilot.go.
//
// STORE-021: every getter and List* here returns a detached snapshot.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Copilot plan types, matching the copilot-organization-details schema.
const (
	CopilotPlanBusiness   = "business"
	CopilotPlanEnterprise = "enterprise"
	CopilotPlanIndividual = "individual"
	CopilotPlanUnknown    = "unknown"
)

// Copilot seat-management settings.
const (
	CopilotSeatsAssignAll      = "assign_all"
	CopilotSeatsAssignSelected = "assign_selected"
	CopilotSeatsDisabled       = "disabled"
	CopilotSeatsUnconfigured   = "unconfigured"
)

// Copilot feature policy values.
const (
	CopilotFeatureEnabled      = "enabled"
	CopilotFeatureDisabled     = "disabled"
	CopilotFeatureUnconfigured = "unconfigured"
	CopilotSuggestionsAllow    = "allow"
	CopilotSuggestionsBlock    = "block"
)

// CopilotOrgPolicy is an organization's Copilot subscription: plan, seat
// handout, and which features members may use.
type CopilotOrgPolicy struct {
	OrgLogin              string    `json:"org_login"`
	PlanType              string    `json:"plan_type"`
	SeatManagementSetting string    `json:"seat_management_setting"`
	PublicCodeSuggestions string    `json:"public_code_suggestions"`
	IDEChat               string    `json:"ide_chat"`
	PlatformChat          string    `json:"platform_chat"`
	CLI                   string    `json:"cli"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// DefaultCopilotOrgPolicy is the provisioning posture: Business, seats assigned
// individually, every feature on.
func DefaultCopilotOrgPolicy(orgLogin string) *CopilotOrgPolicy {
	return &CopilotOrgPolicy{
		OrgLogin:              orgLogin,
		PlanType:              CopilotPlanBusiness,
		SeatManagementSetting: CopilotSeatsAssignSelected,
		PublicCodeSuggestions: CopilotSuggestionsAllow,
		IDEChat:               CopilotFeatureEnabled,
		PlatformChat:          CopilotFeatureEnabled,
		CLI:                   CopilotFeatureEnabled,
	}
}

// CopilotSeatActivity is when a seat was last used, and from where. Separate
// from the seat because usage writes it, not seat administration.
type CopilotSeatActivity struct {
	OrgLogin       string    `json:"org_login"`
	UserID         int       `json:"user_id"`
	LastActivityAt time.Time `json:"last_activity_at"`
	LastEditor     string    `json:"last_activity_editor"`
}

// CopilotUsageRecord is one member's Copilot usage for one day/editor/language.
// The metrics endpoints are pure aggregations of these rows.
type CopilotUsageRecord struct {
	ID              int    `json:"id"`
	OrgLogin        string `json:"org_login"`
	TeamSlug        string `json:"team_slug,omitempty"`
	UserID          int    `json:"user_id"`
	UserLogin       string `json:"user_login"`
	Day             string `json:"day"` // YYYY-MM-DD
	Editor          string `json:"editor"`
	Model           string `json:"model,omitempty"`
	Language        string `json:"language,omitempty"`
	RepoFullName    string `json:"repo_full_name,omitempty"`
	Suggestions     int    `json:"suggestions"`
	Acceptances     int    `json:"acceptances"`
	LinesSuggested  int    `json:"lines_suggested"`
	LinesAccepted   int    `json:"lines_accepted"`
	ChatTurns       int    `json:"chat_turns"`
	ChatAcceptances int    `json:"chat_acceptances"`
}

type CopilotPolicyStore struct {
	Mu      sync.RWMutex `json:"-"`
	Persist *Persistence `json:"-"`

	policies  map[string]*CopilotOrgPolicy            // org login → policy
	activity  map[string]map[int]*CopilotSeatActivity // org login → user ID → activity
	usage     map[int]*CopilotUsageRecord             // id → record
	nextUsage int
}

func NewCopilotPolicyStore() *CopilotPolicyStore {
	return &CopilotPolicyStore{
		policies:  map[string]*CopilotOrgPolicy{},
		activity:  map[string]map[int]*CopilotSeatActivity{},
		usage:     map[int]*CopilotUsageRecord{},
		nextUsage: 1,
	}
}

const (
	copilotPoliciesBucket = "copilot_org_policies"
	copilotActivityBucket = "copilot_seat_activity"
	copilotUsageBucket    = "copilot_usage_records"
)

func copilotActivityKey(orgLogin string, userID int) string {
	return orgLogin + "/" + strconv.Itoa(userID)
}

// loadCopilotPolicies repopulates the policy, activity and usage tables from storage.
func (st *Store) loadCopilotPolicies() error {
	cs := st.CopilotPolicies
	cs.Persist = st.Persist
	if err := st.loadBucket(copilotPoliciesBucket, func(raw []byte) error {
		var p CopilotOrgPolicy
		if err := LoadJSON(raw, &p); err != nil {
			return err
		}
		cs.policies[p.OrgLogin] = &p
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket(copilotActivityBucket, func(raw []byte) error {
		var a CopilotSeatActivity
		if err := LoadJSON(raw, &a); err != nil {
			return err
		}
		if cs.activity[a.OrgLogin] == nil {
			cs.activity[a.OrgLogin] = map[int]*CopilotSeatActivity{}
		}
		cs.activity[a.OrgLogin][a.UserID] = &a
		return nil
	}); err != nil {
		return err
	}
	return st.loadBucket(copilotUsageBucket, func(raw []byte) error {
		var record CopilotUsageRecord
		if err := LoadJSON(raw, &record); err != nil {
			return err
		}
		cs.usage[record.ID] = &record
		if record.ID >= cs.nextUsage {
			cs.nextUsage = record.ID + 1
		}
		return nil
	})
}

// GetCopilotOrgPolicy returns the org's Copilot policy; an unconfigured org
// reads as the default posture and nothing is materialized.
func (cs *CopilotPolicyStore) GetCopilotOrgPolicy(orgLogin string) *CopilotOrgPolicy {
	cs.Mu.RLock()
	defer cs.Mu.RUnlock()
	if policy := cs.policies[orgLogin]; policy != nil {
		clone := *policy
		return &clone
	}
	return DefaultCopilotOrgPolicy(orgLogin)
}

// CopilotOrgPolicyUpdate is a sparse patch; nil fields are left unchanged.
type CopilotOrgPolicyUpdate struct {
	PlanType              *string
	SeatManagementSetting *string
	PublicCodeSuggestions *string
	IDEChat               *string
	PlatformChat          *string
	CLI                   *string
}

// SetCopilotOrgPolicy applies a sparse patch, rejecting any value GitHub does
// not define for the field.
func (cs *CopilotPolicyStore) SetCopilotOrgPolicy(orgLogin string, patch CopilotOrgPolicyUpdate, now time.Time) (*CopilotOrgPolicy, error) {
	cs.Mu.Lock()
	defer cs.Mu.Unlock()
	policy := DefaultCopilotOrgPolicy(orgLogin)
	if existing := cs.policies[orgLogin]; existing != nil {
		clone := *existing
		policy = &clone
	}
	for _, field := range []struct {
		value   *string
		target  *string
		allowed []string
		name    string
	}{
		{patch.PlanType, &policy.PlanType, []string{CopilotPlanBusiness, CopilotPlanEnterprise, CopilotPlanIndividual, CopilotPlanUnknown}, "plan_type"},
		{patch.SeatManagementSetting, &policy.SeatManagementSetting, []string{CopilotSeatsAssignAll, CopilotSeatsAssignSelected, CopilotSeatsDisabled, CopilotSeatsUnconfigured}, "seat_management_setting"},
		{patch.PublicCodeSuggestions, &policy.PublicCodeSuggestions, []string{CopilotSuggestionsAllow, CopilotSuggestionsBlock, CopilotFeatureUnconfigured}, "public_code_suggestions"},
		{patch.IDEChat, &policy.IDEChat, []string{CopilotFeatureEnabled, CopilotFeatureDisabled, CopilotFeatureUnconfigured}, "ide_chat"},
		{patch.PlatformChat, &policy.PlatformChat, []string{CopilotFeatureEnabled, CopilotFeatureDisabled, CopilotFeatureUnconfigured}, "platform_chat"},
		{patch.CLI, &policy.CLI, []string{CopilotFeatureEnabled, CopilotFeatureDisabled, CopilotFeatureUnconfigured}, "cli"},
	} {
		if field.value == nil {
			continue
		}
		if !copilotPolicyValueAllowed(*field.value, field.allowed) {
			return nil, fmt.Errorf("%s must be one of %s", field.name, strings.Join(field.allowed, ", "))
		}
		*field.target = *field.value
	}
	policy.UpdatedAt = now.UTC()
	batch := NewPersistBatch(cs.Persist)
	batch.Put(copilotPoliciesBucket, orgLogin, policy)
	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("persist copilot policy: %w", err)
	}
	cs.policies[orgLogin] = policy
	clone := *policy
	return &clone, nil
}

func copilotPolicyValueAllowed(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// GetCopilotSeatActivity returns when the member last used Copilot, or nil.
func (cs *CopilotPolicyStore) GetCopilotSeatActivity(orgLogin string, userID int) *CopilotSeatActivity {
	cs.Mu.RLock()
	defer cs.Mu.RUnlock()
	if activity := cs.activity[orgLogin][userID]; activity != nil {
		clone := *activity
		return &clone
	}
	return nil
}

// RecordCopilotUsage files one day's usage and advances the seat's
// last-activity marker. Both commit together, so metrics and seat details can
// never disagree about whether a member has used Copilot.
func (cs *CopilotPolicyStore) RecordCopilotUsage(record *CopilotUsageRecord, at time.Time) (*CopilotUsageRecord, error) {
	if record == nil || record.OrgLogin == "" || record.UserID <= 0 {
		return nil, fmt.Errorf("copilot usage needs an organization and a member")
	}
	if record.Day == "" {
		record.Day = at.UTC().Format("2006-01-02")
	}
	if record.Editor == "" {
		return nil, fmt.Errorf("copilot usage needs an editor")
	}
	if record.Suggestions < 0 || record.Acceptances < 0 || record.Acceptances > record.Suggestions {
		return nil, fmt.Errorf("acceptances must be between zero and the number of suggestions")
	}
	if record.LinesAccepted > record.LinesSuggested {
		return nil, fmt.Errorf("accepted lines cannot exceed suggested lines")
	}
	if record.ChatAcceptances > record.ChatTurns {
		return nil, fmt.Errorf("chat acceptances cannot exceed chat turns")
	}

	cs.Mu.Lock()
	defer cs.Mu.Unlock()
	stored := *record
	stored.ID = cs.nextUsage

	activity := &CopilotSeatActivity{
		OrgLogin: stored.OrgLogin, UserID: stored.UserID,
		LastActivityAt: at.UTC(), LastEditor: stored.Editor,
	}
	if previous := cs.activity[stored.OrgLogin][stored.UserID]; previous != nil && previous.LastActivityAt.After(activity.LastActivityAt) {
		activity = previous
	}

	batch := NewPersistBatch(cs.Persist)
	batch.Put(copilotUsageBucket, strconv.Itoa(stored.ID), &stored)
	batch.Put(copilotActivityBucket, copilotActivityKey(activity.OrgLogin, activity.UserID), activity)
	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("persist copilot usage: %w", err)
	}
	cs.usage[stored.ID] = &stored
	cs.nextUsage++
	if cs.activity[stored.OrgLogin] == nil {
		cs.activity[stored.OrgLogin] = map[int]*CopilotSeatActivity{}
	}
	cs.activity[stored.OrgLogin][activity.UserID] = activity
	clone := stored
	return &clone, nil
}

// ListCopilotUsage returns usage rows in the inclusive day window [since, until]
// (either bound may be empty), optionally narrowed to one team.
func (cs *CopilotPolicyStore) ListCopilotUsage(orgLogin, teamSlug, since, until string) []*CopilotUsageRecord {
	cs.Mu.RLock()
	defer cs.Mu.RUnlock()
	out := make([]*CopilotUsageRecord, 0)
	for _, record := range cs.usage {
		if !strings.EqualFold(record.OrgLogin, orgLogin) {
			continue
		}
		if teamSlug != "" && record.TeamSlug != teamSlug {
			continue
		}
		if since != "" && record.Day < since {
			continue
		}
		if until != "" && record.Day > until {
			continue
		}
		clone := *record
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// CopilotDailyMetrics is one day of aggregated usage, shaped as the metrics endpoints report it.
type CopilotDailyMetrics struct {
	Date              string
	TotalActiveUsers  int
	TotalEngagedUsers int

	CompletionsEngagedUsers int
	ChatEngagedUsers        int

	Editors   []CopilotEditorMetrics
	ChatTotal CopilotChatMetrics
}

// CopilotEditorMetrics is one editor's slice of a day, broken down by model and language.
type CopilotEditorMetrics struct {
	Name         string
	EngagedUsers int
	Models       []CopilotModelMetrics
}

type CopilotModelMetrics struct {
	Name         string
	EngagedUsers int
	Languages    []CopilotLanguageMetrics
}

// CopilotLanguageMetrics is the leaf of the completions breakdown.
type CopilotLanguageMetrics struct {
	Name            string
	EngagedUsers    int
	SuggestionCount int
	AcceptanceCount int
	LinesSuggested  int
	LinesAccepted   int
}

type CopilotChatMetrics struct {
	EngagedUsers int
	Chats        int
	Insertions   int
}

// AggregateCopilotMetrics rolls usage rows into the daily metrics shape. Pure
// function of the ledger: no rows yields no days (the "no activity" response).
func AggregateCopilotMetrics(records []*CopilotUsageRecord) []CopilotDailyMetrics {
	byDay := map[string][]*CopilotUsageRecord{}
	days := make([]string, 0)
	for _, record := range records {
		if _, seen := byDay[record.Day]; !seen {
			days = append(days, record.Day)
		}
		byDay[record.Day] = append(byDay[record.Day], record)
	}
	sort.Strings(days)

	out := make([]CopilotDailyMetrics, 0, len(days))
	for _, day := range days {
		out = append(out, aggregateCopilotDay(day, byDay[day]))
	}
	return out
}

func aggregateCopilotDay(day string, records []*CopilotUsageRecord) CopilotDailyMetrics {
	metrics := CopilotDailyMetrics{Date: day}
	activeUsers := map[int]bool{}
	completionUsers := map[int]bool{}
	chatUsers := map[int]bool{}

	// Track engaged-user sets at each level so a user active in two languages
	// counts once per level.
	type leafKey struct{ editor, model, language string }
	leaves := map[leafKey]*CopilotLanguageMetrics{}
	leafUsers := map[leafKey]map[int]bool{}
	editorUsers := map[string]map[int]bool{}
	modelUsers := map[[2]string]map[int]bool{}
	editorOrder := make([]string, 0)
	modelOrder := map[string][]string{}
	languageOrder := map[[2]string][]string{}

	chats, insertions := 0, 0
	for _, record := range records {
		activeUsers[record.UserID] = true
		model := record.Model
		if model == "" {
			model = "default"
		}
		language := record.Language
		if language == "" {
			language = "unknown"
		}
		if record.Suggestions > 0 || record.LinesSuggested > 0 {
			completionUsers[record.UserID] = true
			key := leafKey{record.Editor, model, language}
			if leaves[key] == nil {
				leaves[key] = &CopilotLanguageMetrics{Name: language}
				leafUsers[key] = map[int]bool{}
				if editorUsers[record.Editor] == nil {
					editorUsers[record.Editor] = map[int]bool{}
					editorOrder = append(editorOrder, record.Editor)
				}
				modelKey := [2]string{record.Editor, model}
				if modelUsers[modelKey] == nil {
					modelUsers[modelKey] = map[int]bool{}
					modelOrder[record.Editor] = append(modelOrder[record.Editor], model)
				}
				languageOrder[modelKey] = append(languageOrder[modelKey], language)
			}
			leaf := leaves[key]
			leaf.SuggestionCount += record.Suggestions
			leaf.AcceptanceCount += record.Acceptances
			leaf.LinesSuggested += record.LinesSuggested
			leaf.LinesAccepted += record.LinesAccepted
			leafUsers[key][record.UserID] = true
			editorUsers[record.Editor][record.UserID] = true
			modelUsers[[2]string{record.Editor, model}][record.UserID] = true
		}
		if record.ChatTurns > 0 {
			chatUsers[record.UserID] = true
			chats += record.ChatTurns
			insertions += record.ChatAcceptances
		}
	}

	engaged := map[int]bool{}
	for id := range completionUsers {
		engaged[id] = true
	}
	for id := range chatUsers {
		engaged[id] = true
	}
	metrics.TotalActiveUsers = len(activeUsers)
	metrics.TotalEngagedUsers = len(engaged)
	metrics.CompletionsEngagedUsers = len(completionUsers)
	metrics.ChatEngagedUsers = len(chatUsers)
	metrics.ChatTotal = CopilotChatMetrics{EngagedUsers: len(chatUsers), Chats: chats, Insertions: insertions}

	sort.Strings(editorOrder)
	for _, editor := range editorOrder {
		editorMetrics := CopilotEditorMetrics{Name: editor, EngagedUsers: len(editorUsers[editor])}
		models := append([]string(nil), modelOrder[editor]...)
		sort.Strings(models)
		for _, model := range models {
			modelKey := [2]string{editor, model}
			modelMetrics := CopilotModelMetrics{Name: model, EngagedUsers: len(modelUsers[modelKey])}
			languages := append([]string(nil), languageOrder[modelKey]...)
			sort.Strings(languages)
			for _, language := range languages {
				key := leafKey{editor, model, language}
				leaf := *leaves[key]
				leaf.EngagedUsers = len(leafUsers[key])
				modelMetrics.Languages = append(modelMetrics.Languages, leaf)
			}
			editorMetrics.Models = append(editorMetrics.Models, modelMetrics)
		}
		metrics.Editors = append(metrics.Editors, editorMetrics)
	}
	return metrics
}
