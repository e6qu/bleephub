package store

import (
	"slices"
	"strings"
	"time"
)

// Per-type notification preferences. GitHub has no REST API for these, so the
// simulator stores them per user and serves them from /ui-data.

// NotificationChannels is the delivery selection: email, the web inbox, or both.
type NotificationChannels struct {
	Email bool `json:"email"`
	Web   bool `json:"web"`
}

// Notification event types. Only issue and pull_request raise threads today;
// the rest are stored so the settings page round-trips.
const (
	NotificationEventIssue       = "issue"
	NotificationEventPullRequest = "pull_request"
	NotificationEventRelease     = "release"
	NotificationEventDiscussion  = "discussion"
	NotificationEventCommit      = "commit"
	NotificationEventActions     = "actions"
	NotificationEventDependabot  = "dependabot"
)

// NotificationEventTypes is the ordered set of accepted event types; an unknown
// key in a payload is dropped.
var NotificationEventTypes = []string{
	NotificationEventIssue,
	NotificationEventPullRequest,
	NotificationEventRelease,
	NotificationEventDiscussion,
	NotificationEventCommit,
	NotificationEventActions,
	NotificationEventDependabot,
}

// NotificationPreferences is one account's full notification configuration.
type NotificationPreferences struct {
	// Participating: threads the user authored, is assigned, review-requested,
	// commented on, or @-mentioned in.
	Participating NotificationChannels `json:"participating"`
	// Watching: everything else from a watched repo or thread subscription.
	Watching                       NotificationChannels `json:"watching"`
	AutomaticallyWatchRepositories bool                 `json:"automatically_watch_repositories"`
	AutomaticallyWatchTeams        bool                 `json:"automatically_watch_teams"`
	// Events keys delivery by event type; a type absent from the map inherits
	// DefaultNotificationChannels.
	Events                     map[string]NotificationChannels `json:"events"`
	IncludeOwnUpdates          bool                            `json:"include_own_updates"`
	ActionsFailedWorkflowsOnly bool                            `json:"actions_failed_workflows_only"`
	DependabotWeeklyDigest     bool                            `json:"dependabot_weekly_digest"`
	// EmailDeliveryRestricted is computed on every read (normalize clears it),
	// never persisted: the enterprise policy bars this address, so every email
	// channel above reads false.
	EmailDeliveryRestricted bool `json:"email_delivery_restricted,omitempty"`
}

// DefaultNotificationChannels is the delivery for an event type the user has no
// opinion about.
func DefaultNotificationChannels() NotificationChannels {
	return NotificationChannels{Email: true, Web: true}
}

// DefaultNotificationPreferences matches github.com's out-of-the-box defaults.
func DefaultNotificationPreferences() NotificationPreferences {
	events := make(map[string]NotificationChannels, len(NotificationEventTypes))
	for _, event := range NotificationEventTypes {
		events[event] = DefaultNotificationChannels()
	}
	return NotificationPreferences{
		Participating:                  DefaultNotificationChannels(),
		Watching:                       DefaultNotificationChannels(),
		AutomaticallyWatchRepositories: true,
		AutomaticallyWatchTeams:        false,
		Events:                         events,
		IncludeOwnUpdates:              false,
		ActionsFailedWorkflowsOnly:     true,
		DependabotWeeklyDigest:         true,
	}
}

// clone detaches the preferences, including the event map (STORE-021).
func (p NotificationPreferences) clone() NotificationPreferences {
	copied := p
	copied.Events = make(map[string]NotificationChannels, len(p.Events))
	for event, channels := range p.Events {
		copied.Events[event] = channels
	}
	return copied
}

// normalize drops undefined event keys and fills in omitted ones, so the
// persisted shape is always the full bounded set.
func (p NotificationPreferences) normalize() NotificationPreferences {
	normalized := p
	normalized.EmailDeliveryRestricted = false
	events := make(map[string]NotificationChannels, len(NotificationEventTypes))
	for _, event := range NotificationEventTypes {
		if channels, ok := p.Events[event]; ok {
			events[event] = channels
			continue
		}
		events[event] = DefaultNotificationChannels()
	}
	normalized.Events = events
	return normalized
}

func (p NotificationPreferences) channelsFor(event string) NotificationChannels {
	if channels, ok := p.Events[event]; ok {
		return channels
	}
	return DefaultNotificationChannels()
}

// NotificationEventTypeForThread maps a thread subject type to its preference key.
func NotificationEventTypeForThread(subjectType string) string {
	switch strings.ToLower(subjectType) {
	case "issue":
		return NotificationEventIssue
	case "pullrequest", "pull_request", "pull-request":
		return NotificationEventPullRequest
	case "release":
		return NotificationEventRelease
	case "discussion":
		return NotificationEventDiscussion
	case "commit":
		return NotificationEventCommit
	case "checksuite", "check_suite", "workflowrun", "workflow_run":
		return NotificationEventActions
	case "repositoryvulnerabilityalert", "repository_vulnerability_alert", "dependabotalert":
		return NotificationEventDependabot
	default:
		return ""
	}
}

// participatingReasons make the viewer a participant; everything else is watching.
var participatingReasons = []string{"author", "assign", "comment", "mention", "review_requested", "team_mention", "manual"}

// NotificationDeliversWeb reports whether a thread of this subject type and
// reason reaches the web inbox. Both the subscription class and the event type
// must permit web delivery.
func (p NotificationPreferences) NotificationDeliversWeb(subjectType, reason string) bool {
	class := p.Watching
	if slices.Contains(participatingReasons, reason) {
		class = p.Participating
	}
	if !class.Web {
		return false
	}
	event := NotificationEventTypeForThread(subjectType)
	if event == "" {
		// A thread source with no preference key is not suppressed.
		return true
	}
	return p.channelsFor(event).Web
}

// withoutEmailDelivery clears every email channel and raises the restriction
// flag, for an account whose address the enterprise will not deliver to.
func (p NotificationPreferences) withoutEmailDelivery() NotificationPreferences {
	cleared := p.clone()
	cleared.Participating.Email = false
	cleared.Watching.Email = false
	cleared.DependabotWeeklyDigest = false
	for event, channels := range cleared.Events {
		channels.Email = false
		cleared.Events[event] = channels
	}
	cleared.EmailDeliveryRestricted = true
	return cleared
}

// SelectsEmailDelivery reports whether the document requests email delivery
// anywhere. The write refuses it when the enterprise will not deliver to the
// address.
func (p NotificationPreferences) SelectsEmailDelivery() bool {
	if p.Participating.Email || p.Watching.Email || p.DependabotWeeklyDigest {
		return true
	}
	for _, channels := range p.Events {
		if channels.Email {
			return true
		}
	}
	return false
}

// primaryEmailLocked is the delivery address: the account's primary address,
// or the profile address when none is flagged primary. Caller holds st.Mu.
func primaryEmailLocked(user *User) string {
	for _, address := range user.Emails {
		if address.Primary {
			return address.Email
		}
	}
	return user.Email
}

// NotificationEmailDeliveryAllowed reports whether email delivery to this
// account is permitted and whether a restriction is in force. Under the
// restriction only an address in a verified domain is deliverable — a property
// of the address, not the account's authority, so an enterprise owner outside
// the domains is undeliverable too.
func (st *Store) NotificationEmailDeliveryAllowed(userID int) (allowed, restricted bool) {
	enabled, domains := st.NotificationDeliveryRestriction()
	if !enabled {
		return true, false
	}
	st.Mu.RLock()
	user := st.Users[userID]
	address := ""
	if user != nil {
		address = primaryEmailLocked(user)
	}
	st.Mu.RUnlock()
	return EmailInVerifiedDomain(address, domains), true
}

// EffectiveNotificationPreferences applies the enterprise delivery restriction:
// the saved document with every email channel cleared when the address is
// undeliverable.
func (st *Store) EffectiveNotificationPreferences(userID int) (NotificationPreferences, bool) {
	preferences, ok := st.GetNotificationPreferences(userID)
	if !ok {
		return NotificationPreferences{}, false
	}
	if allowed, restricted := st.NotificationEmailDeliveryAllowed(userID); restricted && !allowed {
		return preferences.withoutEmailDelivery(), true
	}
	return preferences, true
}

// notificationPreferencesLocked resolves the account's preferences, defaulting
// for a user who has saved none. Caller holds st.Mu.
func notificationPreferencesLocked(user *User) NotificationPreferences {
	if user == nil || user.NotificationPreferences == nil {
		return DefaultNotificationPreferences()
	}
	return user.NotificationPreferences.normalize()
}

// GetNotificationPreferences returns a detached copy of the user's preferences
// (defaults when unset); false if the user does not exist.
func (st *Store) GetNotificationPreferences(userID int) (NotificationPreferences, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return NotificationPreferences{}, false
	}
	return notificationPreferencesLocked(user).clone(), true
}

// SetNotificationPreferences replaces the user's preferences and persists them.
// Returns false if the user does not exist.
func (st *Store) SetNotificationPreferences(userID int, preferences NotificationPreferences, now time.Time) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return false
	}
	saved := preferences.normalize().clone()
	user.NotificationPreferences = &saved
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return true
}
