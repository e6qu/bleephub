package actions

import (
	"testing"
	"time"
)

// FuzzParseCron feeds arbitrary schedule expressions to the cron parser. A
// malformed expression must return an error, and any expression that parses
// must evaluate matches() at an arbitrary minute without panicking (out-of-range
// fields, empty lists, bad steps, name tables).
func FuzzParseCron(f *testing.F) {
	f.Add("* * * * *")
	f.Add("")
	f.Add("*/5 * * * *")
	f.Add("0 0 1 JAN MON")
	f.Add("99 99 99 99 99")
	f.Add("*/0 * * * *")
	f.Add("1-5/2 0-23 * * 1-5")
	f.Add("@daily")
	f.Fuzz(func(t *testing.T, expr string) {
		cs, err := parseCron(expr)
		if err != nil || cs == nil {
			return
		}
		cs.matches(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		cs.matches(time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC))
	})
}

// FuzzParseWorkflowOn feeds arbitrary workflow YAML to the trigger parser that
// decides whether an event fires a workflow. Attacker-authored .github/workflows
// files reach this; a parse error is fine, a panic is not.
func FuzzParseWorkflowOn(f *testing.F) {
	f.Add([]byte("on: push"))
	f.Add([]byte("on: [push, pull_request]"))
	f.Add([]byte("on:\n  schedule:\n    - cron: '* * * * *'"))
	f.Add([]byte("on:\n  push:\n    branches: ['**']\n    paths: ['**']"))
	f.Add([]byte(""))
	f.Add([]byte("on: {workflow_run: {workflows: [a], types: [completed]}}"))
	f.Fuzz(func(t *testing.T, content []byte) {
		on, err := ParseWorkflowOn(content)
		if err != nil {
			return
		}
		_ = WorkflowTriggersOn(on, TriggerEvent{Type: "push", Action: "opened", Ref: "refs/heads/main"})
	})
}
