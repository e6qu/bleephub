package actions

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"gopkg.in/yaml.v3"
)

// TriggerDef is one event entry under a workflow's `on:` with its filters. A
// nil *TriggerDef (event listed without filters) matches every activity the
// event's defaults cover.
type TriggerDef struct {
	Branches       []string
	BranchesIgnore []string
	Tags           []string
	TagsIgnore     []string
	Paths          []string
	PathsIgnore    []string
	Types          []string
	// Workflows names the source workflows an `on.workflow_run` trigger listens
	// to; empty means every workflow in the repository.
	Workflows []string
	Inputs    map[string]*store.WorkflowInputDef
	// Schedules carries every cron line with its optional IANA timezone.
	Schedules []WorkflowScheduleDef
	// Outputs maps a workflow_call output name to its value template.
	Outputs map[string]string
	Secrets map[string]*WorkflowCallSecretDef
}

// WorkflowScheduleDef is one `on.schedule` entry.
type WorkflowScheduleDef struct {
	Cron     string
	Timezone string
}

type WorkflowCallSecretDef struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

type workflowCallOutputDef struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

// TriggerEvent is a concrete event occurrence to match against workflow `on:`
// definitions.
type TriggerEvent struct {
	Type   string // "push", "pull_request", ...
	Action string // activity type ("opened", ...) or repository_dispatch event_type
	Ref    string // full ref ("refs/heads/main", "refs/tags/v1")
	// ChangedFiles is valid only when ChangedFilesKnown; when unknown, path
	// filters pass open like GitHub does for new-branch pushes.
	ChangedFiles      []string
	ChangedFilesKnown bool
	// WorkflowName is the workflow whose run raised a `workflow_run` event,
	// filtered on by `on.workflow_run.workflows`.
	WorkflowName string
}

// defaultActivityTypes holds GitHub's documented default activity types for the
// events whose default is not "all types", used when a trigger declares no
// `types:` filter.
var defaultActivityTypes = map[string][]string{
	"pull_request":        {"opened", "synchronize", "reopened"},
	"pull_request_target": {"opened", "synchronize", "reopened"},
}

// ParseWorkflowOn extracts the `on:` definition from workflow YAML as event
// name → filters (nil when the event has no filters).
func ParseWorkflowOn(yamlContent []byte) (map[string]*TriggerDef, error) {
	var raw struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(yamlContent, &raw); err != nil {
		return nil, fmt.Errorf("parse workflow on: %w", err)
	}
	out := map[string]*TriggerDef{}
	node := &raw.On
	switch node.Kind {
	case 0:
		// No `on:` at all. An unquoted `on:` key parses as boolean true, so
		// callers see an empty map either way.
		return out, nil
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("parse workflow on: %w", err)
		}
		if s != "" && s != "true" {
			out[s] = nil
		}
		return out, nil
	case yaml.SequenceNode:
		var events []string
		if err := node.Decode(&events); err != nil {
			return nil, fmt.Errorf("parse workflow on: %w", err)
		}
		for _, e := range events {
			out[e] = nil
		}
		return out, nil
	case yaml.MappingNode:
		var m map[string]yaml.Node
		if err := node.Decode(&m); err != nil {
			return nil, fmt.Errorf("parse workflow on: %w", err)
		}
		for event, val := range m {
			td, err := parseTriggerDef(event, &val)
			if err != nil {
				return nil, fmt.Errorf("on.%s: %w", event, err)
			}
			out[event] = td
		}
		return out, nil
	default:
		return nil, fmt.Errorf("on: must be a string, list, or map")
	}
}

func parseTriggerDef(event string, node *yaml.Node) (*TriggerDef, error) {
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	if event == "schedule" {
		var entries []struct {
			Cron     string `yaml:"cron"`
			Timezone string `yaml:"timezone"`
		}
		if err := node.Decode(&entries); err != nil {
			return nil, fmt.Errorf("schedule must be a list of {cron: ...}: %w", err)
		}
		td := &TriggerDef{}
		for _, e := range entries {
			if e.Cron != "" {
				if e.Timezone != "" {
					if _, err := time.LoadLocation(e.Timezone); err != nil {
						return nil, fmt.Errorf("schedule timezone %q is not an IANA timezone: %w", e.Timezone, err)
					}
				}
				td.Schedules = append(td.Schedules, WorkflowScheduleDef{
					Cron:     e.Cron,
					Timezone: e.Timezone,
				})
			}
		}
		return td, nil
	}
	var raw struct {
		Branches       []string                           `yaml:"branches"`
		BranchesIgnore []string                           `yaml:"branches-ignore"`
		Tags           []string                           `yaml:"tags"`
		TagsIgnore     []string                           `yaml:"tags-ignore"`
		Paths          []string                           `yaml:"paths"`
		PathsIgnore    []string                           `yaml:"paths-ignore"`
		Types          []string                           `yaml:"types"`
		Workflows      []string                           `yaml:"workflows"`
		Inputs         map[string]*store.WorkflowInputDef `yaml:"inputs"`
		Outputs        map[string]*workflowCallOutputDef  `yaml:"outputs"`
		Secrets        map[string]*WorkflowCallSecretDef  `yaml:"secrets"`
	}
	if err := node.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid trigger filters: %w", err)
	}
	td := &TriggerDef{
		Branches:       raw.Branches,
		BranchesIgnore: raw.BranchesIgnore,
		Tags:           raw.Tags,
		TagsIgnore:     raw.TagsIgnore,
		Paths:          raw.Paths,
		PathsIgnore:    raw.PathsIgnore,
		Types:          raw.Types,
		Workflows:      raw.Workflows,
		Inputs:         raw.Inputs,
		Secrets:        raw.Secrets,
	}
	if len(raw.Outputs) > 0 {
		td.Outputs = make(map[string]string, len(raw.Outputs))
		for name, o := range raw.Outputs {
			if o != nil {
				td.Outputs[name] = o.Value
			}
		}
	}
	if len(td.Branches) > 0 && len(td.BranchesIgnore) > 0 {
		return nil, fmt.Errorf("branches and branches-ignore cannot be used together")
	}
	if len(td.Tags) > 0 && len(td.TagsIgnore) > 0 {
		return nil, fmt.Errorf("tags and tags-ignore cannot be used together")
	}
	if len(td.Paths) > 0 && len(td.PathsIgnore) > 0 {
		return nil, fmt.Errorf("paths and paths-ignore cannot be used together")
	}
	return td, nil
}

// WorkflowTriggersOn reports whether a workflow's parsed `on:` fires for a
// concrete event. An event absent from `on:` never fires.
func WorkflowTriggersOn(on map[string]*TriggerDef, ev TriggerEvent) bool {
	td, ok := on[ev.Type]
	if !ok {
		return false
	}

	// Explicit types list, else the event's documented default; events with no
	// default match any action.
	types := defaultActivityTypes[ev.Type]
	if td != nil && len(td.Types) > 0 {
		types = td.Types
	}
	if len(types) > 0 && ev.Action != "" {
		matched := false
		for _, t := range types {
			if t == ev.Action {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if td == nil {
		return true
	}

	// Without `on.workflow_run.workflows` a listener fires on every run in the
	// repository, including its own; the filter is what scopes it.
	if len(td.Workflows) > 0 {
		matched := false
		for _, name := range td.Workflows {
			if name == ev.WorkflowName {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Branch/tag filters apply to the pushed ref for push events; for
	// pull_request events ev.Ref carries the base ref, which the branches
	// filter applies to.
	isTag := strings.HasPrefix(ev.Ref, "refs/tags/")
	branchName := strings.TrimPrefix(ev.Ref, "refs/heads/")
	tagName := strings.TrimPrefix(ev.Ref, "refs/tags/")
	hasBranchFilter := len(td.Branches) > 0 || len(td.BranchesIgnore) > 0
	hasTagFilter := len(td.Tags) > 0 || len(td.TagsIgnore) > 0

	if isTag {
		// A trigger filtered to branches only never matches tags.
		if hasBranchFilter && !hasTagFilter {
			return false
		}
		if len(td.Tags) > 0 && !filterPatternsMatch(td.Tags, tagName) {
			return false
		}
		if len(td.TagsIgnore) > 0 && filterPatternsMatch(td.TagsIgnore, tagName) {
			return false
		}
	} else if ev.Ref != "" {
		if hasTagFilter && !hasBranchFilter {
			return false
		}
		if len(td.Branches) > 0 && !filterPatternsMatch(td.Branches, branchName) {
			return false
		}
		if len(td.BranchesIgnore) > 0 && filterPatternsMatch(td.BranchesIgnore, branchName) {
			return false
		}
	}

	// Path filters pass open when the diff is unknown, matching GitHub.
	if len(td.Paths) > 0 && ev.ChangedFilesKnown {
		any := false
		for _, f := range ev.ChangedFiles {
			if filterPatternsMatch(td.Paths, f) {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	if len(td.PathsIgnore) > 0 && ev.ChangedFilesKnown {
		allIgnored := true
		for _, f := range ev.ChangedFiles {
			if !filterPatternsMatch(td.PathsIgnore, f) {
				allIgnored = false
				break
			}
		}
		if allIgnored && len(ev.ChangedFiles) > 0 {
			return false
		}
	}

	return true
}

// filterPatternsMatch evaluates GitHub filter patterns in order: a matching
// pattern includes the value, a later matching `!pattern` excludes it, and a
// yet-later positive match re-includes it.
func filterPatternsMatch(patterns []string, value string) bool {
	matched := false
	for _, p := range patterns {
		if neg, ok := strings.CutPrefix(p, "!"); ok {
			if matched && FilterPatternMatch(neg, value) {
				matched = false
			}
			continue
		}
		if !matched && FilterPatternMatch(p, value) {
			matched = true
		}
	}
	return matched
}

// filterPatternCache memoizes compiled filter patterns; trigger evaluation
// runs concurrently across pushed refs, so it must be a sync.Map.
var filterPatternCache sync.Map // pattern string → *regexp.Regexp

// FilterPatternMatch matches one GitHub filter pattern: `*` (any except '/'),
// `**` (any), `?`/`+` (zero-or-one / one-or-more of the preceding token),
// `[...]` character classes.
func FilterPatternMatch(pattern, value string) bool {
	re, err := compileFilterPattern(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func compileFilterPattern(pattern string) (*regexp.Regexp, error) {
	if cached, ok := filterPatternCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	var sb strings.Builder
	sb.WriteString("^(?:")
	lastAtom := ""
	emit := func(atom string) {
		sb.WriteString(lastAtom)
		lastAtom = atom
	}
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				emit("(?s:.*)")
				i++
			} else {
				emit("[^/]*")
			}
		case '?':
			if lastAtom == "" {
				emit(regexp.QuoteMeta("?"))
			} else {
				lastAtom = "(?:" + lastAtom + ")?"
			}
		case '+':
			if lastAtom == "" {
				emit(regexp.QuoteMeta("+"))
			} else {
				lastAtom = "(?:" + lastAtom + ")+"
			}
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end <= 1 {
				emit(regexp.QuoteMeta(string(c)))
				continue
			}
			emit(pattern[i : i+end+1])
			i += end
		default:
			emit(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString(lastAtom)
	sb.WriteString(")$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, err
	}
	actual, _ := filterPatternCache.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp), nil
}

// ChangedFilesBetween computes the paths touched between two commits. ok is
// false when the diff cannot be computed (zero/unknown shas), which makes path
// filters pass open like real GitHub.
func ChangedFilesBetween(stor gitStorage.Storer, beforeSha, afterSha string) (files []string, ok bool) {
	const zeroSha = "0000000000000000000000000000000000000000"
	if beforeSha == "" || afterSha == "" || beforeSha == zeroSha || afterSha == zeroSha {
		return nil, false
	}
	beforeCommit, err := object.GetCommit(stor, plumbing.NewHash(beforeSha))
	if err != nil {
		return nil, false
	}
	afterCommit, err := object.GetCommit(stor, plumbing.NewHash(afterSha))
	if err != nil {
		return nil, false
	}
	beforeTree, err := beforeCommit.Tree()
	if err != nil {
		return nil, false
	}
	afterTree, err := afterCommit.Tree()
	if err != nil {
		return nil, false
	}
	changes, err := object.DiffTree(beforeTree, afterTree)
	if err != nil {
		return nil, false
	}
	seen := map[string]bool{}
	for _, ch := range changes {
		for _, name := range []string{ch.From.Name, ch.To.Name} {
			if name != "" && !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
	}
	return files, true
}
