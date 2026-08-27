package bleephub

// Copilot subscription policy, seat activity and usage aggregation behind
// the billing and metrics endpoints. GitHub publishes no REST write surface
// for policy or usage, so the two writers live under /ui-data.

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHCopilotPolicyRoutes() {
	s.route("GET /ui-data/orgs/{org}/copilot/policy", s.handleGetCopilotPolicy)
	s.route("PUT /ui-data/orgs/{org}/copilot/policy", s.handleSetCopilotPolicy)
	s.route("GET /ui-data/orgs/{org}/copilot/usage", s.handleListCopilotUsage)
	s.route("POST /ui-data/orgs/{org}/copilot/usage", s.handleRecordCopilotUsage)
}

func copilotPolicyJSON(policy *store.CopilotOrgPolicy) map[string]interface{} {
	return map[string]interface{}{
		"public_code_suggestions": policy.PublicCodeSuggestions,
		"ide_chat":                policy.IDEChat,
		"platform_chat":           policy.PlatformChat,
		"cli":                     policy.CLI,
		"seat_management_setting": policy.SeatManagementSetting,
		"plan_type":               policy.PlanType,
	}
}

// CopilotSeatActivityJSON renders a seat's activity members; an unused seat
// reads as null on both, matching GitHub.
func (s *Server) CopilotSeatActivityJSON(orgLogin string, userID int) (lastActivityAt, lastActivityEditor interface{}) {
	activity := s.store.CopilotPolicies.GetCopilotSeatActivity(orgLogin, userID)
	if activity == nil {
		return nil, nil
	}
	return activity.LastActivityAt.UTC().Format(time.RFC3339), activity.LastEditor
}

// copilotSeatIsActiveThisCycle reports whether the seat has been used since
// the start of the current monthly billing cycle.
func (s *Server) copilotSeatIsActiveThisCycle(orgLogin string, userID int, now time.Time) bool {
	activity := s.store.CopilotPolicies.GetCopilotSeatActivity(orgLogin, userID)
	if activity == nil {
		return false
	}
	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return !activity.LastActivityAt.Before(cycleStart)
}

func (s *Server) handleGetCopilotPolicy(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	writeJSON(w, http.StatusOK, copilotPolicyJSON(s.store.CopilotPolicies.GetCopilotOrgPolicy(org.Login)))
}

func (s *Server) handleSetCopilotPolicy(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		PlanType              *string `json:"plan_type"`
		SeatManagementSetting *string `json:"seat_management_setting"`
		PublicCodeSuggestions *string `json:"public_code_suggestions"`
		IDEChat               *string `json:"ide_chat"`
		PlatformChat          *string `json:"platform_chat"`
		CLI                   *string `json:"cli"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	policy, err := s.store.CopilotPolicies.SetCopilotOrgPolicy(org.Login, store.CopilotOrgPolicyUpdate{
		PlanType:              req.PlanType,
		SeatManagementSetting: req.SeatManagementSetting,
		PublicCodeSuggestions: req.PublicCodeSuggestions,
		IDEChat:               req.IDEChat,
		PlatformChat:          req.PlatformChat,
		CLI:                   req.CLI,
	}, s.currentTime())
	if err != nil {
		store.WriteGHValidationError(w, "CopilotPolicy", "settings", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, copilotPolicyJSON(policy))
}

// handleRecordCopilotUsage files a day of Copilot usage for a member; it is
// the writer behind the metrics figures and seat last-activity fields.
func (s *Server) handleRecordCopilotUsage(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		Username        string `json:"username"`
		TeamSlug        string `json:"team_slug"`
		Day             string `json:"day"`
		Editor          string `json:"editor"`
		Model           string `json:"model"`
		Language        string `json:"language"`
		Repository      string `json:"repository"`
		Suggestions     int    `json:"suggestions"`
		Acceptances     int    `json:"acceptances"`
		LinesSuggested  int    `json:"lines_suggested"`
		LinesAccepted   int    `json:"lines_accepted"`
		ChatTurns       int    `json:"chat_turns"`
		ChatAcceptances int    `json:"chat_acceptances"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	member := s.store.LookupUserByLogin(req.Username)
	if member == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Attribute usage only to a member holding a seat, else metrics count
	// unbilled activity.
	if s.store.GetCopilotSeat(org.Login, member.ID) == nil {
		store.WriteGHValidationError(w, "CopilotUsage", "username", "member does not hold a Copilot seat")
		return
	}
	if req.TeamSlug != "" && s.store.GetTeam(org.Login, req.TeamSlug) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	record, err := s.store.CopilotPolicies.RecordCopilotUsage(&store.CopilotUsageRecord{
		OrgLogin: org.Login, TeamSlug: req.TeamSlug, UserID: member.ID, UserLogin: member.Login,
		Day: req.Day, Editor: req.Editor, Model: req.Model, Language: req.Language,
		RepoFullName: req.Repository, Suggestions: req.Suggestions, Acceptances: req.Acceptances,
		LinesSuggested: req.LinesSuggested, LinesAccepted: req.LinesAccepted,
		ChatTurns: req.ChatTurns, ChatAcceptances: req.ChatAcceptances,
	}, s.currentTime())
	if err != nil {
		store.WriteGHValidationError(w, "CopilotUsage", "usage", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, copilotUsageRecordJSON(record))
}

func copilotUsageRecordJSON(record *store.CopilotUsageRecord) map[string]interface{} {
	return map[string]interface{}{
		"id": record.ID, "username": record.UserLogin, "team_slug": record.TeamSlug,
		"day": record.Day, "editor": record.Editor, "model": record.Model,
		"language": record.Language, "repository": record.RepoFullName,
		"suggestions": record.Suggestions, "acceptances": record.Acceptances,
		"lines_suggested": record.LinesSuggested, "lines_accepted": record.LinesAccepted,
		"chat_turns": record.ChatTurns, "chat_acceptances": record.ChatAcceptances,
	}
}

func (s *Server) handleListCopilotUsage(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	rows := []map[string]interface{}{}
	for _, record := range s.store.CopilotPolicies.ListCopilotUsage(org.Login,
		r.URL.Query().Get("team_slug"), r.URL.Query().Get("since"), r.URL.Query().Get("until")) {
		rows = append(rows, copilotUsageRecordJSON(record))
	}
	writeJSON(w, http.StatusOK, rows)
}

// CopilotMetricsForOrg aggregates recorded usage into GitHub's
// copilot-usage-metrics-day array, optionally narrowed to one team and a
// [since, until] window.
func (s *Server) CopilotMetricsForOrg(orgLogin, teamSlug, since, until string) []map[string]interface{} {
	records := s.store.CopilotPolicies.ListCopilotUsage(orgLogin, teamSlug, since, until)
	days := store.AggregateCopilotMetrics(records)
	out := make([]map[string]interface{}, 0, len(days))
	for _, day := range days {
		out = append(out, copilotMetricsDayJSON(day))
	}
	return out
}

func copilotMetricsDayJSON(day store.CopilotDailyMetrics) map[string]interface{} {
	editors := make([]map[string]interface{}, 0, len(day.Editors))
	for _, editor := range day.Editors {
		models := make([]map[string]interface{}, 0, len(editor.Models))
		for _, model := range editor.Models {
			languages := make([]map[string]interface{}, 0, len(model.Languages))
			for _, language := range model.Languages {
				languages = append(languages, map[string]interface{}{
					"name":                       language.Name,
					"total_engaged_users":        language.EngagedUsers,
					"total_code_suggestions":     language.SuggestionCount,
					"total_code_acceptances":     language.AcceptanceCount,
					"total_code_lines_suggested": language.LinesSuggested,
					"total_code_lines_accepted":  language.LinesAccepted,
				})
			}
			models = append(models, map[string]interface{}{
				"name":                model.Name,
				"is_custom_model":     false,
				"total_engaged_users": model.EngagedUsers,
				"languages":           languages,
			})
		}
		editors = append(editors, map[string]interface{}{
			"name":                editor.Name,
			"total_engaged_users": editor.EngagedUsers,
			"models":              models,
		})
	}
	return map[string]interface{}{
		"date":                day.Date,
		"total_active_users":  day.TotalActiveUsers,
		"total_engaged_users": day.TotalEngagedUsers,
		"copilot_ide_code_completions": map[string]interface{}{
			"total_engaged_users": day.CompletionsEngagedUsers,
			"editors":             editors,
		},
		"copilot_ide_chat": map[string]interface{}{
			"total_engaged_users": day.ChatEngagedUsers,
			"editors": []map[string]interface{}{{
				"name":                "all",
				"total_engaged_users": day.ChatTotal.EngagedUsers,
				"models": []map[string]interface{}{{
					"name":                        "default",
					"is_custom_model":             false,
					"total_engaged_users":         day.ChatTotal.EngagedUsers,
					"total_chats":                 day.ChatTotal.Chats,
					"total_chat_insertion_events": day.ChatTotal.Insertions,
					"total_chat_copy_events":      0,
				}},
			}},
		},
	}
}

// CopilotUsageDaysForOrg lists the days with recorded usage, newest first.
// The single-day report endpoints answer 204 for a day not in this list.
func (s *Server) CopilotUsageDaysForOrg(orgLogin string) []string {
	seen := map[string]bool{}
	days := []string{}
	for _, record := range s.store.CopilotPolicies.ListCopilotUsage(orgLogin, "", "", "") {
		if seen[record.Day] {
			continue
		}
		seen[record.Day] = true
		days = append(days, record.Day)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

// CopilotSeatBreakdown counts the org's seats as the billing endpoint
// reports them, with the active/inactive split from recorded activity.
func (s *Server) CopilotSeatBreakdown(org *store.Org, now time.Time) map[string]interface{} {
	seats := s.store.ListCopilotSeats(org.Login)
	total, pendingCancellation, addedThisCycle, pendingInvitation, active := len(seats), 0, 0, 0, 0
	for _, seat := range seats {
		if seat.PendingCancellationDate != "" {
			pendingCancellation++
		}
		if seat.CreatedAt.Year() == now.Year() && seat.CreatedAt.Month() == now.Month() {
			addedThisCycle++
		}
		if m := s.store.GetMembership(org.Login, seat.UserID); m != nil && m.State == store.MembershipStatePending {
			pendingInvitation++
		}
		if s.copilotSeatIsActiveThisCycle(org.Login, seat.UserID, now) {
			active++
		}
	}
	return map[string]interface{}{
		"total":                total,
		"added_this_cycle":     addedThisCycle,
		"pending_cancellation": pendingCancellation,
		"pending_invitation":   pendingInvitation,
		"active_this_cycle":    active,
		"inactive_this_cycle":  total - active,
	}
}

// copilotMetricsWindowBounds reads since/until query params as YYYY-MM-DD
// day bounds.
func copilotMetricsWindowBounds(r *http.Request) (since, until string) {
	trim := func(value string) string {
		value = strings.TrimSpace(value)
		if len(value) >= 10 {
			return value[:10]
		}
		return ""
	}
	return trim(r.URL.Query().Get("since")), trim(r.URL.Query().Get("until"))
}
