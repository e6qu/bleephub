package store

// NotificationSettings mirrors the toggles on github.com's Settings →
// Notifications page. GitHub has no REST API for these, so the simulator stores
// them per user and serves them from /ui-data.
type NotificationSettings struct {
	Email         bool `json:"email"`
	Web           bool `json:"web"`
	Participating bool `json:"participating"`
	Watching      bool `json:"watching"`
}

// DefaultNotificationSettings matches github.com's out-of-the-box defaults.
func DefaultNotificationSettings() NotificationSettings {
	return NotificationSettings{Email: true, Web: true, Participating: true, Watching: true}
}

// GetAccountSecurity returns the user's 2FA flag and a detached copy of their
// notification settings (defaults when unset). Third result is false if the
// user does not exist.
func (st *Store) GetAccountSecurity(userID int) (bool, NotificationSettings, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return false, NotificationSettings{}, false
	}
	notif := DefaultNotificationSettings()
	if user.NotificationSettings != nil {
		notif = *user.NotificationSettings
	}
	return user.TwoFactorEnabled, notif, true
}

// SetTwoFactor toggles the user's two-factor flag and persists.
func (st *Store) SetTwoFactor(userID int, enabled bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return false
	}
	user.TwoFactorEnabled = enabled
	user.UpdatedAt = st.CurrentTime()
	st.persistUserLocked(user)
	return true
}

// SetNotificationSettings replaces the user's notification settings and persists.
func (st *Store) SetNotificationSettings(userID int, s NotificationSettings) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return false
	}
	saved := s
	user.NotificationSettings = &saved
	user.UpdatedAt = st.CurrentTime()
	st.persistUserLocked(user)
	return true
}
