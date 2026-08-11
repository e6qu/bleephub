package store

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

type SecretScanningCustomPattern struct {
	ID                    int       `json:"id"`
	Name                  string    `json:"name"`
	Pattern               string    `json:"pattern"`
	Slug                  string    `json:"slug"`
	State                 string    `json:"state"`
	PushProtectionEnabled bool      `json:"push_protection_enabled"`
	StartDelimiter        *string   `json:"start_delimiter"`
	EndDelimiter          *string   `json:"end_delimiter"`
	MustMatch             []string  `json:"must_match"`
	MustNotMatch          []string  `json:"must_not_match"`
	Version               string    `json:"custom_pattern_version"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func cloneSecretScanningCustomPattern(pattern *SecretScanningCustomPattern) *SecretScanningCustomPattern {
	if pattern == nil {
		return nil
	}
	copy := *pattern
	copy.MustMatch = append([]string(nil), pattern.MustMatch...)
	copy.MustNotMatch = append([]string(nil), pattern.MustNotMatch...)
	return &copy
}

func (st *Store) ListSecretScanningCustomPatterns(scope string) []*SecretScanningCustomPattern {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*SecretScanningCustomPattern, 0, len(st.SecretScanningCustomPatterns[scope]))
	for _, pattern := range st.SecretScanningCustomPatterns[scope] {
		out = append(out, cloneSecretScanningCustomPattern(pattern))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSecretScanningCustomPatterns(out)
}

func (st *Store) CreateSecretScanningCustomPatterns(scope string, specs []SecretScanningPatternCreate) []*SecretScanningCustomPattern {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.SecretScanningCustomPatterns[scope] == nil {
		st.SecretScanningCustomPatterns[scope] = map[int]*SecretScanningCustomPattern{}
	}
	now := st.CurrentTime()
	out := make([]*SecretScanningCustomPattern, 0, len(specs))
	for _, spec := range specs {
		start := spec.StartDelimiter
		if start == nil {
			value := `\A|[^0-9A-Za-z]`
			start = &value
		}
		end := spec.EndDelimiter
		if end == nil {
			value := `\z|[^0-9A-Za-z]`
			end = &value
		}
		pattern := &SecretScanningCustomPattern{
			ID: st.NextSecretScanningPatternID, Name: spec.Name, Pattern: spec.Pattern,
			Slug: Slugify(spec.Name), State: "published", PushProtectionEnabled: false,
			StartDelimiter: start, EndDelimiter: end,
			MustMatch:    append([]string(nil), spec.MustMatch...),
			MustNotMatch: append([]string(nil), spec.MustNotMatch...),
			Version:      uuid.NewString(), CreatedAt: now, UpdatedAt: now,
		}
		st.NextSecretScanningPatternID++
		st.SecretScanningCustomPatterns[scope][pattern.ID] = pattern
		out = append(out, cloneSecretScanningCustomPattern(pattern))
	}
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return out
}

func (st *Store) UpdateSecretScanningCustomPattern(scope string, id int, update SecretScanningPatternUpdate) (*SecretScanningCustomPattern, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	pattern := st.SecretScanningCustomPatterns[scope][id]
	if pattern == nil {
		return nil, false
	}
	if pattern.Version != update.Version {
		return cloneSecretScanningCustomPattern(pattern), false
	}
	if update.Pattern != nil {
		pattern.Pattern = *update.Pattern
	}
	if update.StartDelimiter != nil {
		pattern.StartDelimiter = update.StartDelimiter
	}
	if update.EndDelimiter != nil {
		pattern.EndDelimiter = update.EndDelimiter
	}
	if update.MustMatch != nil {
		pattern.MustMatch = append([]string(nil), (*update.MustMatch)...)
	}
	if update.MustNotMatch != nil {
		pattern.MustNotMatch = append([]string(nil), (*update.MustNotMatch)...)
	}
	pattern.Version = uuid.NewString()
	pattern.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return cloneSecretScanningCustomPattern(pattern), true
}

func (st *Store) DeleteSecretScanningCustomPatterns(scope string, deletes []SecretScanningPatternDelete) (found, versionsOK bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, request := range deletes {
		pattern := st.SecretScanningCustomPatterns[scope][request.PatternID]
		if pattern == nil {
			return false, true
		}
		if request.Version != "" && request.Version != pattern.Version {
			return true, false
		}
	}
	for _, request := range deletes {
		delete(st.SecretScanningCustomPatterns[scope], request.PatternID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return true, true
}

type SecretScanningPatternCreate struct {
	Name           string   `json:"name"`
	Pattern        string   `json:"pattern"`
	StartDelimiter *string  `json:"start_delimiter"`
	EndDelimiter   *string  `json:"end_delimiter"`
	MustMatch      []string `json:"must_match"`
	MustNotMatch   []string `json:"must_not_match"`
}

type SecretScanningPatternDelete struct {
	PatternID int    `json:"pattern_id"`
	Version   string `json:"custom_pattern_version"`
}

type SecretScanningPatternUpdate struct {
	Pattern        *string   `json:"pattern"`
	StartDelimiter *string   `json:"start_delimiter"`
	EndDelimiter   *string   `json:"end_delimiter"`
	MustMatch      *[]string `json:"must_match"`
	MustNotMatch   *[]string `json:"must_not_match"`
	Version        string    `json:"custom_pattern_version"`
}
