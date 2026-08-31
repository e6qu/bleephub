package actions

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

// BuildJobMessageFromDef builds a job message from a WorkflowDef-based job,
// handling both run: and uses: steps.
func (s *Engine) BuildJobMessageFromDef(serverURL string, wf *store.Workflow, wfJob *store.WorkflowJob, planID, timelineID string, requestID int64, defaultImage string) (map[string]interface{}, error) {
	jd := wfJob.Def
	scopeID := uuid.New().String()
	// GITHUB_TOKEN is scoped to this repository and the least-privilege
	// permissions its `permissions:` block resolves to (ACT-014).
	jobToken := s.mintJobToken(scopeID, wf, jd)

	// Empty image means host mode: the runner executes the job directly. A bare
	// image string rides as-is; the object form (`container:` with
	// env/ports/volumes/options) becomes a full mapping token.
	image := defaultImage
	var jobContainer interface{}
	if img := jd.ContainerImage(); img != "" {
		image = img
	}
	if cd := jd.ContainerObject(); cd != nil {
		jobContainer = containerSpecToken(cd.Image, cd.Env, cd.Ports, cd.Volumes, cd.Options)
	} else {
		jobContainer = jobContainerValue(image)
	}

	steps := make([]map[string]interface{}, 0, len(jd.Steps))
	for i, step := range jd.Steps {
		if step.Shell == "" {
			step.Shell = jd.Defaults.Shell
		}
		if step.WorkingDirectory == "" {
			step.WorkingDirectory = jd.Defaults.WorkingDirectory
		}
		stepID := uuid.New().String()

		if step.Run != "" {
			displayName := step.Name
			if displayName == "" {
				displayName = fmt.Sprintf("Run %s", truncateDisplay(step.Run, 40))
			}
			contextName := step.ID
			if contextName == "" {
				contextName = fmt.Sprintf("__run_%d", i+1)
			}
			inputEntries := []interface{}{
				map[string]interface{}{
					"Key":   map[string]interface{}{"type": 0, "lit": "script"},
					"Value": templateToken(step.Run),
				},
			}
			if step.Shell != "" {
				inputEntries = append(inputEntries, mappingEntry("shell", templateToken(step.Shell)))
			}
			if step.WorkingDirectory != "" {
				inputEntries = append(inputEntries, mappingEntry("workingDirectory", templateToken(step.WorkingDirectory)))
			}
			messageStep := map[string]interface{}{
				"type": "action",
				"id":   stepID,
				"name": contextName,
				"reference": map[string]interface{}{
					"type": "script",
				},
				"displayNameToken": displayName,
				"contextName":      contextName,
				"condition":        stepCondition(step.If),
				"inputs": map[string]interface{}{
					"type": 2,
					"map":  inputEntries,
				},
			}
			applyStepExecutionOptions(messageStep, step)
			steps = append(steps, messageStep)
		} else if step.Uses != "" {
			nameWithOwner, path, ref, isLocal := store.ParseActionRef(step.Uses)
			displayName := step.Name
			if displayName == "" {
				displayName = step.Uses
			}
			contextName := step.ID
			if contextName == "" {
				contextName = fmt.Sprintf("__action_%d", i+1)
			}

			var reference map[string]interface{}
			if isLocal {
				reference = map[string]interface{}{
					"type": "script",
					"path": path,
				}
			} else {
				reference = map[string]interface{}{
					"type":           "repository",
					"name":           nameWithOwner,
					"ref":            ref,
					"repositoryType": "GitHub",
				}
				if path != "" {
					reference["path"] = path
				}
			}

			inputEntries := make([]interface{}, 0, len(step.With))
			for k, v := range step.With {
				inputEntries = append(inputEntries, map[string]interface{}{
					"Key":   map[string]interface{}{"type": 0, "lit": k},
					"Value": templateToken(v),
				})
			}

			messageStep := map[string]interface{}{
				"type":             "action",
				"id":               stepID,
				"name":             contextName,
				"reference":        reference,
				"displayNameToken": displayName,
				"contextName":      contextName,
				"condition":        stepCondition(step.If),
				"inputs": map[string]interface{}{
					"type": 2,
					"map":  inputEntries,
				},
			}
			applyStepExecutionOptions(messageStep, step)
			steps = append(steps, messageStep)
		}
	}

	envPairs := make([]string, 0)
	for k, v := range wf.Env {
		if k != "__serverURL" && k != "__defaultImage" {
			envPairs = append(envPairs, k, v)
		}
	}
	for k, v := range jd.Env {
		envPairs = append(envPairs, k, v)
	}

	needsCtx := BuildNeedsContext(wf, wfJob)

	// Job outputs ride as a mapping TemplateToken, not an unevaluated
	// server-side expression map: the runner evaluates it against its final
	// steps context and returns resolved values in the JobCompleted event.
	jobOutputs := jobOutputsToken(jd.Outputs)

	var matrixCtx interface{}
	if len(wfJob.MatrixValues) > 0 {
		matrixCtx = toPipelineContextData(anyMap(wfJob.MatrixValues))
	}

	runID := strconv.Itoa(wf.RunID)

	repoFullName := wf.RepoFullName

	secretsPairs := make([]string, 0)
	maskArray := make([]interface{}, 0)

	secretsPairs = append(secretsPairs, "GITHUB_TOKEN", jobToken)
	maskArray = append(maskArray, map[string]interface{}{"type": "regex", "value": regexp.QuoteMeta(jobToken)})

	// Every secret rides the mask list so the runner scrubs it from logs. The
	// official runner builds its `secrets` context from message.Variables
	// entries flagged isSecret, NOT from contextData, so every secret also
	// rides the variables map under its own name.
	variables := map[string]interface{}{
		"system.github.job":                      varVal(wfJob.Key),
		"system.github.runid":                    varVal(runID),
		"system.github.token":                    varSecret(jobToken),
		"github_token":                           varSecret(jobToken),
		"GITHUB_TOKEN":                           varSecret(jobToken),
		"system.phaseDisplayName":                varVal(wfJob.DisplayName),
		"system.runnerGroupName":                 varVal("Default"),
		"DistributedTask.NewActionMetadata":      varVal("true"),
		"DistributedTask.EnableCompositeActions": varVal("true"),
	}
	varsPairs := make([]string, 0)
	if s.store != nil && repoFullName != "" {
		secretsMap, varsMap, err := s.CollectJobSecretsAndVars(repoFullName, jd.EnvironmentName())
		if err != nil {
			return nil, err
		}
		if forkPullRequestWithholdsSecrets(wf) {
			secretsMap = nil
		}
		if jd.Call != nil && jd.CallRole == "" {
			secretsMap, err = EffectiveCallSecrets(s, wf, jd.Call, secretsMap)
			if err != nil {
				return nil, err
			}
		}
		for _, name := range sortedKeys(secretsMap) {
			secretsPairs = append(secretsPairs, name, secretsMap[name])
			variables[name] = varSecret(secretsMap[name])
			maskArray = append(maskArray, map[string]interface{}{"type": "regex", "value": regexp.QuoteMeta(secretsMap[name])})
		}
		for _, name := range sortedKeys(varsMap) {
			varsPairs = append(varsPairs, name, varsMap[name])
		}
	}

	// Called jobs get the call's resolved typed inputs; workflow_dispatch runs
	// their typed inputs; else strings.
	var inputsCtx interface{}
	switch {
	case jd.Call != nil && jd.CallRole == "" && jd.Call.ResolvedInputs() != nil:
		inputsCtx = toPipelineContextData(anyMap(jd.Call.ResolvedInputs()))
	case len(wf.TypedInputs) > 0:
		inputsCtx = toPipelineContextData(anyMap(wf.TypedInputs))
	case len(wf.Inputs) > 0:
		inputsPairs := make([]string, 0, len(wf.Inputs)*2)
		for k, v := range wf.Inputs {
			inputsPairs = append(inputsPairs, k, v)
		}
		inputsCtx = DictContextData(inputsPairs...)
	}

	return map[string]interface{}{
		"messageType": "PipelineAgentJobRequest",
		"plan": map[string]interface{}{
			"scopeIdentifier": scopeID,
			"planId":          planID,
			"planType":        "free",
			"planGroup":       "free",
			"version":         12,
			"owner": map[string]interface{}{
				"id":   0,
				"name": "Community",
			},
		},
		"timeline": map[string]interface{}{
			"id":       timelineID,
			"changeId": 1,
			"location": nil,
		},
		"jobId":                wfJob.JobID,
		"jobDisplayName":       wfJob.DisplayName,
		"jobName":              wfJob.Key,
		"requestId":            requestID,
		"lockedUntil":          "0001-01-01T00:00:00",
		"jobContainer":         jobContainer,
		"jobServiceContainers": buildServiceContainers(jd.Services),
		"jobOutputs":           jobOutputs,
		"resources": map[string]interface{}{
			"endpoints": []map[string]interface{}{
				{
					"name": "SystemVssConnection",
					"url":  serverURL + "/",
					"authorization": map[string]interface{}{
						"scheme": "OAuth",
						"parameters": map[string]string{
							"AccessToken": jobToken,
						},
					},
					"data": map[string]string{
						"CacheServerUrl":    serverURL + "/",
						"ResultsServiceUrl": serverURL + "/",
					},
					"isShared": false,
					"isReady":  true,
				},
			},
			"repositories": []interface{}{},
			"containers":   []interface{}{},
		},
		"contextData": map[string]interface{}{
			"github": toPipelineContextData(githubRunnerContext(s, wf, wfJob, serverURL, jobToken)),
			// Built runner-agnostic; the broker rebinds it to the leasing agent
			// at delivery (ACT-051).
			"runner":   RunnerContextData(nil),
			"env":      DictContextData(envPairs...),
			"vars":     DictContextData(varsPairs...),
			"secrets":  DictContextData(secretsPairs...),
			"needs":    needsCtx,
			"inputs":   inputsCtx,
			"matrix":   matrixCtx,
			"strategy": nil,
		},
		"variables":            variables,
		"mask":                 maskArray,
		"steps":                steps,
		"workspace":            map[string]interface{}{},
		"defaults":             nil,
		"environmentVariables": nil,
		"actionsEnvironment":   nil,
		"fileTable":            []string{".github/workflows/ci.yml"},
	}, nil
}

// jobOutputsToken encodes declared outputs as the TemplateToken mapping
// actions/runner consumes, or nil when there are none.
func jobOutputsToken(outputs map[string]string) interface{} {
	if len(outputs) == 0 {
		return nil
	}
	entries := make([]interface{}, 0, len(outputs))
	for _, name := range sortedKeys(outputs) {
		entries = append(entries, mappingEntry(name, templateToken(outputs[name])))
	}
	return mappingToken(entries)
}

// templateToken converts a workflow string into the runner's template token: a
// plain literal when it carries no ${{ }}, else a BasicExpression token
// (`format('...', expr...)` with {N} placeholders) so the RUNNER evaluates the
// expressions against its contexts rather than raw text reaching the shell.
func templateToken(s string) map[string]interface{} {
	if !strings.Contains(s, "${{") {
		return map[string]interface{}{"type": 0, "lit": s}
	}
	return map[string]interface{}{"type": 3, "expr": templateToFormatExpr(s)}
}

// templateToFormatExpr rewrites "a ${{ x }} b" into "format('a {0} b', x)"; a
// string that is exactly one template becomes the bare inner expression.
func templateToFormatExpr(s string) string {
	escape := func(part string) string {
		part = strings.ReplaceAll(part, "'", "''")
		part = strings.ReplaceAll(part, "{", "{{")
		part = strings.ReplaceAll(part, "}", "}}")
		return part
	}
	var fmtStr strings.Builder
	var exprs []string
	rest := s
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			fmtStr.WriteString(escape(rest))
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			fmtStr.WriteString(escape(rest))
			break
		}
		fmtStr.WriteString(escape(rest[:i]))
		exprs = append(exprs, strings.TrimSpace(rest[i+3:i+j]))
		fmt.Fprintf(&fmtStr, "{%d}", len(exprs)-1)
		rest = rest[i+j+2:]
	}
	if len(exprs) == 1 && fmtStr.String() == "{0}" {
		return exprs[0]
	}
	return fmt.Sprintf("format('%s', %s)", fmtStr.String(), strings.Join(exprs, ", "))
}

// sortedKeys returns a map's keys sorted so context payloads are deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// anyMap widens a typed-inputs map for toPipelineContextData.
func anyMap(m map[string]interface{}) map[string]interface{} { return m }

// githubRunnerContext assembles the full `github` context: the server-side
// context map plus the runner-session keys.
func githubRunnerContext(s *Engine, wf *store.Workflow, wfJob *store.WorkflowJob, serverURL, jobToken string) map[string]interface{} {
	m := s.GithubContextMap(wf)
	m["server_url"] = serverURL
	m["api_url"] = serverURL
	if wfJob != nil {
		m["job"] = wfJob.Key
	}
	m["action"] = "__run"
	m["workspace"] = "/github/workspace"
	m["token"] = jobToken
	m["job_workflow_sha"] = wf.Sha
	if wfJob != nil && wfJob.Def != nil && wfJob.Def.Call != nil && wfJob.Def.CallRole == "" {
		binding := wfJob.Def.Call
		m["job_workflow_ref"] = binding.CalledRepo + "/" + binding.CalledPath + "@" + wf.Ref
	} else if workflowRef, ok := m["workflow_ref"]; ok {
		m["job_workflow_ref"] = workflowRef
	}
	return m
}

// forkPullRequestWithholdsSecrets reports whether a run is denied all secrets
// but GITHUB_TOKEN: a fork contributor authors the `pull_request` workflow, so
// its run must not see the base repository's secrets, and approval only decides
// whether the run starts. pull_request_target is excluded — it runs the BASE
// workflow in the base context and does receive secrets.
func forkPullRequestWithholdsSecrets(wf *store.Workflow) bool {
	return wf.EventName == "pull_request" && pullRequestIsFromFork(wf.EventPayload, wf.RepoFullName)
}

// IsForkPullRequestRun reports whether wf is a fork-authored pull_request run —
// the runs that must receive a read-only GITHUB_TOKEN (and no secrets) by
// default. Exported for the token-permission resolver in the server package.
func IsForkPullRequestRun(wf *store.Workflow) bool {
	return forkPullRequestWithholdsSecrets(wf)
}

// EffectiveCallSecrets resolves the secrets a job inside a reusable-workflow
// call receives, narrowing the repository's set once per call from the
// outermost inwards. `secrets: inherit` inherits the CALLING workflow's
// secrets, which for a nested call is whatever the enclosing call narrowed them
// to, not the repository's full set.
func EffectiveCallSecrets(s *Engine, wf *store.Workflow, binding *store.WorkflowCallBinding, repoSecrets map[string]string) (map[string]string, error) {
	if binding == nil {
		return repoSecrets, nil
	}
	callerSecrets := repoSecrets
	if binding.Parent != nil {
		var err error
		callerSecrets, err = EffectiveCallSecrets(s, wf, binding.Parent, repoSecrets)
		if err != nil {
			return nil, err
		}
	}
	if binding.SecretsInherit {
		return callerSecrets, nil
	}
	return RemapCallSecrets(s, wf, binding, callerSecrets)
}

// RemapCallSecrets applies a call's explicit `secrets:` map: the called job
// receives ONLY the mapped names, each value template evaluated against the
// caller's secrets.
func RemapCallSecrets(s *Engine, wf *store.Workflow, binding *store.WorkflowCallBinding, callerSecrets map[string]string) (map[string]string, error) {
	secretsCtx := make(map[string]interface{}, len(callerSecrets))
	for k, v := range callerSecrets {
		secretsCtx[k] = v
	}
	ctx := &ExprContext{Contexts: map[string]interface{}{
		"secrets": secretsCtx,
		"github":  s.GithubContextMap(wf),
	}}
	mapped := make(map[string]string, len(binding.SecretsMap))
	for name, tmpl := range binding.SecretsMap {
		val, err := EvalTemplate(tmpl, ctx)
		if err != nil {
			s.logger.Warn().Err(err).Str("secret", name).Str("workflow", binding.CalledPath).
				Msg("workflow_call secret template failed")
			return nil, fmt.Errorf("evaluate reusable workflow secret %q: %w", name, err)
		}
		mapped[name] = val
	}
	return mapped, nil
}

// toPipelineContextData converts a Go value into the runner's
// PipelineContextData JSON encoding: bare strings, {"t":3,"d":bool},
// {"t":4,"d":number}, {"t":1,"a":[...]} arrays, {"t":2,"d":[{"k",..,"v":..}]}
// dictionaries.
func toPipelineContextData(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return t
	case bool:
		return map[string]interface{}{"t": 3, "d": t}
	case float64:
		return map[string]interface{}{"t": 4, "d": t}
	case int:
		return map[string]interface{}{"t": 4, "d": float64(t)}
	case []interface{}:
		arr := make([]interface{}, 0, len(t))
		for _, item := range t {
			arr = append(arr, toPipelineContextData(item))
		}
		return map[string]interface{}{"t": 1, "a": arr}
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		entries := make([]map[string]interface{}, 0, len(t))
		for _, k := range keys {
			entries = append(entries, map[string]interface{}{"k": k, "v": toPipelineContextData(t[k])})
		}
		return map[string]interface{}{"t": 2, "d": entries}
	default:
		return fmt.Sprintf("%v", t)
	}
}

// buildServiceContainers converts ServiceDefs to the jobServiceContainers wire
// shape: a mapping TemplateToken of alias → container-spec token. The runner
// deserializes this field as a TemplateToken, NOT plain JSON — a raw map fails
// its template validation at job start.
func buildServiceContainers(services map[string]*store.ServiceDef) interface{} {
	if len(services) == 0 {
		return nil
	}
	entries := make([]interface{}, 0, len(services))
	for _, name := range sortedServiceNames(services) {
		entries = append(entries, mappingEntry(name, containerSpecToken(
			services[name].Image, services[name].Env, services[name].Ports,
			services[name].Volumes, services[name].Options,
		)))
	}
	return mappingToken(entries)
}

// containerSpecToken builds the container-spec token shared by object-form
// `container:` and each `services:` entry, keyed per the runner's ContainerInfo
// reader (image/env/ports/volumes/options). Strings route through templateToken
// so ${{ }} expressions still evaluate runner-side.
func containerSpecToken(image string, env map[string]string, ports []interface{}, volumes []string, options string) map[string]interface{} {
	entries := []interface{}{mappingEntry("image", templateToken(image))}
	if len(env) > 0 {
		envEntries := make([]interface{}, 0, len(env))
		for _, k := range sortedKeys(env) {
			envEntries = append(envEntries, mappingEntry(k, templateToken(env[k])))
		}
		entries = append(entries, mappingEntry("env", mappingToken(envEntries)))
	}
	if len(ports) > 0 {
		seq := make([]interface{}, 0, len(ports))
		for _, p := range ports {
			seq = append(seq, scalarToken(p))
		}
		entries = append(entries, mappingEntry("ports", map[string]interface{}{"type": 1, "seq": seq}))
	}
	if len(volumes) > 0 {
		seq := make([]interface{}, 0, len(volumes))
		for _, v := range volumes {
			seq = append(seq, templateToken(v))
		}
		entries = append(entries, mappingEntry("volumes", map[string]interface{}{"type": 1, "seq": seq}))
	}
	if options != "" {
		entries = append(entries, mappingEntry("options", templateToken(options)))
	}
	return mappingToken(entries)
}

func mappingToken(entries []interface{}) map[string]interface{} {
	return map[string]interface{}{"type": 2, "map": entries}
}

func mappingEntry(key string, value interface{}) map[string]interface{} {
	return map[string]interface{}{
		"Key":   map[string]interface{}{"type": 0, "lit": key},
		"Value": value,
	}
}

func applyStepExecutionOptions(message map[string]interface{}, step store.StepDef) {
	if len(step.Env) > 0 {
		entries := make([]interface{}, 0, len(step.Env))
		for _, name := range sortedKeys(step.Env) {
			entries = append(entries, mappingEntry(name, templateToken(step.Env[name])))
		}
		message["environment"] = mappingToken(entries)
	}
	if step.ContinueOnError != nil {
		message["continueOnError"] = scalarOrTemplateToken(step.ContinueOnError)
	}
	if step.TimeoutMinutes > 0 {
		message["timeoutInMinutes"] = scalarToken(step.TimeoutMinutes)
	}
}

func scalarOrTemplateToken(value interface{}) map[string]interface{} {
	if text, ok := value.(string); ok {
		return templateToken(text)
	}
	return scalarToken(value)
}

// scalarToken encodes a YAML scalar (string/number/bool) as the matching
// literal TemplateToken; `ports:` entries parse as numbers or strings.
func scalarToken(v interface{}) map[string]interface{} {
	switch t := v.(type) {
	case string:
		return templateToken(t)
	case bool:
		return map[string]interface{}{"type": 5, "lit": t}
	case int, int64, float64:
		return map[string]interface{}{"type": 6, "lit": t}
	default:
		return templateToken(fmt.Sprintf("%v", t))
	}
}

func sortedServiceNames(m map[string]*store.ServiceDef) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// BuildNeedsContext builds the "needs" PipelineContextData from completed
// dependency outputs. Jobs inside a reusable-workflow call see sibling needs
// under unprefixed keys; the synthetic gate never appears.
func BuildNeedsContext(wf *store.Workflow, wfJob *store.WorkflowJob) interface{} {
	if len(wfJob.Needs) == 0 {
		return DictContextData()
	}
	var binding *store.WorkflowCallBinding
	if wfJob.Def != nil && wfJob.Def.Call != nil && wfJob.Def.CallRole == "" {
		binding = wfJob.Def.Call
	}

	entries := make([]map[string]interface{}, 0, len(wfJob.Needs))
	for _, depKey := range wfJob.Needs {
		depJob, ok := wf.Jobs[depKey]
		if !ok {
			continue
		}
		if binding != nil {
			if depKey == binding.CallerKey+"/__call" {
				continue
			}
			depKey = strings.TrimPrefix(depKey, binding.CallerKey+"/")
		}

		outputEntries := make([]map[string]interface{}, 0, len(depJob.Outputs))
		for k, v := range depJob.Outputs {
			outputEntries = append(outputEntries, map[string]interface{}{
				"k": k, "v": v,
			})
		}

		depEntries := []map[string]interface{}{
			{"k": "result", "v": string(depJob.Result)},
			{"k": "outputs", "v": map[string]interface{}{"t": 2, "d": outputEntries}},
		}

		entries = append(entries, map[string]interface{}{
			"k": depKey,
			"v": map[string]interface{}{"t": 2, "d": depEntries},
		})
	}

	return map[string]interface{}{"t": 2, "d": entries}
}

func stepCondition(ifExpr string) string {
	if ifExpr == "" {
		return "success()"
	}
	if ExprContainsAnyStatusFunction(ifExpr) {
		return ifExpr
	}
	return "success() && (" + ifExpr + ")"
}

// DictContextData builds a DictionaryContextData from alternating key/value
// strings.
func DictContextData(kvs ...string) map[string]interface{} {
	entries := make([]map[string]interface{}, 0, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		entries = append(entries, map[string]interface{}{
			"k": kvs[i],
			"v": kvs[i+1],
		})
	}
	return map[string]interface{}{
		"t": 2,
		"d": entries,
	}
}

func varVal(value string) map[string]interface{} {
	return map[string]interface{}{
		"value":    value,
		"isSecret": false,
	}
}

// RunnerContextData builds the `runner` context (os/arch/name and OS-specific
// tool_cache/temp paths). A nil agent yields generic defaults; the broker
// rebinds this to the leasing agent at delivery (ACT-051).
func RunnerContextData(agent *store.Agent) map[string]interface{} {
	osName, arch, name := "Linux", "X64", "test-runner"
	if agent != nil {
		if agent.Name != "" {
			name = agent.Name
		}
		osName = runnerContextOS(agent)
		arch = runnerContextArch(agent)
	}
	toolCache, temp := "/opt/hostedtoolcache", "/home/runner/work/_temp"
	switch osName {
	case "Windows":
		toolCache, temp = `C:\hostedtoolcache\windows`, `C:\a\_temp`
	case "macOS":
		toolCache, temp = "/Users/runner/hostedtoolcache", "/Users/runner/work/_temp"
	}
	return DictContextData(
		"os", osName,
		"arch", arch,
		"name", name,
		"tool_cache", toolCache,
		"temp", temp,
	)
}

// runnerContextOS maps an agent to the canonical `runner.os` value
// ("Linux"/"Windows"/"macOS"), preferring a self-reported label over the
// free-form OS description.
func runnerContextOS(agent *store.Agent) string {
	for _, l := range agent.Labels {
		switch strings.ToLower(l.Name) {
		case "linux":
			return "Linux"
		case "windows":
			return "Windows"
		case "macos":
			return "macOS"
		}
	}
	switch OSFromDescription(agent.OSDescription) {
	case "windows":
		return "Windows"
	case "macos":
		return "macOS"
	default:
		return "Linux"
	}
}

// runnerContextArch maps an agent to the canonical `runner.arch` value
// ("X64"/"ARM"/"ARM64"), preferring a self-reported label and falling back to
// tokens in the OS description.
func runnerContextArch(agent *store.Agent) string {
	for _, l := range agent.Labels {
		switch strings.ToLower(l.Name) {
		case "arm64":
			return "ARM64"
		case "arm":
			return "ARM"
		case "x64", "x86_64", "amd64":
			return "X64"
		}
	}
	d := strings.ToLower(agent.OSDescription)
	switch {
	case strings.Contains(d, "arm64"), strings.Contains(d, "aarch64"):
		return "ARM64"
	case strings.Contains(d, "arm"):
		return "ARM"
	default:
		return "X64"
	}
}

// rebindRunnerContext rewrites the `contextData.runner` block of a serialized
// job message to reflect the leasing agent. Idempotent. Returns the original
// bytes unchanged if the message is not a job request in the expected shape.
func rebindRunnerContext(bodyJSON string, agent *store.Agent) string {
	if agent == nil {
		return bodyJSON
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(bodyJSON), &m); err != nil {
		return bodyJSON
	}
	cd, ok := m["contextData"].(map[string]interface{})
	if !ok {
		return bodyJSON
	}
	cd["runner"] = RunnerContextData(agent)
	out, err := json.Marshal(m)
	if err != nil {
		return bodyJSON
	}
	return string(out)
}

func varSecret(value string) map[string]interface{} {
	return map[string]interface{}{
		"value":    value,
		"isSecret": true,
	}
}

func truncateDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
