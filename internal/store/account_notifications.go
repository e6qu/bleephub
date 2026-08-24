package store

import (
	"slices"
	"strings"
	"time"
)

// Per-type notification preferences, mirroring github.com's Settings →
// Notifications page. GitHub has no REST API for these, so the simulator stores
// them per user and serves them from /ui-data.
//
// The model is the page's own structure rather than a flattened set of global
// switches: a subscription class (participating vs watching) with its own
// delivery channels, an automatic-watching section, per-event-type delivery,
// and the two digest options. Preferences are enforced where the simulator can
// actually act on them — NotificationDeliversWeb gates the web inbox — rather
// than being stored and ignored.

// NotificationChannels is the delivery selection for one notification class or
// event type: email, the web inbox, or both.
type NotificationChannels struct {
	Email bool `json:"email"`
	Web   bool `json:"web"`
}

// Notification event types, matching the sections github.com's notification
// settings page offers. IssueEvent and PullRequestEvent are the two the
// simulator raises threads for today; the rest are stored so the page round
// trips faithfully and so a future thread source has a preference to read.
const (
	NotificationEventIssue       = "issue"
	NotificationEventPullRequest = "pull_request"
	NotificationEventRelease     = "release"
	NotificationEventDiscussion  = "discussion"
	NotificationEventCommit      = "commit"
	NotificationEventActions     = "actions"
	NotificationEventDependabot  = "dependabot"
)

// NotificationEventTypes is the canonical, ordered set of event types the
// settings page renders and the store accepts. An unknown key in a submitted
// payload is dropped rather than persisted, so the stored shape stays bounded.
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
	// Participating covers threads the user is in: authored, assigned,
	// review-requested, commented on, or @-mentioned.
	Participating NotificationChannels `json:"participating"`
	// Watching covers everything else arriving from a watched repository or
	// an explicit thread subscription.
	Watching NotificationChannels `json:"watching"`
	// AutomaticallyWatchRepositories mirrors "Automatically watch
	// repositories" — watch a repository on gaining push access.
	AutomaticallyWatchRepositories bool `json:"automatically_watch_repositories"`
	// AutomaticallyWatchTeams mirrors "Automatically watch teams".
	AutomaticallyWatchTeams bool `json:"automatically_watch_teams"`
	// Events is the per-event-type delivery selection, keyed by the constants
	// above. A type absent from the map inherits DefaultNotificationChannels.
	Events map[string]NotificationChannels `json:"events"`
	// IncludeOwnUpdates mirrors "Include your own updates": whether the user's
	// own activity produces notifications for themselves.
	IncludeOwnUpdates bool `json:"include_own_updates"`
	// ActionsFailedWorkflowsOnly narrows Actions notifications to failed runs,
	// which is github.com's default.
	ActionsFailedWorkflowsOnly bool `json:"actions_failed_workflows_only"`
	// DependabotWeeklyDigest mirrors the Dependabot alerts weekly email digest.
	DependabotWeeklyDigest bool `json:"dependabot_weekly_digest"`
	// EmailDeliveryRestricted reports that the enterprise's
	// notification-delivery restriction bars this account's address from
	// receiving email notifications, which is why every email channel above
	// reads false. It is a property of the enterprise policy and the account's
	// address, never of the saved document: normalize clears it, so it is
	// computed on every read and never persisted.
	EmailDeliveryRestricted bool `json:"email_delivery_restricted,omitempty"`
}

// DefaultNotificationChannels is the delivery an event type gets when the user
// has expressed no opinion about it.
func DefaultNotificationChannels() NotificationChannels {
	return NotificationChannels{Email: true, Web: true}
}

// DefaultNotificationPreferences matches github.com's out-of-the-box defaults:
// participating and watching both deliver everywhere, repositories are watched
// automatically, teams are not, your own updates are not echoed back to you,
// and Actions notifies only on failure.
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

// clone detaches the preferences, including the event map, so a caller cannot
// reach back into store state through the returned value (STORE-021).
func (p NotificationPreferences) clone() NotificationPreferences {
	copied := p
	copied.Events = make(map[string]NotificationChannels, len(p.Events))
	for event, channels := range p.Events {
		copied.Events[event] = channels
	}
	return copied
}

// normalize drops event keys the model does not define and fills in the ones a
// caller omitted, so what is persisted is always the full, bounded shape.
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

// channelsFor returns the delivery selection for an event type.
func (p NotificationPreferences) channelsFor(event string) NotificationChannels {
	if channels, ok := p.Events[event]; ok {
		return channels
	}
	return DefaultNotificationChannels()
}

// NotificationEventTypeForThread maps a notification thread's subject type to
// the preference key that governs it.
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

// participatingReasons are the thread reasons that make the viewer a
// participant rather than a bystander; everything else ("subscribed") is
// watching.
var participatingReasons = []string{"author", "assign", "comment", "mention", "review_requested", "team_mention", "manual"}

// NotificationDeliversWeb reports whether a thread of this subject type, with
// this reason, should appear in the user's web inbox. Both gates must pass:
// the subscription class (participating vs watching) and the event type.
//
// This is what makes the settings page mean something — switching "Watching →
// Web" off actually stops those threads arriving, instead of merely recording
// a preference nothing reads.
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
		// A thread source with no preference key is not silently suppressed.
		return true
	}
	return p.channelsFor(event).Web
}

// withoutEmailDelivery returns the preferences with every email channel off
// and the restriction flag raised. It is what an account whose address the
// enterprise will not deliver to actually has: a document that cannot claim a
// delivery the policy forbids.
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

// SelectsEmailDelivery reports whether the document asks for email delivery
// anywhere. The preferences write refuses such a document when the enterprise
// will not deliver to the account's address, rather than saving a request that
// can never be honored.
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

// primaryEmailLocked is the address notifications would be delivered to: the
// account's primary address, or the profile address when no address on the
// account is flagged primary. Callers hold st.Mu.
func primaryEmailLocked(user *User) string {
	for _, address := range user.Emails {
		if address.Primary {
			return address.Email
		}
	}
	return user.Email
}

// NotificationEmailDeliveryAllowed reports whether the enterprise's
// notification-delivery restriction permits email notifications to this
// account, and whether a restriction is in force at all. With the restriction
// on, only an address inside a verified domain is a delivery target — which is
// exactly GitHub's rule, and is a property of the address rather than of the
// account's authority, so an enterprise owner outside the verified domains is
// as undeliverable as anyone else.
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

// EffectiveNotificationPreferences is what the account's preferences actually
// mean once the enterprise's notification-delivery restriction is applied: the
// saved document, with every email channel cleared when the account's address
// is not one the enterprise delivers to.
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

// notificationPreferencesLocked resolves the account's preferences, falling
// back to the defaults for a user who has never saved any. Callers hold st.Mu.
func notificationPreferencesLocked(user *User) NotificationPreferences {
	if user == nil || user.NotificationPreferences == nil {
		return DefaultNotificationPreferences()
	}
	return user.NotificationPreferences.normalize()
}

// GetNotificationPreferences returns a detached copy of the user's preferences
// (the defaults when unset). The second result is false if the user does not
// exist.
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
