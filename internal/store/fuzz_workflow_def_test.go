package store

import "testing"

// FuzzParseWorkflow feeds arbitrary YAML to the workflow-definition parser. A
// workflow file is attacker-authored content committed to a repository, so the
// parser must reject malformed input with an error rather than panic, and a
// definition that parses must expose its jobs/steps without panicking.
func FuzzParseWorkflow(f *testing.F) {
	f.Add([]byte("name: ci\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi"))
	f.Add([]byte(""))
	f.Add([]byte("on: [push]"))
	f.Add([]byte("permissions: read-all\ndefaults:\n  run:\n    shell: bash"))
	f.Add([]byte("jobs:\n  a:\n    strategy:\n      matrix:\n        x: [1,2,3]\n    steps: []"))
	f.Add([]byte("run-name: ${{ github.actor }}\non: workflow_dispatch\njobs: {}"))
	f.Add([]byte("jobs:\n 0:")) // regression: a null job body must not panic normalizeJob
	f.Fuzz(func(t *testing.T, content []byte) {
		def, err := ParseWorkflow(content)
		if err != nil || def == nil {
			return
		}
		for _, job := range def.Jobs {
			_ = job.Steps
			_ = job.Permissions
		}
	})
}
