package bleephub

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// scheduleFiredKeys dedupes cron firings: a (repo, file, cron) tuple
// fires at most once per minute even if the ticker drifts.
type scheduleFiredKeys struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// startScheduleDispatcher launches the minute-aligned loop that fires
// `on: schedule:` workflows, the server-side clock real GitHub runs for
// cron triggers.
func (s *Server) startScheduleDispatcher(ctx context.Context) {
	s.goBackground(func() {
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
			s.fireDueSchedules(s.currentTime())
		}
	})
}

// fireDueSchedules triggers every schedule-bearing workflow from each
// repository's explicit default branch whose cron matches the given minute.
// Separated from the ticker so tests drive it with a fixed clock.
func (s *Server) fireDueSchedules(now time.Time) {
	minute := now.Truncate(time.Minute)
	if err := s.store.RefreshFromPersistenceIfStale(); err != nil {
		s.logger.Error().Err(err).Msg("scheduled workflow scan could not refresh shared state")
		return
	}

	s.store.mu.RLock()
	repoKeys := make([]string, 0, len(s.store.ReposByName))
	for key := range s.store.ReposByName {
		repoKeys = append(repoKeys, key)
	}
	s.store.mu.RUnlock()

	for _, repoKey := range repoKeys {
		repo := s.store.GetRepoByFullName(repoKey)
		if repo == nil {
			continue
		}
		parts := splitRepoKeyParts(repoKey)
		stor := s.store.GetGitStorage(parts[0], parts[1])
		if stor == nil {
			continue
		}
		defaultBranch := repo.DefaultBranch
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		definitionRef := plumbing.NewBranchReferenceName(defaultBranch).String()
		for name, content := range listWorkflowFilesAtRef(stor, definitionRef) {
			on, err := ParseWorkflowOn(content)
			if err != nil {
				s.logger.Warn().Err(err).Str("repo", repoKey).Str("file", name).Msg("invalid scheduled workflow")
				continue
			}
			sched := on["schedule"]
			if sched == nil || len(sched.Schedules) == 0 {
				continue
			}
			if scheduleInactive(repo, minute) {
				s.store.SetWorkflowFileState(repoKey, ".github/workflows/"+name, "disabled_inactivity")
				continue
			}
			if s.workflowFileDisabled(repoKey, name) {
				continue
			}
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
				scheduledMinute := minute
				if entry.Timezone != "" {
					location, err := time.LoadLocation(entry.Timezone)
					if err != nil {
						s.logger.Warn().Err(err).Str("file", name).Str("timezone", entry.Timezone).Msg("invalid schedule timezone")
						continue
					}
					scheduledMinute = minute.In(location)
				}
				if !cs.matches(scheduledMinute) {
					continue
				}
				claimKey := repoKey + "\x00" + name + "\x00" + entry.Cron + "\x00" + entry.Timezone
				claimed, err := s.markScheduleFired(claimKey, minute)
				if err != nil {
					s.logger.Error().Err(err).Str("repo", repoKey).Str("file", name).Str("cron", entry.Cron).Msg("failed to claim scheduled workflow firing")
					continue
				}
				if !claimed {
					continue
				}
				s.fireScheduledWorkflow(repoKey, name, content, entry.Cron)
			}
		}
	}
}

func scheduleInactive(repo *Repo, now time.Time) bool {
	if repo == nil || repo.Private {
		return false
	}
	lastActivity := repo.PushedAt
	if repo.UpdatedAt.After(lastActivity) {
		lastActivity = repo.UpdatedAt
	}
	return !lastActivity.IsZero() && now.Sub(lastActivity) >= 60*24*time.Hour
}

// markScheduleFired records a firing; false means this (key, minute)
// already fired.
func (s *Server) markScheduleFired(key string, minute time.Time) (bool, error) {
	s.store.mu.RLock()
	persist := s.store.persist
	s.store.mu.RUnlock()
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

// fireScheduledWorkflow submits one schedule-triggered run. The schedule
// event has no webhook delivery on real GitHub — it only starts the run;
// its payload carries the matching cron line.
func (s *Server) fireScheduledWorkflow(repoKey, fileName string, content []byte, cron string) {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return
	}
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	ref := "refs/heads/" + defaultBranch

	parts := splitRepoKeyParts(repoKey)
	stor := s.store.GetGitStorage(parts[0], parts[1])

	payload := map[string]interface{}{
		"schedule":   cron,
		"repository": repoPayload(repo),
	}
	sha := resolveRefSha(stor, ref)
	if sha == "0000000000000000000000000000000000000000" {
		s.logger.Error().
			Str("repo", repoKey).
			Str("ref", ref).
			Str("cron", cron).
			Msg("scheduled workflow rejected because the default-branch git ref did not resolve to a commit")
		return
	}
	meta := &WorkflowEventMeta{
		EventName: "schedule",
		Ref:       ref,
		Sha:       sha,
		Repo:      repoKey,
		Payload:   payload,
	}
	workflow, err := s.submitTriggeredWorkflow(fileName, content, meta)
	if err != nil {
		s.logger.Error().Err(err).Str("file", fileName).Str("cron", cron).Msg("failed to fire scheduled workflow")
		return
	}
	s.logger.Info().
		Str("workflow_id", workflow.ID).
		Str("file", fileName).
		Str("cron", cron).
		Msg("workflow fired by schedule")
}

// ── Cron parsing (POSIX 5-field, with JAN-DEC / SUN-SAT names) ──────

type cronSchedule struct {
	min, hour, dom, month, dow uint64
	domStar, dowStar           bool
}

// minimumInterval returns the shortest interval this cron's minute/hour masks
// can produce. Day/month filters can only make occurrences farther apart, so
// this is a conservative enforcement of GitHub's five-minute floor.
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

// parseCron parses a 5-field cron expression (minute hour day-of-month
// month day-of-week) with lists, ranges, steps, and month/day names.
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

// parseCronField parses one field into a bitset. star reports whether
// the field was unrestricted ("*"), which matters for the day-of-month /
// day-of-week OR rule.
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

// matches reports whether the schedule fires at t (minute precision).
// Standard cron rule: when both day-of-month and day-of-week are
// restricted, either matching suffices.
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
