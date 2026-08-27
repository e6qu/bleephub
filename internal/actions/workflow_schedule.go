package actions

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// scheduleFiredKeys dedupes cron firings: a (repo, file, cron) tuple fires at
// most once per minute even if the ticker drifts.
type scheduleFiredKeys struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// maxScheduleCatchup bounds how many missed minutes one tick replays, so a
// long stall cannot unleash an unbounded burst of scans.
const maxScheduleCatchup = 10 * time.Minute

// startScheduleDispatcher launches the minute-aligned loop that fires
// `on: schedule:` workflows.
func (s *Engine) startScheduleDispatcher(ctx context.Context) {
	s.goBackground(func() {
		// Seed the cursor at the current minute so a fresh process does not
		// replay history.
		lastFired := s.currentTime().Truncate(time.Minute)
		for {
			now := s.currentTime()
			next := now.Truncate(time.Minute).Add(time.Minute)
			timer := time.NewTimer(next.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			tickTime := s.currentTime()
			// Per-tick hooks ride this minute clock (login-session reaping,
			// org-invitation reconciliation, STORE-034).
			for _, hook := range s.onScheduleTick {
				hook(tickTime)
			}
			lastFired = s.FireSchedulesThrough(lastFired, tickTime)
		}
	})
}

// FireSchedulesThrough replays every whole minute in (lastFired, now] so a scan
// that overran a minute boundary does not drop the minutes it skipped, bounded
// by maxScheduleCatchup. It returns the last minute processed.
func (s *Engine) FireSchedulesThrough(lastFired, now time.Time) time.Time {
	current := now.Truncate(time.Minute)
	if current.Before(lastFired) { // clock skew: never move the cursor backward
		return lastFired
	}
	minute := lastFired.Truncate(time.Minute).Add(time.Minute)
	if earliest := current.Add(-maxScheduleCatchup); minute.Before(earliest) {
		minute = earliest
	}
	for ; !minute.After(current); minute = minute.Add(time.Minute) {
		s.FireDueSchedules(minute)
	}
	return current
}

// scheduleIndex caches the parsed `on: schedule:` timetable for each
// repository's default branch, keyed by that branch's tip commit. Workflow
// content at a given tip is immutable, so invalidating on tip movement reduces
// the per-minute scan to cron-matching against pre-parsed schedules. Only
// schedule-bearing workflows are retained; parse and cron validation happen at
// build time, not every tick.
type scheduleIndex struct {
	mu       sync.Mutex
	entries  map[string]*scheduleIndexEntry
	rebuilds atomic.Int64
}

type scheduleIndexEntry struct {
	defaultBranch string
	tipSHA        string
	workflows     []indexedWorkflowSchedule
}

// indexedWorkflowSchedule is one schedule-bearing workflow file with its cron
// entries parsed. content is retained so a firing needs no second git read.
type indexedWorkflowSchedule struct {
	fileName string
	content  []byte
	entries  []indexedCronEntry
}

type indexedCronEntry struct {
	cron     string
	timezone string
	cs       *cronSchedule
}

// lookup returns the cached schedule for repoKey, rebuilding via build() when
// the entry is absent or its (defaultBranch, tipSHA) key no longer matches.
// build runs outside the lock; a given tip SHA yields identical content, so a
// rare concurrent duplicate build is harmless.
func (si *scheduleIndex) lookup(repoKey, defaultBranch, tipSHA string, build func() []indexedWorkflowSchedule) []indexedWorkflowSchedule {
	si.mu.Lock()
	if e, ok := si.entries[repoKey]; ok && e.tipSHA == tipSHA && e.defaultBranch == defaultBranch {
		wf := e.workflows
		si.mu.Unlock()
		return wf
	}
	si.mu.Unlock()

	built := build()
	si.rebuilds.Add(1)

	si.mu.Lock()
	if si.entries == nil {
		si.entries = map[string]*scheduleIndexEntry{}
	}
	si.entries[repoKey] = &scheduleIndexEntry{defaultBranch: defaultBranch, tipSHA: tipSHA, workflows: built}
	si.mu.Unlock()
	return built
}

// retain drops cache entries for repositories no longer present.
func (si *scheduleIndex) retain(live map[string]struct{}) {
	si.mu.Lock()
	defer si.mu.Unlock()
	for key := range si.entries {
		if _, ok := live[key]; !ok {
			delete(si.entries, key)
		}
	}
}

// buildScheduleIndex reads and parses every schedule-bearing workflow file at
// definitionRef. Invalid crons and sub-five-minute intervals are logged and
// dropped here, not re-warned every minute.
func (s *Engine) buildScheduleIndex(repoKey, definitionRef string, stor gitStorage.Storer) []indexedWorkflowSchedule {
	var out []indexedWorkflowSchedule
	for name, content := range ListWorkflowFilesAtRef(stor, definitionRef) {
		on, err := ParseWorkflowOn(content)
		if err != nil {
			s.logger.Warn().Err(err).Str("repo", repoKey).Str("file", name).Msg("invalid scheduled workflow")
			continue
		}
		sched := on["schedule"]
		if sched == nil || len(sched.Schedules) == 0 {
			continue
		}
		var entries []indexedCronEntry
		for _, entry := range sched.Schedules {
			cs, err := parseCron(entry.Cron)
			if err != nil {
				s.logger.Warn().Err(err).Str("file", name).Str("cron", entry.Cron).Msg("invalid cron in on: schedule")
				continue
			}
			if cs.minimumInterval() < 5*time.Minute {
				s.logger.Warn().Str("file", name).Str("cron", entry.Cron).Msg("scheduled workflow interval is shorter than GitHub's five-minute minimum")
				continue
			}
			entries = append(entries, indexedCronEntry{cron: entry.Cron, timezone: entry.Timezone, cs: cs})
		}
		if len(entries) == 0 {
			continue
		}
		out = append(out, indexedWorkflowSchedule{fileName: name, content: content, entries: entries})
	}
	return out
}

// FireDueSchedules triggers every schedule-bearing workflow from each
// repository's default branch whose cron matches the given minute. Separated
// from the ticker so tests drive it with a fixed clock.
func (s *Engine) FireDueSchedules(now time.Time) {
	minute := now.Truncate(time.Minute)
	if err := s.store.RefreshFromPersistenceIfStale(); err != nil {
		s.logger.Error().Err(err).Msg("scheduled workflow scan could not refresh shared state")
		return
	}

	s.store.Mu.RLock()
	repoKeys := make([]string, 0, len(s.store.ReposByName))
	for key := range s.store.ReposByName {
		repoKeys = append(repoKeys, key)
	}
	s.store.Mu.RUnlock()

	live := make(map[string]struct{}, len(repoKeys))
	for _, repoKey := range repoKeys {
		repo := s.store.GetRepoByFullName(repoKey)
		if repo == nil {
			continue
		}
		parts := SplitRepoKeyParts(repoKey)
		stor := s.store.GetGitStorage(parts[0], parts[1])
		if stor == nil {
			continue
		}
		defaultBranch := repo.DefaultBranch
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		definitionRef := plumbing.NewBranchReferenceName(defaultBranch).String()
		tipSHA := ResolveRefSha(stor, definitionRef)
		live[repoKey] = struct{}{}

		workflows := s.scheduleIndex.lookup(repoKey, defaultBranch, tipSHA, func() []indexedWorkflowSchedule {
			return s.buildScheduleIndex(repoKey, definitionRef, stor)
		})
		for _, wf := range workflows {
			if scheduleInactive(repo, minute) {
				s.store.SetWorkflowFileState(repoKey, ".github/workflows/"+wf.fileName, "disabled_inactivity")
				continue
			}
			if s.WorkflowFileDisabled(repoKey, wf.fileName) {
				continue
			}
			for _, entry := range wf.entries {
				scheduledMinute := minute
				claimMinute := minute
				if entry.timezone != "" {
					location, err := time.LoadLocation(entry.timezone)
					if err != nil {
						s.logger.Warn().Err(err).Str("file", wf.fileName).Str("timezone", entry.timezone).Msg("invalid schedule timezone")
						continue
					}
					scheduledMinute = minute.In(location)
					// Dedup on the wall-clock minute, not the UTC instant: on a
					// DST fall-back the same local minute recurs at two UTC
					// instants and must fire once. Reconstructing the civil
					// minute collapses both to one canonical claim.
					claimMinute = time.Date(scheduledMinute.Year(), scheduledMinute.Month(), scheduledMinute.Day(), scheduledMinute.Hour(), scheduledMinute.Minute(), 0, 0, location)
				}
				if !entry.cs.matches(scheduledMinute) {
					continue
				}
				claimKey := repoKey + "\x00" + wf.fileName + "\x00" + entry.cron + "\x00" + entry.timezone
				claimed, err := s.markScheduleFired(claimKey, claimMinute)
				if err != nil {
					s.logger.Error().Err(err).Str("repo", repoKey).Str("file", wf.fileName).Str("cron", entry.cron).Msg("failed to claim scheduled workflow firing")
					continue
				}
				if !claimed {
					continue
				}
				if err := s.fireScheduledWorkflow(repoKey, wf.fileName, wf.content, entry.cron); err != nil {
					// The claim was taken before firing; release it on a
					// transient failure so a later attempt can pick it up.
					if relErr := s.releaseScheduleFiring(claimKey, claimMinute); relErr != nil {
						s.logger.Error().Err(relErr).Str("repo", repoKey).Str("file", wf.fileName).Str("cron", entry.cron).Msg("failed to release scheduled workflow claim after firing error")
					}
				}
			}
		}
	}
	s.scheduleIndex.retain(live)
}

func scheduleInactive(repo *store.Repo, now time.Time) bool {
	if repo == nil || repo.Private {
		return false
	}
	lastActivity := repo.PushedAt
	if repo.UpdatedAt.After(lastActivity) {
		lastActivity = repo.UpdatedAt
	}
	return !lastActivity.IsZero() && now.Sub(lastActivity) >= 60*24*time.Hour
}

// markScheduleFired records a firing; false means this (key, minute) already fired.
func (s *Engine) markScheduleFired(key string, minute time.Time) (bool, error) {
	s.store.Mu.RLock()
	persist := s.store.Persist
	s.store.Mu.RUnlock()
	if persist != nil {
		return persist.ClaimScheduleFiring(key, minute)
	}
	s.scheduleFired.mu.Lock()
	defer s.scheduleFired.mu.Unlock()
	if s.scheduleFired.seen == nil {
		s.scheduleFired.seen = map[string]time.Time{}
	}
	if last, ok := s.scheduleFired.seen[key]; ok && last.Equal(minute) {
		return false, nil
	}
	s.scheduleFired.seen[key] = minute
	return true, nil
}

// releaseScheduleFiring undoes a markScheduleFired claim whose firing failed,
// so the occurrence can be retried.
func (s *Engine) releaseScheduleFiring(key string, minute time.Time) error {
	s.store.Mu.RLock()
	persist := s.store.Persist
	s.store.Mu.RUnlock()
	if persist != nil {
		return persist.ReleaseScheduleFiring(key, minute)
	}
	s.scheduleFired.mu.Lock()
	defer s.scheduleFired.mu.Unlock()
	if last, ok := s.scheduleFired.seen[key]; ok && last.Equal(minute) {
		delete(s.scheduleFired.seen, key)
	}
	return nil
}

// fireScheduledWorkflow submits one schedule-triggered run (no webhook on real
// GitHub). It returns a non-nil error only for a transient failure whose claim
// the caller should release for retry; permanent conditions (repo or git
// storage gone, ref that does not resolve) are logged and swallowed.
func (s *Engine) fireScheduledWorkflow(repoKey, fileName string, content []byte, cron string) error {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return nil
	}
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	ref := "refs/heads/" + defaultBranch

	parts := SplitRepoKeyParts(repoKey)
	stor := s.store.GetGitStorage(parts[0], parts[1])
	if stor == nil {
		// A concurrent persistence refresh can drop the storer between the scan
		// and here; ResolveRefSha would dereference a nil storer.
		s.logger.Error().Str("repo", repoKey).Str("cron", cron).Msg("scheduled workflow rejected because git storage is unavailable")
		return nil
	}

	payload := map[string]interface{}{
		"schedule":   cron,
		"repository": s.repoEventPayload(repo),
	}
	sha := ResolveRefSha(stor, ref)
	if sha == "0000000000000000000000000000000000000000" {
		s.logger.Error().
			Str("repo", repoKey).
			Str("ref", ref).
			Str("cron", cron).
			Msg("scheduled workflow rejected because the default-branch git ref did not resolve to a commit")
		return nil
	}
	meta := &WorkflowEventMeta{
		EventName: "schedule",
		Ref:       ref,
		Sha:       sha,
		Repo:      repoKey,
		Payload:   payload,
	}
	workflow, err := s.SubmitTriggeredWorkflow(fileName, content, meta)
	if err != nil {
		s.logger.Error().Err(err).Str("file", fileName).Str("cron", cron).Msg("failed to fire scheduled workflow")
		return err
	}
	s.logger.Info().
		Str("workflow_id", workflow.ID).
		Str("file", fileName).
		Str("cron", cron).
		Msg("workflow fired by schedule")
	return nil
}

// ── Cron parsing (POSIX 5-field, with JAN-DEC / SUN-SAT names) ──────

type cronSchedule struct {
	min, hour, dom, month, dow uint64
	domStar, dowStar           bool
}

// minimumInterval returns the shortest interval this cron's minute/hour masks
// can produce. Day/month filters only spread occurrences farther apart, so this
// conservatively enforces GitHub's five-minute floor.
func (cs *cronSchedule) minimumInterval() time.Duration {
	var minutes []int
	for hour := 0; hour < 24; hour++ {
		if cs.hour&(uint64(1)<<hour) == 0 {
			continue
		}
		for minute := 0; minute < 60; minute++ {
			if cs.min&(uint64(1)<<minute) != 0 {
				minutes = append(minutes, hour*60+minute)
			}
		}
	}
	if len(minutes) < 2 {
		return 24 * time.Hour
	}
	best := 24 * time.Hour
	for i, current := range minutes {
		next := minutes[(i+1)%len(minutes)]
		if i == len(minutes)-1 {
			next += 24 * 60
		}
		if gap := time.Duration(next-current) * time.Minute; gap < best {
			best = gap
		}
	}
	return best
}

var cronMonthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var cronDowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// parseCron parses a 5-field cron expression (minute hour day-of-month month
// day-of-week) with lists, ranges, steps, and month/day names.
func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}
	cs := &cronSchedule{}
	var err error
	if cs.min, _, err = parseCronField(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", expr, err)
	}
	if cs.hour, _, err = parseCronField(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", expr, err)
	}
	if cs.dom, cs.domStar, err = parseCronField(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q day-of-month: %w", expr, err)
	}
	if cs.month, _, err = parseCronField(fields[3], 1, 12, cronMonthNames); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", expr, err)
	}
	if cs.dow, cs.dowStar, err = parseCronField(fields[4], 0, 7, cronDowNames); err != nil {
		return nil, fmt.Errorf("cron %q day-of-week: %w", expr, err)
	}
	// 7 means Sunday, same as 0.
	if cs.dow&(1<<7) != 0 {
		cs.dow |= 1
		cs.dow &^= 1 << 7
	}
	return cs, nil
}

// parseCronField parses one field into a bitset. star reports whether the field
// was unrestricted ("*"), which matters for the day-of-month/day-of-week OR rule.
func parseCronField(field string, lo, hi int, names map[string]int) (bits uint64, star bool, err error) {
	resolve := func(tok string) (int, error) {
		if names != nil {
			if v, ok := names[strings.ToUpper(tok)]; ok {
				return v, nil
			}
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return 0, fmt.Errorf("invalid value %q", tok)
		}
		return n, nil
	}
	star = field == "*"
	for _, term := range strings.Split(field, ",") {
		step := 1
		if idx := strings.IndexByte(term, '/'); idx >= 0 {
			st, err := strconv.Atoi(term[idx+1:])
			if err != nil || st <= 0 {
				return 0, false, fmt.Errorf("invalid step in %q", term)
			}
			step = st
			term = term[:idx]
		}
		from, to := lo, hi
		switch {
		case term == "*":
			// full range
		case strings.Contains(term, "-"):
			parts := strings.SplitN(term, "-", 2)
			if from, err = resolve(parts[0]); err != nil {
				return 0, false, err
			}
			if to, err = resolve(parts[1]); err != nil {
				return 0, false, err
			}
		default:
			v, err := resolve(term)
			if err != nil {
				return 0, false, err
			}
			from, to = v, v
		}
		if from < lo || to > hi || from > to {
			return 0, false, fmt.Errorf("value out of range [%d-%d] in %q", lo, hi, term)
		}
		for v := from; v <= to; v += step {
			bits |= 1 << uint(v)
		}
	}
	if bits == 0 {
		return 0, false, fmt.Errorf("empty field %q", field)
	}
	return bits, star, nil
}

// matches reports whether the schedule fires at t (minute precision). Standard
// cron rule: when both day-of-month and day-of-week are restricted, either match suffices.
func (c *cronSchedule) matches(t time.Time) bool {
	if c.min&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if c.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if c.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}
