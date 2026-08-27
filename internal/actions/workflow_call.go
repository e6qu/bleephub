package actions

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// maxWorkflowCallDepth bounds reusable-workflow nesting; GitHub allows four
// levels (the caller plus three nested calls).
const maxWorkflowCallDepth = 4

// expandReusableWorkflows replaces every `uses:` job in def with the called
// workflow's jobs plus two synthetic server-completed nodes:
//
//	<caller>/__call  — gate: carries the caller's needs/if, resolves `with:`
//	<caller>/<k>     — each called job (display "caller / called"); roots need the gate
//	<caller>         — collector: needs every called job, computes workflow_call
//	                   outputs, serves downstream `needs.<caller>` edges
//
// repoKey resolves "./" references; depth starts at 1 for the top-level workflow.
func (s *Engine) expandReusableWorkflows(def *store.WorkflowDef, repoKey string, depth int) (*store.WorkflowDef, error) {
	hasCalls := false
	for _, jd := range def.Jobs {
		if jd.Uses != "" {
			hasCalls = true
			break
		}
	}
	if !hasCalls {
		return def, nil
	}
	if depth >= maxWorkflowCallDepth {
		return nil, fmt.Errorf("reusable workflows nested deeper than %d levels", maxWorkflowCallDepth)
	}

	out := &store.WorkflowDef{
		Name:        def.Name,
		RunName:     def.RunName,
		Env:         def.Env,
		Permissions: def.Permissions,
		Defaults:    def.Defaults,
		Concurrency: def.Concurrency,
		Jobs:        make(map[string]*store.JobDef, len(def.Jobs)),
	}
	for key, jd := range def.Jobs {
		if jd.Uses == "" {
			out.Jobs[key] = jd
			continue
		}
		if err := s.expandOneCall(out, key, jd, repoKey, depth); err != nil {
			return nil, fmt.Errorf("job %q: %w", key, err)
		}
	}
	if err := ValidateJobGraph(out); err != nil {
		return nil, fmt.Errorf("reusable-workflow expansion produced an invalid graph: %w", err)
	}
	return out, nil
}

// expandOneCall expands a single `uses:` job into out.
func (s *Engine) expandOneCall(out *store.WorkflowDef, callerKey string, caller *store.JobDef, repoKey string, depth int) error {
	calledRepo, calledPath, calledYAML, err := s.resolveCalledWorkflow(repoKey, caller.Uses)
	if err != nil {
		return err
	}

	calledOn, err := ParseWorkflowOn(calledYAML)
	if err != nil {
		return fmt.Errorf("called workflow %s: %w", calledPath, err)
	}
	callDef, isCallable := calledOn["workflow_call"]
	if !isCallable {
		return fmt.Errorf("called workflow %s does not declare on: workflow_call", calledPath)
	}
	var inputDefs map[string]*store.WorkflowInputDef
	var outputDefs map[string]string
	var secretDefs map[string]*WorkflowCallSecretDef
	if callDef != nil {
		inputDefs = callDef.Inputs
		outputDefs = callDef.Outputs
		secretDefs = callDef.Secrets
	}

	// GitHub rejects these at run creation, so validate here.
	for name := range caller.With {
		if _, ok := inputDefs[name]; !ok {
			return fmt.Errorf("called workflow %s does not define input %q", calledPath, name)
		}
	}
	for name, def := range inputDefs {
		if def != nil && def.Required && def.Default == nil {
			if _, ok := caller.With[name]; !ok {
				return fmt.Errorf("called workflow %s requires input %q", calledPath, name)
			}
		}
	}
	if !caller.SecretsInherit {
		for name, def := range secretDefs {
			if def != nil && def.Required {
				if _, ok := caller.SecretsMap[name]; !ok {
					return fmt.Errorf("called workflow %s requires secret %q", calledPath, name)
				}
			}
		}
	}

	calledDef, err := store.ParseWorkflow(calledYAML)
	if err != nil {
		return fmt.Errorf("called workflow %s: %w", calledPath, err)
	}
	calledDef = ExpandMatrixJobs(calledDef)
	calledDef, err = s.expandReusableWorkflows(calledDef, calledRepo, depth+1)
	if err != nil {
		return fmt.Errorf("called workflow %s: %w", calledPath, err)
	}

	// Bindings already on the called jobs belong to calls it makes itself; make
	// their roots children of this call so secret resolution narrows outside-in.
	// Collect before `binding` exists so the chain can never close on itself.
	nestedRoots := map[*store.WorkflowCallBinding]bool{}
	for _, cjd := range calledDef.Jobs {
		if cjd.Call != nil {
			nestedRoots[callBindingRoot(cjd.Call)] = true
		}
	}

	binding := &store.WorkflowCallBinding{
		CallerKey:      callerKey,
		CalledPath:     calledPath,
		CalledRepo:     calledRepo,
		With:           caller.With,
		InputDefs:      inputDefs,
		SecretsMap:     caller.SecretsMap,
		SecretsInherit: caller.SecretsInherit,
		OutputDefs:     outputDefs,
	}
	for root := range nestedRoots {
		root.Parent = binding
	}

	callerDisplay := caller.Name
	if callerDisplay == "" {
		callerDisplay = callerKey
	}

	gateKey := callerKey + "/__call"
	out.Jobs[gateKey] = &store.JobDef{
		Name:            callerDisplay,
		Needs:           caller.Needs,
		If:              caller.If,
		Env:             caller.Env, // carries __matrix_* for `with:`
		Call:            binding,
		ServerCompleted: true,
		CallRole:        "gate",
	}

	collectorNeeds := []string{gateKey}
	for k, cjd := range calledDef.Jobs {
		childKey := callerKey + "/" + k
		child := *cjd
		childDisplay := cjd.Name
		if childDisplay == "" {
			childDisplay = k
		}
		child.Name = callerDisplay + " / " + childDisplay
		// Internal needs point at sibling called jobs; roots wait on the gate.
		if len(cjd.Needs) == 0 {
			child.Needs = []string{gateKey}
		} else {
			prefixed := make([]string, 0, len(cjd.Needs))
			for _, n := range cjd.Needs {
				prefixed = append(prefixed, callerKey+"/"+n)
			}
			child.Needs = prefixed
		}
		if child.Call == nil {
			child.Call = binding
		}
		child.Env = MergedCallEnvironment(calledDef.Env, child.Env)
		out.Jobs[childKey] = &child
		binding.CalledJobKeys = append(binding.CalledJobKeys, childKey)
		collectorNeeds = append(collectorNeeds, childKey)
	}

	out.Jobs[callerKey] = &store.JobDef{
		Name:            callerDisplay,
		Needs:           collectorNeeds,
		Call:            binding,
		ServerCompleted: true,
		CallRole:        "collector",
	}
	return nil
}

// callBindingRoot walks a binding chain to its outermost call.
func callBindingRoot(binding *store.WorkflowCallBinding) *store.WorkflowCallBinding {
	for binding.Parent != nil {
		binding = binding.Parent
	}
	return binding
}

// resolveCalledWorkflow loads the YAML a `uses:` reference points at:
// "./..." resolves in the caller's repo at HEAD; "owner/repo/...@ref" resolves
// in another repo on this server at the given ref.
func (s *Engine) resolveCalledWorkflow(repoKey, uses string) (calledRepo, calledPath string, yaml []byte, err error) {
	if strings.HasPrefix(uses, "./") {
		if repoKey == "" {
			return "", "", nil, fmt.Errorf("local reusable workflow %q needs a repository context", uses)
		}
		path := strings.TrimPrefix(uses, "./")
		parts := SplitRepoKeyParts(repoKey)
		stor := s.store.GetGitStorage(parts[0], parts[1])
		if stor == nil {
			return "", "", nil, fmt.Errorf("no git storage for %s", repoKey)
		}
		content, err := gitFileAtRef(stor, "", path)
		if err != nil {
			return "", "", nil, fmt.Errorf("reusable workflow %s not found in %s: %w", path, repoKey, err)
		}
		return repoKey, path, content, nil
	}

	nameWithOwner, path, ref, isLocal := store.ParseActionRef(uses)
	if isLocal || nameWithOwner == "" || path == "" {
		return "", "", nil, fmt.Errorf("invalid reusable workflow reference %q", uses)
	}
	parts := SplitRepoKeyParts(nameWithOwner)
	stor := s.store.GetGitStorage(parts[0], parts[1])
	if stor == nil {
		return "", "", nil, fmt.Errorf("repository %s not found on this server", nameWithOwner)
	}
	content, err := gitFileAtRef(stor, ref, path)
	if err != nil {
		return "", "", nil, fmt.Errorf("reusable workflow %s@%s not found in %s: %w", path, ref, nameWithOwner, err)
	}
	return nameWithOwner, path, content, nil
}

// gitFileAtRef reads one file at a ref (branch, tag, or sha); empty ref means HEAD.
func gitFileAtRef(stor gitStorage.Storer, ref, path string) ([]byte, error) {
	var hash plumbing.Hash
	if ref == "" {
		sha := ResolveRefSha(stor, "")
		if sha == "0000000000000000000000000000000000000000" {
			return nil, fmt.Errorf("HEAD did not resolve to a commit")
		}
		hash = plumbing.NewHash(sha)
	} else {
		candidates := []plumbing.ReferenceName{
			plumbing.ReferenceName(ref),
			plumbing.NewBranchReferenceName(ref),
			plumbing.NewTagReferenceName(ref),
		}
		for _, name := range candidates {
			if r, err := stor.Reference(name); err == nil {
				hash = r.Hash()
				break
			}
		}
		if hash.IsZero() && len(ref) == 40 {
			hash = plumbing.NewHash(ref)
		}
		if hash.IsZero() {
			return nil, fmt.Errorf("ref %q not found", ref)
		}
	}

	commit, err := object.GetCommit(stor, hash)
	if err != nil {
		return nil, fmt.Errorf("resolve commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return nil, fmt.Errorf("file %q not found", path)
	}
	blob, err := object.GetBlob(stor, entry.Hash)
	if err != nil {
		return nil, err
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// completeServerJobLocked finishes a synthetic gate/collector node in place.
// Callers hold the store write lock.
func (s *Engine) completeServerJobLocked(wf *store.Workflow, wfJob *store.WorkflowJob) {
	jd := wfJob.Def
	switch jd.CallRole {
	case "gate":
		if !s.ResolveCallInputsLocked(wf, wfJob) {
			wfJob.Status = store.JobStatusCompleted
			wfJob.Result = store.ResultFailure
			break
		}
		wfJob.Status = store.JobStatusCompleted
		wfJob.Result = store.ResultSuccess
	case "collector":
		s.collectCallOutputsLocked(wf, wfJob)
	default:
		wfJob.Status = store.JobStatusCompleted
		wfJob.Result = store.ResultSuccess
	}
	wfJob.CompletedAt = time.Now()
	if wfJob.StartedAt.IsZero() {
		wfJob.StartedAt = wfJob.CompletedAt
	}
}

// ResolveCallInputsLocked evaluates the caller's `with:` templates against the
// contexts available to jobs.<id>.with (github, needs, vars, inputs, matrix),
// then applies the called workflow's defaults and typing.
func (s *Engine) ResolveCallInputsLocked(wf *store.Workflow, gate *store.WorkflowJob) bool {
	binding := gate.Def.Call
	if binding == nil {
		return true
	}
	ctx, err := s.jobExprContext(wf, gate)
	if err != nil {
		s.logger.Warn().Err(err).Str("workflow", binding.CalledPath).
			Msg("workflow_call input context failed")
		return false
	}
	if len(gate.MatrixValues) > 0 {
		matrixCtx := make(map[string]interface{}, len(gate.MatrixValues))
		for k, v := range gate.MatrixValues {
			matrixCtx[k] = v
		}
		ctx.Contexts["matrix"] = matrixCtx
	}

	resolved := make(map[string]interface{}, len(binding.InputDefs))
	for name, tmpl := range binding.With {
		val, err := EvalTemplate(tmpl, ctx)
		if err != nil {
			s.logger.Warn().Err(err).Str("input", name).Str("workflow", binding.CalledPath).
				Msg("workflow_call input template failed")
			return false
		}
		typed, err := TypedCallInput(binding.InputDefs[name], val)
		if err != nil {
			s.logger.Warn().Err(err).Str("input", name).Str("workflow", binding.CalledPath).
				Msg("workflow_call input validation failed")
			return false
		}
		resolved[name] = typed
	}
	for name, def := range binding.InputDefs {
		if _, ok := resolved[name]; ok || def == nil {
			continue
		}
		if def.Default != nil {
			resolved[name] = store.NormalizeYAMLValue(def.Default)
		} else if def.Type == "boolean" {
			resolved[name] = false
		}
	}
	binding.SetResolvedInputs(resolved)
	return true
}

// TypedCallInput converts a resolved string input to the declared type,
// rejecting the truthy spellings GitHub's input contract rejects.
func TypedCallInput(def *store.WorkflowInputDef, val string) (interface{}, error) {
	if def == nil {
		return val, nil
	}
	switch def.Type {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, fmt.Errorf("%q is not a boolean", val)
		}
	case "number":
		f, err := parseJSONNumber(val)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number: %w", val, err)
		}
		return f, nil
	case "choice":
		for _, option := range def.Options {
			if fmt.Sprint(store.NormalizeYAMLValue(option)) == val {
				return val, nil
			}
		}
		return nil, fmt.Errorf("%q is not one of the declared options", val)
	default:
		return val, nil
	}
}

// collectCallOutputsLocked finishes a collector node: it evaluates the
// workflow_call output templates against the called jobs' results, and reports
// skipped when every called job skipped (matching GitHub's caller-job state).
func (s *Engine) collectCallOutputsLocked(wf *store.Workflow, collector *store.WorkflowJob) {
	binding := collector.Def.Call
	allSkipped := true
	jobsCtx := make(map[string]interface{})
	if binding != nil {
		for _, key := range binding.CalledJobKeys {
			called, ok := wf.Jobs[key]
			if !ok {
				continue
			}
			if called.Status != store.JobStatusSkipped {
				allSkipped = false
			}
			outputs := make(map[string]interface{}, len(called.Outputs))
			for k, v := range called.Outputs {
				outputs[k] = v
			}
			// Output templates address jobs by their UNPREFIXED key.
			shortKey := strings.TrimPrefix(key, binding.CallerKey+"/")
			jobsCtx[shortKey] = map[string]interface{}{
				"result":  string(called.Result),
				"outputs": outputs,
			}
		}
	}

	if allSkipped {
		collector.Status = store.JobStatusSkipped
		collector.Result = store.ResultSkipped
		return
	}

	collector.Status = store.JobStatusCompleted
	collector.Result = store.ResultSuccess
	if binding == nil || len(binding.OutputDefs) == 0 {
		return
	}
	ctx := &ExprContext{Contexts: map[string]interface{}{
		"jobs":   jobsCtx,
		"github": s.GithubContextMap(wf),
		"inputs": binding.ResolvedInputs(),
	}}
	for name, tmpl := range binding.OutputDefs {
		val, err := EvalTemplate(tmpl, ctx)
		if err != nil {
			s.logger.Warn().Err(err).Str("output", name).Msg("workflow_call output template failed")
			collector.Status = store.JobStatusCompleted
			collector.Result = store.ResultFailure
			return
		}
		collector.Outputs[name] = val
	}
}

// parseJSONNumber parses a numeric string as expression numbers work.
func parseJSONNumber(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

func MergedCallEnvironment(workflowEnv, jobEnv map[string]string) map[string]string {
	if len(workflowEnv) == 0 && len(jobEnv) == 0 {
		return nil
	}
	merged := make(map[string]string, len(workflowEnv)+len(jobEnv))
	for name, value := range workflowEnv {
		merged[name] = value
	}
	for name, value := range jobEnv {
		merged[name] = value
	}
	return merged
}
