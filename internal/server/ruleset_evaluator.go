package bleephub

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// evaluateRulesetsForRefWrite evaluates and records every applicable rule,
// returning the first active-rule refusal. Evaluate-mode failures are recorded
// in the suite but never block the ref update.
func (s *Server) evaluateRulesetsForRefWrite(ctx context.Context, repo *store.Repo, stor storer.Storer, ref plumbing.ReferenceName, kind refWriteKind, target plumbing.Hash) string {
	rulesets := s.store.ApplicableRulesets(repo, ref.String())
	if len(rulesets) == 0 {
		return ""
	}
	actor := ghUserFromContext(ctx)
	before := plumbing.ZeroHash.String()
	if current, err := stor.Reference(ref); err == nil && current.Type() == plumbing.HashReference {
		before = current.Hash().String()
	}
	after := target.String()
	if target.IsZero() {
		after = plumbing.ZeroHash.String()
	}

	activeFailed, evaluateFailed, hasEvaluate := false, false, false
	activeBypassed := false
	evaluations := make([]store.RulesetEvaluation, 0)
	firstRefusal := ""
	for i := range rulesets {
		rs := &rulesets[i]
		bypassed := rulesetActorBypasses(rs, actor)
		if bypassed && rs.Enforcement == "active" && len(rs.Rules) > 0 {
			activeBypassed = true
		}
		if rs.Enforcement == "evaluate" {
			hasEvaluate = true
		}
		for _, rule := range rs.Rules {
			passed, detail := s.evaluateRulesetRule(repo, stor, ref, kind, target, rule)
			if bypassed {
				passed, detail = true, ""
			}
			evaluation := store.RulesetEvaluation{
				RuleSource: store.RulesetEvaluationSource{
					Type: "ruleset",
					ID:   intPointer(rs.ID),
					Name: stringPointer(rs.Name),
				},
				Enforcement: rs.Enforcement,
				Result:      boolRuleResult(passed),
				RuleType:    rule.Type,
				Details:     optionalStringPointer(detail),
			}
			evaluations = append(evaluations, evaluation)
			if passed {
				continue
			}
			if rs.Enforcement == "evaluate" {
				evaluateFailed = true
				continue
			}
			activeFailed = true
			if firstRefusal == "" {
				firstRefusal = detail
			}
		}
	}

	result := "pass"
	if activeFailed {
		result = "fail"
	} else if activeBypassed {
		result = "bypass"
	}
	var evaluationResult *string
	if hasEvaluate {
		value := "pass"
		if activeFailed || evaluateFailed {
			value = "fail"
		}
		evaluationResult = &value
	}
	s.store.RecordRulesetSuite(repo, actor, ref.String(), before, after, result, evaluationResult, evaluations, s.currentTime())
	return firstRefusal
}

func (s *Server) evaluateRulesetRule(repo *store.Repo, stor storer.Storer, ref plumbing.ReferenceName, kind refWriteKind, target plumbing.Hash, rule store.Rule) (bool, string) {
	short := ref.Short()
	switch rule.Type {
	case "creation":
		if kind == refCreation {
			return false, "Cannot create ref " + short + " because a repository ruleset prohibits creation."
		}
	case "update":
		if kind == refFastForward || kind == refForcePush {
			return false, "Cannot update ref " + short + " because a repository ruleset prohibits updates."
		}
	case "deletion":
		if kind == refDeletion {
			return false, "Cannot delete ref " + short + " because a repository ruleset prohibits deletion."
		}
	case "non_fast_forward":
		if kind == refForcePush {
			return false, "Cannot force-push to " + short + " because a repository ruleset prohibits non-fast-forward updates."
		}
	case "pull_request":
		if kind != refDeletion {
			return false, "Changes must be made through a pull request."
		}
	case "required_linear_history":
		for _, commit := range rulesetAddedCommits(stor, ref, target) {
			if commit.NumParents() > 1 {
				return false, "Cannot push a merge commit to " + short + ", which requires linear history."
			}
		}
	case "required_signatures":
		for _, commit := range rulesetAddedCommits(stor, ref, target) {
			if commit.PGPSignature == "" {
				return false, "Commits must have verified signatures."
			}
		}
	case "required_status_checks":
		if target.IsZero() {
			break
		}
		if missing := s.missingRulesetStatusChecks(repo, target.String(), rule.Parameters); len(missing) > 0 {
			return false, fmt.Sprintf("Required status check %q is expected.", missing[0])
		}
	case "branch_name_pattern", "tag_name_pattern":
		if ok, detail := rulesetPatternMatches(short, rule.Parameters); !ok {
			return false, detail
		}
	case "commit_message_pattern", "commit_author_email_pattern", "committer_email_pattern":
		for _, commit := range rulesetAddedCommits(stor, ref, target) {
			value := commit.Message
			switch rule.Type {
			case "commit_author_email_pattern":
				value = commit.Author.Email
			case "committer_email_pattern":
				value = commit.Committer.Email
			}
			if ok, detail := rulesetPatternMatches(value, rule.Parameters); !ok {
				return false, detail
			}
		}
	}
	return true, ""
}

func rulesetAddedCommits(stor storer.Storer, ref plumbing.ReferenceName, target plumbing.Hash) []*object.Commit {
	if stor == nil || target.IsZero() {
		return nil
	}
	head, err := object.GetCommit(stor, target)
	if err != nil {
		return nil
	}
	seen := map[plumbing.Hash]bool{}
	if current, err := stor.Reference(ref); err == nil {
		if base, err := object.GetCommit(stor, current.Hash()); err == nil {
			_ = object.NewCommitPreorderIter(base, nil, nil).ForEach(func(commit *object.Commit) error {
				seen[commit.Hash] = true
				return nil
			})
		}
	}
	var added []*object.Commit
	_ = object.NewCommitPreorderIter(head, seen, nil).ForEach(func(commit *object.Commit) error {
		added = append(added, commit)
		return nil
	})
	return added
}

func (s *Server) missingRulesetStatusChecks(repo *store.Repo, sha string, parameters map[string]interface{}) []string {
	required := rulesetRequiredStatusContexts(parameters)
	if len(required) == 0 {
		return nil
	}
	green := map[string]bool{}
	for _, run := range s.store.ListCheckRunsForCommit(repo.FullName, sha, "", "", 0) {
		if _, seen := green[run.Name]; seen {
			continue
		}
		green[run.Name] = run.Status == "completed" &&
			(run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped")
	}
	_, _, statuses := s.store.CommitStatuses.Combined(repo.FullName, sha)
	for _, status := range statuses {
		if _, seen := green[status.Context]; !seen {
			green[status.Context] = status.State == "success"
		}
	}
	var missing []string
	for _, context := range required {
		if !green[context] {
			missing = append(missing, context)
		}
	}
	return missing
}

func rulesetRequiredStatusContexts(parameters map[string]interface{}) []string {
	var contexts []string
	appendItem := func(item interface{}) {
		switch value := item.(type) {
		case string:
			contexts = append(contexts, value)
		case map[string]interface{}:
			if context, _ := value["context"].(string); context != "" {
				contexts = append(contexts, context)
			}
		case map[string]string:
			if context := value["context"]; context != "" {
				contexts = append(contexts, context)
			}
		}
	}
	switch raw := parameters["required_status_checks"].(type) {
	case []interface{}:
		for _, item := range raw {
			appendItem(item)
		}
	case []map[string]interface{}:
		for _, item := range raw {
			appendItem(item)
		}
	case []map[string]string:
		for _, item := range raw {
			appendItem(item)
		}
	case []string:
		for _, item := range raw {
			appendItem(item)
		}
	}
	return contexts
}

func rulesetPatternMatches(value string, parameters map[string]interface{}) (bool, string) {
	pattern, _ := parameters["pattern"].(string)
	operator, _ := parameters["operator"].(string)
	negate, _ := parameters["negate"].(bool)
	if pattern == "" {
		return true, ""
	}
	var matched bool
	switch operator {
	case "starts_with":
		matched = strings.HasPrefix(value, pattern)
	case "ends_with":
		matched = strings.HasSuffix(value, pattern)
	case "contains":
		matched = strings.Contains(value, pattern)
	case "regex":
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return false, "The configured ruleset pattern is invalid."
		}
		matched = expression.MatchString(value)
	default:
		matched = value == pattern
	}
	if negate {
		matched = !matched
	}
	if !matched {
		return false, fmt.Sprintf("%q does not satisfy the configured %s pattern.", value, operator)
	}
	return true, ""
}

func rulesetActorBypasses(rs *store.Ruleset, actor *store.User) bool {
	if actor == nil {
		return false
	}
	for _, bypass := range rs.BypassActors {
		if bypass.ActorType == "User" && bypass.ActorID == actor.ID && bypass.BypassMode == "always" {
			return true
		}
	}
	return false
}

func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }

func optionalStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func boolRuleResult(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}
