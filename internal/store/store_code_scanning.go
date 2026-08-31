package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// CodeScanningState is a code-scanning alert's lifecycle state; the three
// constants are the only values GitHub emits.
type CodeScanningState string

const (
	CodeScanningStateOpen      CodeScanningState = "open"
	CodeScanningStateDismissed CodeScanningState = "dismissed"
	CodeScanningStateFixed     CodeScanningState = "fixed"
)

// CodeScanningDismissedReason is the reason recorded when an alert is dismissed;
// only these four values are accepted.
type CodeScanningDismissedReason string

const (
	// GitHub's code-scanning dismissed_reason enum uses spaces, not underscores
	// (unlike the secret-scanning resolution enum), and has no "ignored" value
	// (PAR-010).
	CodeScanningDismissedFalsePositive CodeScanningDismissedReason = "false positive"
	CodeScanningDismissedWontFix       CodeScanningDismissedReason = "won't fix"
	CodeScanningDismissedUsedInTests   CodeScanningDismissedReason = "used in tests"
	CodeScanningDismissedMitigated     CodeScanningDismissedReason = "mitigated"
)

// CodeScanningAlertInstance is one occurrence of a code-scanning alert.
type CodeScanningAlertInstance struct {
	Ref         string            `json:"ref"`
	AnalysisKey string            `json:"analysis_key"`
	Category    string            `json:"category"`
	State       CodeScanningState `json:"state"`
	CommitSHA   string            `json:"commit_sha"`
	Path        string            `json:"path"`
	StartLine   int               `json:"start_line"`
	EndLine     int               `json:"end_line"`
	StartColumn int               `json:"start_column"`
	EndColumn   int               `json:"end_column"`
	Message     string            `json:"message"`
}

// CodeScanningAlert is a repo-scoped alert from a SARIF upload or the operator
// seeding endpoint.
type CodeScanningAlert struct {
	ID           int    `json:"id"`
	NodeID       string `json:"node_id"`
	Number       int    `json:"number"`
	RepoKey      string `json:"repo_key"`
	RuleID       string `json:"rule_id"`
	RuleSeverity string `json:"rule_severity"`
	// SecuritySeverityLevel is the low/medium/high/critical bucket derived from a
	// rule's numeric SARIF security-severity score; empty (null) for a quality rule.
	SecuritySeverityLevel string                      `json:"security_severity_level"`
	RuleDescription       string                      `json:"rule_description"`
	ToolName              string                      `json:"tool_name"`
	ToolGUID              string                      `json:"tool_guid"`
	State                 CodeScanningState           `json:"state"`
	DismissedReason       CodeScanningDismissedReason `json:"dismissed_reason"`
	DismissedComment      string                      `json:"dismissed_comment"`
	DismissedAt           *time.Time                  `json:"dismissed_at"`
	FixedAt               *time.Time                  `json:"fixed_at"`
	HTMLURL               string                      `json:"html_url"`
	URL                   string                      `json:"url"`
	InstancesURL          string                      `json:"instances_url"`
	Instances             []CodeScanningAlertInstance `json:"instances"`
	CreatedAt             time.Time                   `json:"created_at"`
	UpdatedAt             time.Time                   `json:"updated_at"`
}

// CodeScanningAnalysis is a single code-scanning analysis run for a repo.
type CodeScanningAnalysis struct {
	ID            int       `json:"id"`
	NodeID        string    `json:"node_id"`
	RepoKey       string    `json:"repo_key"`
	Ref           string    `json:"ref"`
	CommitSHA     string    `json:"commit_sha"`
	AnalysisKey   string    `json:"analysis_key"`
	Category      string    `json:"category"`
	ToolName      string    `json:"tool_name"`
	ToolGUID      string    `json:"tool_guid"`
	ResultsCount  int       `json:"results_count"`
	RulesCount    int       `json:"rules_count"`
	SARIFUploadID string    `json:"sarif_upload_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	HTMLURL       string    `json:"html_url"`
	URL           string    `json:"url"`
}

// SARIFUpload tracks a SARIF upload request. GitHub processes asynchronously;
// bleephub processes synchronously and stores the upload as complete.
type SARIFUpload struct {
	ID        string    `json:"id"`
	RepoKey   string    `json:"repo_key"`
	Status    string    `json:"status"`
	Errors    []string  `json:"errors"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateCodeScanningAlert seeds an alert through the operator surface.
func (st *Store) CreateCodeScanningAlert(repoKey, ruleID, severity, description, toolName, toolGUID, state string, instances []CodeScanningAlertInstance) *CodeScanningAlert {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.CodeScanningAlertsByRepo[repoKey] == nil {
		st.CodeScanningAlertsByRepo[repoKey] = make(map[int]*CodeScanningAlert)
	}
	if st.CodeScanningNextNumber[repoKey] == 0 {
		st.CodeScanningNextNumber[repoKey] = 1
	}

	now := st.CurrentTime()
	if state == "" {
		state = "open"
	}

	number := st.CodeScanningNextNumber[repoKey]
	st.CodeScanningNextNumber[repoKey] = number + 1

	alert := &CodeScanningAlert{
		ID:              st.NextCodeScanningAlertID,
		NodeID:          fmt.Sprintf("CSWA%d", st.NextCodeScanningAlertID),
		Number:          number,
		RepoKey:         repoKey,
		RuleID:          ruleID,
		RuleSeverity:    severity,
		RuleDescription: description,
		ToolName:        toolName,
		State:           CodeScanningState(state),
		Instances:       instances,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	st.NextCodeScanningAlertID++

	st.CodeScanningAlerts[alert.ID] = alert
	st.CodeScanningAlertsByRepo[repoKey][number] = alert
	st.persistCodeScanningAlert(alert)
	return alert
}

// cloneCodeScanningAlert returns a deep copy safe to hand outside the store lock
// (STORE-021). Instances, DismissedAt and FixedAt are the only reference fields.
func cloneCodeScanningAlert(a *CodeScanningAlert) *CodeScanningAlert {
	if a == nil {
		return nil
	}
	clone := *a
	if a.Instances != nil {
		clone.Instances = append([]CodeScanningAlertInstance(nil), a.Instances...)
	}
	if a.DismissedAt != nil {
		dismissed := *a.DismissedAt
		clone.DismissedAt = &dismissed
	}
	if a.FixedAt != nil {
		fixed := *a.FixedAt
		clone.FixedAt = &fixed
	}
	return &clone
}

// GetCodeScanningAlert returns an alert by repo + alert number.
func (st *Store) GetCodeScanningAlert(repoKey string, number int) *CodeScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneCodeScanningAlert(st.CodeScanningAlertsByRepo[repoKey][number])
}

// ListCodeScanningAlerts returns repo alerts filtered and sorted per GitHub's
// list endpoint.
func (st *Store) ListCodeScanningAlerts(repoKey, state, severity, toolName, rule, sortField, direction string) []*CodeScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	byRepo := st.CodeScanningAlertsByRepo[repoKey]
	out := make([]*CodeScanningAlert, 0, len(byRepo))
	for _, a := range byRepo {
		if state != "" && a.State != CodeScanningState(state) {
			continue
		}
		if severity != "" && a.RuleSeverity != severity {
			continue
		}
		if toolName != "" && a.ToolName != toolName {
			continue
		}
		if rule != "" && a.RuleID != rule {
			continue
		}
		out = append(out, a)
	}

	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}

	sort.SliceStable(out, func(i, j int) bool {
		var less bool
		switch sortField {
		case "updated":
			less = out[i].UpdatedAt.Before(out[j].UpdatedAt)
		default:
			less = out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		if direction == "asc" {
			return less
		}
		return !less
	})
	return snapshotCodeScanningAlerts(out)
}

// UpdateCodeScanningAlert applies a state/dismissed_reason transition. Since `a`
// is a detached clone from GetCodeScanningAlert, the mutation hits the LIVE row
// re-fetched by key, and a fresh snapshot is written back into `a`.
func (st *Store) UpdateCodeScanningAlert(a *CodeScanningAlert, state, dismissedReason, dismissedComment string) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	live := st.CodeScanningAlertsByRepo[a.RepoKey][a.Number]
	if live == nil {
		return fmt.Errorf("code scanning alert %s#%d not found", a.RepoKey, a.Number)
	}
	if err := validateCodeScanningTransition(string(live.State), state, dismissedReason); err != nil {
		return err
	}

	now := st.CurrentTime()
	switch state {
	case "dismissed":
		live.State = CodeScanningStateDismissed
		live.DismissedReason = CodeScanningDismissedReason(dismissedReason)
		live.DismissedComment = dismissedComment
		live.DismissedAt = &now
		live.FixedAt = nil
	case "fixed":
		live.State = CodeScanningStateFixed
		live.FixedAt = &now
		live.DismissedReason = ""
		live.DismissedComment = ""
		live.DismissedAt = nil
	case "open":
		live.State = CodeScanningStateOpen
		live.DismissedReason = ""
		live.DismissedComment = ""
		live.DismissedAt = nil
		live.FixedAt = nil
	}
	live.UpdatedAt = now
	st.persistCodeScanningAlert(live)
	*a = *cloneCodeScanningAlert(live)
	return nil
}

func validateCodeScanningTransition(currentState, newState, dismissedReason string) error {
	// GitHub's set-state enum is open|dismissed; `fixed` is a system-derived
	// response-only state a client PATCH cannot set (422).
	if newState != "" && newState != "open" && newState != "dismissed" {
		return fmt.Errorf("invalid state %q", newState)
	}
	if newState == "dismissed" && !isValidDismissedReason(dismissedReason) {
		return fmt.Errorf("invalid dismissed_reason %q", dismissedReason)
	}
	if newState == "open" && currentState == "dismissed" {
		return nil
	}
	if newState == "fixed" && (currentState == "open" || currentState == "dismissed") {
		return nil
	}
	if newState == "dismissed" && (currentState == "open" || currentState == "fixed") {
		return nil
	}
	if newState == currentState {
		return nil
	}
	return fmt.Errorf("invalid transition from %q to %q", currentState, newState)
}

func isValidDismissedReason(r string) bool {
	switch CodeScanningDismissedReason(r) {
	case CodeScanningDismissedFalsePositive, CodeScanningDismissedWontFix,
		CodeScanningDismissedUsedInTests, CodeScanningDismissedMitigated:
		return true
	}
	return false
}

// CreateCodeScanningAnalysis records a new analysis run.
func (st *Store) CreateCodeScanningAnalysis(repoKey, ref, commitSHA, analysisKey, category, toolName, toolGUID string) *CodeScanningAnalysis {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.CodeScanningAnalysesByRepo[repoKey] == nil {
		st.CodeScanningAnalysesByRepo[repoKey] = make(map[int]*CodeScanningAnalysis)
	}

	now := st.CurrentTime()
	analysis := &CodeScanningAnalysis{
		ID:          st.NextCodeScanningAnalysisID,
		NodeID:      fmt.Sprintf("CSWA%d", st.NextCodeScanningAnalysisID),
		RepoKey:     repoKey,
		Ref:         ref,
		CommitSHA:   commitSHA,
		AnalysisKey: analysisKey,
		Category:    category,
		ToolName:    toolName,
		ToolGUID:    toolGUID,
		CreatedAt:   now,
	}
	st.NextCodeScanningAnalysisID++

	st.CodeScanningAnalyses[analysis.ID] = analysis
	st.CodeScanningAnalysesByRepo[repoKey][analysis.ID] = analysis
	st.persistCodeScanningAnalysis(analysis)
	return analysis
}

// GetCodeScanningAnalysis returns an analysis by ID scoped to the repo.
func (st *Store) GetCodeScanningAnalysis(repoKey string, id int) *CodeScanningAnalysis {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	a := st.CodeScanningAnalyses[id]
	if a != nil && a.RepoKey != repoKey {
		return nil
	}
	return a
}

// ListCodeScanningAnalyses returns a repo's analyses, optionally filtered by ref
// and tool_name.
func (st *Store) ListCodeScanningAnalyses(repoKey, ref, toolName string) []*CodeScanningAnalysis {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	byRepo := st.CodeScanningAnalysesByRepo[repoKey]
	out := make([]*CodeScanningAnalysis, 0, len(byRepo))
	for _, a := range byRepo {
		if ref != "" && a.Ref != ref {
			continue
		}
		if toolName != "" && a.ToolName != toolName {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return snapshotSlice(out)
}

// DeleteCodeScanningAnalysis removes an analysis from the store.
func (st *Store) DeleteCodeScanningAnalysis(repoKey string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	a := st.CodeScanningAnalyses[id]
	if a == nil || a.RepoKey != repoKey {
		return false
	}
	delete(st.CodeScanningAnalyses, id)
	delete(st.CodeScanningAnalysesByRepo[repoKey], id)
	if st.Persist != nil {
		st.Persist.MustDelete("code_scanning_analyses", strconv.Itoa(id))
	}
	return true
}

// CreateSARIFUpload parses a base64-encoded SARIF payload, creates analyses and
// alerts, and returns the (always "complete") upload record. Every analysis,
// alert, and the upload row commit in one transaction (STORE-001/002).
func (st *Store) CreateSARIFUpload(repoKey string, payload map[string]interface{}) (*SARIFUpload, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	commitSHA, _ := payload["commit_sha"].(string)
	ref, _ := payload["ref"].(string)
	toolNameOverride, _ := payload["tool_name"].(string)

	if commitSHA == "" {
		return nil, fmt.Errorf("commit_sha is required")
	}
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}

	sarifRaw, _ := payload["sarif"].(string)
	var sarif map[string]interface{}
	if sarifRaw != "" {
		decoded, err := base64.StdEncoding.DecodeString(sarifRaw)
		if err != nil {
			return nil, fmt.Errorf("sarif is not valid base64: %w", err)
		}
		if err := json.Unmarshal(decoded, &sarif); err != nil {
			return nil, fmt.Errorf("sarif is not valid JSON: %w", err)
		}
	} else {
		return nil, fmt.Errorf("sarif is required")
	}

	now := st.CurrentTime()
	uploadID := fmt.Sprintf("%s-%d", strings.ReplaceAll(repoKey, "/", "-"), now.UnixNano())
	upload := &SARIFUpload{
		ID:        uploadID,
		RepoKey:   repoKey,
		Status:    "complete",
		CreatedAt: now,
	}

	if st.SARIFUploads == nil {
		st.SARIFUploads = make(map[string]*SARIFUpload)
	}

	batch := NewPersistBatch(st.Persist)
	runs, _ := sarif["runs"].([]interface{})
	for _, rawRun := range runs {
		run, _ := rawRun.(map[string]interface{})
		toolName := extractSARIFToolName(run, toolNameOverride)
		toolGUID := extractSARIFToolGUID(run)
		results, _ := run["results"].([]interface{})
		analysisKey := fmt.Sprintf("%s:%s", toolName, ref)
		category := fmt.Sprintf("%s/%s", toolName, ref)
		analysis := st.createAnalysisAndAlertsLocked(batch, repoKey, ref, commitSHA, analysisKey, category, toolName, toolGUID, run, results)
		analysis.SARIFUploadID = uploadID
		st.persistCodeScanningAnalysisBatchLocked(batch, analysis)
	}

	st.SARIFUploads[uploadID] = upload
	batch.Put("sarif_uploads", uploadID, upload)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "sarif_uploads", Key: uploadID, Err: err})
	}
	return upload, nil
}

func extractSARIFToolName(run map[string]interface{}, fallback string) string {
	tool, _ := run["tool"].(map[string]interface{})
	driver, _ := tool["driver"].(map[string]interface{})
	name, _ := driver["name"].(string)
	if name != "" {
		return name
	}
	return fallback
}

func extractSARIFToolGUID(run map[string]interface{}) string {
	tool, _ := run["tool"].(map[string]interface{})
	driver, _ := tool["driver"].(map[string]interface{})
	guid, _ := driver["guid"].(string)
	return guid
}

// createAnalysisAndAlertsLocked builds one analysis plus its alerts, staging
// every row into batch so the whole SARIF upload persists atomically
// (STORE-001/002). Callers hold st.Mu.
func (st *Store) createAnalysisAndAlertsLocked(batch *PersistBatch, repoKey, ref, commitSHA, analysisKey, category, toolName, toolGUID string, run map[string]interface{}, results []interface{}) *CodeScanningAnalysis {
	if st.CodeScanningAlertsByRepo[repoKey] == nil {
		st.CodeScanningAlertsByRepo[repoKey] = make(map[int]*CodeScanningAlert)
	}
	if st.CodeScanningNextNumber[repoKey] == 0 {
		st.CodeScanningNextNumber[repoKey] = 1
	}
	if st.CodeScanningAnalysesByRepo[repoKey] == nil {
		st.CodeScanningAnalysesByRepo[repoKey] = make(map[int]*CodeScanningAnalysis)
	}

	now := st.CurrentTime()
	analysis := &CodeScanningAnalysis{
		ID:          st.NextCodeScanningAnalysisID,
		NodeID:      fmt.Sprintf("CSWA%d", st.NextCodeScanningAnalysisID),
		RepoKey:     repoKey,
		Ref:         ref,
		CommitSHA:   commitSHA,
		AnalysisKey: analysisKey,
		Category:    category,
		ToolName:    toolName,
		ToolGUID:    toolGUID,
		CreatedAt:   now,
	}
	st.NextCodeScanningAnalysisID++
	st.CodeScanningAnalyses[analysis.ID] = analysis
	st.CodeScanningAnalysesByRepo[repoKey][analysis.ID] = analysis

	ruleSet := make(map[string]struct{})
	for _, r := range results {
		result, _ := r.(map[string]interface{})
		ruleID, _ := result["ruleId"].(string)
		if ruleID == "" {
			ruleID, _ = result["rule_id"].(string)
		}
		ruleSet[ruleID] = struct{}{}
	}
	analysis.ResultsCount = len(results)
	analysis.RulesCount = len(ruleSet)
	st.persistCodeScanningAnalysisBatchLocked(batch, analysis)

	for _, r := range results {
		result, _ := r.(map[string]interface{})
		ruleID, _ := result["ruleId"].(string)
		if ruleID == "" {
			ruleID, _ = result["rule_id"].(string)
		}
		ruleSeverity, ruleDescription, ruleSecurityLevel := sarifRuleMetadata(run, ruleID)
		message := ""
		if msg, ok := result["message"].(map[string]interface{}); ok {
			message, _ = msg["text"].(string)
		}
		if message == "" {
			message, _ = result["message"].(string)
		}
		if ruleDescription == "" {
			ruleDescription = message
		}

		var instances []CodeScanningAlertInstance
		locations, _ := result["locations"].([]interface{})
		for _, loc := range locations {
			instance := codeScanningInstanceFromLocation(loc, ref, analysisKey, category, commitSHA)
			if instance != nil {
				instances = append(instances, *instance)
			}
		}
		if len(instances) == 0 {
			instances = append(instances, CodeScanningAlertInstance{
				Ref:         ref,
				AnalysisKey: analysisKey,
				Category:    category,
				State:       "open",
				CommitSHA:   commitSHA,
				Message:     message,
			})
		}

		number := st.CodeScanningNextNumber[repoKey]
		st.CodeScanningNextNumber[repoKey] = number + 1

		alert := &CodeScanningAlert{
			ID:                    st.NextCodeScanningAlertID,
			NodeID:                fmt.Sprintf("CSWA%d", st.NextCodeScanningAlertID),
			Number:                number,
			RepoKey:               repoKey,
			RuleID:                ruleID,
			RuleSeverity:          ruleSeverity,
			SecuritySeverityLevel: ruleSecurityLevel,
			RuleDescription:       ruleDescription,
			ToolName:              toolName,
			ToolGUID:              toolGUID,
			State:                 "open",
			Instances:             instances,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
		st.NextCodeScanningAlertID++

		// Default severity when the SARIF payload carries no rule metadata.
		if alert.RuleSeverity == "" {
			alert.RuleSeverity = "warning"
		}

		st.CodeScanningAlerts[alert.ID] = alert
		st.CodeScanningAlertsByRepo[repoKey][number] = alert
		st.persistCodeScanningAlertBatchLocked(batch, alert)
	}

	return analysis
}

func sarifRuleMetadata(run map[string]interface{}, ruleID string) (severity, description, securityLevel string) {
	tool, _ := run["tool"].(map[string]interface{})
	driver, _ := tool["driver"].(map[string]interface{})
	rules, _ := driver["rules"].([]interface{})
	for _, raw := range rules {
		rule, _ := raw.(map[string]interface{})
		id, _ := rule["id"].(string)
		if id != ruleID {
			continue
		}
		if full, ok := rule["fullDescription"].(map[string]interface{}); ok {
			description, _ = full["text"].(string)
		}
		if description == "" {
			if short, ok := rule["shortDescription"].(map[string]interface{}); ok {
				description, _ = short["text"].(string)
			}
		}
		if props, ok := rule["properties"].(map[string]interface{}); ok {
			severity, _ = props["problem.severity"].(string)
			if severity == "" {
				severity, _ = props["severity"].(string)
			}
			if score, ok := props["security-severity"].(string); ok {
				securityLevel = securitySeverityLevel(score)
			}
		}
		if severity == "" {
			if cfg, ok := rule["defaultConfiguration"].(map[string]interface{}); ok {
				severity, _ = cfg["level"].(string)
			}
		}
		return severity, description, securityLevel
	}
	return "", "", ""
}

// securitySeverityLevel maps a SARIF security-severity score to GitHub's
// low/medium/high/critical bucket via its CVSS-aligned thresholds. An
// unparseable or empty score yields no level.
func securitySeverityLevel(score string) string {
	value, err := strconv.ParseFloat(strings.TrimSpace(score), 64)
	if err != nil {
		return ""
	}
	switch {
	case value >= 9.0:
		return "critical"
	case value >= 7.0:
		return "high"
	case value >= 4.0:
		return "medium"
	case value >= 0.1:
		return "low"
	default:
		return ""
	}
}

func codeScanningInstanceFromLocation(loc interface{}, ref, analysisKey, category, commitSHA string) *CodeScanningAlertInstance {
	location, _ := loc.(map[string]interface{})
	if location == nil {
		return nil
	}
	physicalLocation, _ := location["physicalLocation"].(map[string]interface{})
	if physicalLocation == nil {
		physicalLocation, _ = location["physical_location"].(map[string]interface{})
	}
	if physicalLocation == nil {
		return nil
	}
	artifactLocation, _ := physicalLocation["artifactLocation"].(map[string]interface{})
	if artifactLocation == nil {
		artifactLocation, _ = physicalLocation["artifact_location"].(map[string]interface{})
	}
	path := ""
	if artifactLocation != nil {
		path, _ = artifactLocation["uri"].(string)
	}
	region, _ := physicalLocation["region"].(map[string]interface{})
	if region == nil {
		return nil
	}
	startLine := intNumber(region["startLine"])
	endLine := intNumber(region["endLine"])
	if endLine == 0 {
		endLine = startLine
	}
	startColumn := intNumber(region["startColumn"])
	endColumn := intNumber(region["endColumn"])

	var message string
	if msg, ok := location["message"].(map[string]interface{}); ok {
		message, _ = msg["text"].(string)
	}

	return &CodeScanningAlertInstance{
		Ref:         ref,
		AnalysisKey: analysisKey,
		Category:    category,
		State:       "open",
		CommitSHA:   commitSHA,
		Path:        path,
		StartLine:   startLine,
		EndLine:     endLine,
		StartColumn: startColumn,
		EndColumn:   endColumn,
		Message:     message,
	}
}

func intNumber(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

// CodeScanningDefaultSetup is one repository's default-setup configuration. A
// repo without a row reports "not-configured".
type CodeScanningDefaultSetup struct {
	RepoKey     string    `json:"repo_key"`
	State       string    `json:"state"` // "configured" or "not-configured"
	QuerySuite  string    `json:"query_suite"`
	Languages   []string  `json:"languages"`
	RunnerType  string    `json:"runner_type,omitempty"`
	RunnerLabel string    `json:"runner_label,omitempty"`
	ThreatModel string    `json:"threat_model,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GetCodeScanningDefaultSetup returns a repo's default-setup configuration, or
// nil when never configured.
func (st *Store) GetCodeScanningDefaultSetup(repoKey string) *CodeScanningDefaultSetup {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// Detach: SetCodeScanningDefaultSetup stamps UpdatedAt in place, so the reader
	// needs an independent snapshot with its own Languages slice (STORE-021).
	s := st.CodeScanningDefaultSetups[repoKey]
	if s == nil {
		return nil
	}
	clone := *s
	clone.Languages = append([]string(nil), s.Languages...)
	return &clone
}

// SetCodeScanningDefaultSetup records a repo's default-setup configuration,
// stamping UpdatedAt.
func (st *Store) SetCodeScanningDefaultSetup(setup *CodeScanningDefaultSetup) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	setup.UpdatedAt = st.CurrentTime()
	st.CodeScanningDefaultSetups[setup.RepoKey] = setup
	if st.Persist != nil {
		st.Persist.MustPut("code_scanning_default_setups", setup.RepoKey, setup)
	}
}

// linguistToCodeQLLanguage maps a Linguist language to the CodeQL default-setup
// language it is analyzed as. Unsupported languages are absent.
var linguistToCodeQLLanguage = map[string]string{
	"Go":         "go",
	"JavaScript": "javascript-typescript",
	"TypeScript": "javascript-typescript",
	"JSX":        "javascript-typescript",
	"TSX":        "javascript-typescript",
	"Python":     "python",
	"Ruby":       "ruby",
	"Java":       "java-kotlin",
	"Kotlin":     "java-kotlin",
	"C":          "c-cpp",
	"C++":        "c-cpp",
	"C#":         "csharp",
	"Swift":      "swift",
}

// DetectCodeQLLanguages derives a repo's sorted CodeQL default-setup languages
// from its default-branch content: the CodeQL mapping of every Linguist-detected
// language, plus "actions" when the repo carries Actions workflow files.
func (st *Store) DetectCodeQLLanguages(repo *Repo) []string {
	set := map[string]bool{}
	for lang := range st.ComputeRepoLanguages(repo) {
		if ql := linguistToCodeQLLanguage[lang]; ql != "" {
			set[ql] = true
		}
	}
	if st.repoHasWorkflowFiles(repo) {
		set["actions"] = true
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// repoHasWorkflowFiles reports whether the repo's default branch carries any
// workflow file under .github/workflows.
func (st *Store) repoHasWorkflowFiles(repo *Repo) bool {
	st.Mu.RLock()
	stor := st.GitStorages[repo.FullName]
	st.Mu.RUnlock()
	if stor == nil {
		return false
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch))
	if err != nil {
		return false
	}
	commit, err := object.GetCommit(stor, ref.Hash())
	if err != nil {
		return false
	}
	tree, err := commit.Tree()
	if err != nil {
		return false
	}
	found := false
	_ = tree.Files().ForEach(func(f *object.File) error {
		if strings.HasPrefix(f.Name, ".github/workflows/") &&
			(strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	return found
}

// GetSARIFUpload returns a SARIF upload by ID.
func (st *Store) GetSARIFUpload(repoKey, id string) *SARIFUpload {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	up := st.SARIFUploads[id]
	if up == nil || up.RepoKey != repoKey {
		return nil
	}
	return up
}

func (st *Store) persistCodeScanningAlert(a *CodeScanningAlert) {
	if st.Persist != nil {
		st.Persist.MustPut("code_scanning_alerts", strconv.Itoa(a.ID), a)
	}
}

func (st *Store) persistCodeScanningAnalysis(a *CodeScanningAnalysis) {
	if st.Persist != nil {
		st.Persist.MustPut("code_scanning_analyses", strconv.Itoa(a.ID), a)
	}
}

// persistCodeScanningAlertBatchLocked stages an alert row into batch (STORE-001/002).
// Callers hold st.Mu.
func (st *Store) persistCodeScanningAlertBatchLocked(batch *PersistBatch, a *CodeScanningAlert) {
	batch.Put("code_scanning_alerts", strconv.Itoa(a.ID), a)
}

// persistCodeScanningAnalysisBatchLocked stages an analysis row into batch
// (STORE-001/002). Callers hold st.Mu.
func (st *Store) persistCodeScanningAnalysisBatchLocked(batch *PersistBatch, a *CodeScanningAnalysis) {
	batch.Put("code_scanning_analyses", strconv.Itoa(a.ID), a)
}

// ListCodeScanningAlertsByOrg returns all alerts for repos owned by the org,
// sorted per GitHub's organization list endpoint.
func (st *Store) ListCodeScanningAlertsByOrg(orgID int, state, severity, toolName, sortField, direction string) []*CodeScanningAlert {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var out []*CodeScanningAlert
	for repoKey, byNumber := range st.CodeScanningAlertsByRepo {
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != orgID {
			continue
		}
		for _, a := range byNumber {
			if state != "" && a.State != CodeScanningState(state) {
				continue
			}
			if severity != "" && a.RuleSeverity != severity {
				continue
			}
			if toolName != "" && a.ToolName != toolName {
				continue
			}
			out = append(out, a)
		}
	}

	if sortField == "" {
		sortField = "created"
	}
	if direction == "" {
		direction = "desc"
	}
	sortAlertList(out, sortField, direction,
		func(a *CodeScanningAlert) time.Time { return a.CreatedAt },
		func(a *CodeScanningAlert) time.Time { return a.UpdatedAt })
	return snapshotCodeScanningAlerts(out)
}

// CodeScanningAutofix is a Copilot Autofix suggestion for one alert. GitHub
// generates it asynchronously; bleephub does so synchronously, so a created
// autofix is immediately in "success" status.
type CodeScanningAutofix struct {
	RepoKey     string    `json:"repo_key"`
	AlertNumber int       `json:"alert_number"`
	Status      string    `json:"status"` // pending | error | success | outdated
	Description string    `json:"description"`
	StartedAt   time.Time `json:"started_at"`
}

// autofixKey keys Store.CodeScanningAutofixes and the persistence bucket.
// The unit separator cannot appear in an "owner/repo" key.
func autofixKey(repoKey string, number int) string {
	return repoKey + "\x1f" + strconv.Itoa(number)
}

// GetCodeScanningAutofix returns the autofix for an alert, or nil.
func (st *Store) GetCodeScanningAutofix(repoKey string, number int) *CodeScanningAutofix {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// Detached value copy so a future in-place status update can't race a reader
	// (STORE-021).
	a := st.CodeScanningAutofixes[autofixKey(repoKey, number)]
	if a == nil {
		return nil
	}
	clone := *a
	return &clone
}

// CreateCodeScanningAutofix generates and stores the autofix for an alert.
// created is false when one already existed, returned unchanged.
func (st *Store) CreateCodeScanningAutofix(a *CodeScanningAlert) (*CodeScanningAutofix, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	key := autofixKey(a.RepoKey, a.Number)
	if existing := st.CodeScanningAutofixes[key]; existing != nil {
		return existing, false
	}

	// A malformed or partially-loaded alert (no instances) must not panic under
	// the write lock.
	var inst CodeScanningAlertInstance
	if len(a.Instances) > 0 {
		inst = a.Instances[len(a.Instances)-1]
	}
	fix := &CodeScanningAutofix{
		RepoKey:     a.RepoKey,
		AlertNumber: a.Number,
		Status:      "success",
		Description: fmt.Sprintf("Remediates %s at %s:%d.", a.RuleID, inst.Path, inst.StartLine),
		StartedAt:   st.CurrentTime(),
	}
	st.CodeScanningAutofixes[key] = fix
	if st.Persist != nil {
		st.Persist.MustPut("code_scanning_autofixes", key, fix)
	}
	return fix, true
}

// CodeQL databases

// CodeQLDatabase is a CodeQL database for one repo + language pair. StoragePath
// points at the durable archive bytes; Content is used only by non-persistent
// in-memory stores.
type CodeQLDatabase struct {
	ID          int       `json:"id"`
	RepoKey     string    `json:"repo_key"`
	Name        string    `json:"name"`
	Language    string    `json:"language"`
	UploaderID  int       `json:"uploader_id"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	StoragePath string    `json:"storage_path,omitempty"`
	Content     []byte    `json:"-"`
	CommitOID   string    `json:"commit_oid"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UpsertCodeQLDatabase creates or replaces the CodeQL database for a repo +
// language, as a new upload supersedes the previous one on GitHub.
func (st *Store) UpsertCodeQLDatabase(repoKey, language, name, contentType, commitOID string, content []byte, uploaderID int) (*CodeQLDatabase, error) {
	sum := sha256.Sum256(content)
	return st.UpsertCodeQLDatabaseStream(repoKey, language, name, contentType, commitOID, bytes.NewReader(content), int64(len(content)), sum[:], uploaderID)
}

// UpsertCodeQLDatabaseStream stores a CodeQL database from a reader whose size
// and SHA-256 the caller already computed (a handler stages the ZIP to a temp
// file and validates it there), so a multi-GB database never lands whole on the
// heap. r must be positioned at the start.
func (st *Store) UpsertCodeQLDatabaseStream(repoKey, language, name, contentType, commitOID string, r io.Reader, size int64, sum []byte, uploaderID int) (*CodeQLDatabase, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.Persist != nil && st.ObjectByteStore == nil {
		return nil, fmt.Errorf("CodeQL database byte storage requires BLEEPHUB_OBJECT_S3_BUCKET when persistence is enabled")
	}

	now := st.CurrentTime()
	if st.CodeQLDatabasesByRepo[repoKey] == nil {
		st.CodeQLDatabasesByRepo[repoKey] = make(map[string]*CodeQLDatabase)
	}
	previous := st.CodeQLDatabasesByRepo[repoKey][language]
	id := st.NextCodeQLDatabaseID
	createdAt := now
	if previous != nil {
		id = previous.ID
		createdAt = previous.CreatedAt
	}
	storagePath := CodeQLDatabaseDataKeyHashed(id, sum)
	var memContent []byte
	if st.ObjectByteStore != nil {
		if err := st.ObjectByteStore.PutStreamHashed(context.Background(), storagePath, r, size, sum); err != nil {
			return nil, fmt.Errorf("write CodeQL database bytes: %w", err)
		}
	} else {
		// No object store (in-memory dev server): keep the bytes with the record.
		buffered, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read CodeQL database bytes: %w", err)
		}
		memContent = buffered
	}
	db := &CodeQLDatabase{
		ID:          id,
		RepoKey:     repoKey,
		Name:        name,
		Language:    language,
		UploaderID:  uploaderID,
		ContentType: contentType,
		Size:        size,
		StoragePath: storagePath,
		CommitOID:   commitOID,
		CreatedAt:   createdAt,
		UpdatedAt:   now,
	}
	if st.ObjectByteStore != nil {
		db.Content = nil
	} else {
		db.Content = memContent
	}
	if st.Persist != nil {
		if err := st.Persist.Put("codeql_databases", strconv.Itoa(db.ID), db); err != nil {
			cleanupErr := st.deleteCodeQLDatabaseDataLocked(db)
			if cleanupErr != nil {
				return nil, fmt.Errorf("persist CodeQL database metadata: %w; cleanup new archive: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("persist CodeQL database metadata: %w", err)
		}
	}
	if previous != nil && previous.StoragePath != "" && previous.StoragePath != storagePath && st.ObjectByteStore != nil {
		if err := st.deleteCodeQLDatabaseDataLocked(previous); err != nil {
			if st.Persist != nil {
				if rollbackErr := st.Persist.Put("codeql_databases", strconv.Itoa(previous.ID), previous); rollbackErr != nil {
					st.CodeQLDatabases[db.ID] = db
					st.CodeQLDatabasesByRepo[repoKey][language] = db
					return nil, fmt.Errorf("delete replaced CodeQL database archive: %w; rollback metadata: %v", err, rollbackErr)
				}
			}
			cleanupErr := st.deleteCodeQLDatabaseDataLocked(db)
			if cleanupErr != nil {
				return nil, fmt.Errorf("delete replaced CodeQL database archive: %w; cleanup replacement archive: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("delete replaced CodeQL database archive: %w", err)
		}
	}
	if previous == nil {
		st.NextCodeQLDatabaseID++
	}
	st.CodeQLDatabases[db.ID] = db
	st.CodeQLDatabasesByRepo[repoKey][language] = db
	return db, nil
}

// GetCodeQLDatabase returns the live row, not a snapshot: a stored database is
// write-once (Upsert swaps a fresh row under the write lock), so a reader never
// races a writer, and its Content blob would be needlessly copied on every read
// (STORE-021 documented exception).
func (st *Store) GetCodeQLDatabase(repoKey, language string) *CodeQLDatabase {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.CodeQLDatabasesByRepo[repoKey][language]
}

// ListCodeQLDatabases returns a repo's CodeQL databases sorted by language,
// as live rows for the same write-once reason as GetCodeQLDatabase (STORE-021
// documented exception).
func (st *Store) ListCodeQLDatabases(repoKey string) []*CodeQLDatabase {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	m := st.CodeQLDatabasesByRepo[repoKey]
	langs := make([]string, 0, len(m))
	for l := range m {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	out := make([]*CodeQLDatabase, 0, len(langs))
	for _, l := range langs {
		out = append(out, m[l])
	}
	return out
}

// ReadCodeQLDatabaseContent reads the archive bytes for a CodeQL database.
func (st *Store) ReadCodeQLDatabaseContent(ctx context.Context, db *CodeQLDatabase) ([]byte, error) {
	if db == nil {
		return nil, fmt.Errorf("CodeQL database is nil")
	}
	if st.ObjectByteStore != nil {
		return st.ObjectByteStore.Get(ctx, db.StoragePath)
	}
	if db.StoragePath != "" && db.Content == nil && db.Size > 0 {
		return nil, fmt.Errorf("CodeQL database bytes require configured object storage")
	}
	return append([]byte(nil), db.Content...), nil
}

// DeleteCodeQLDatabase removes the CodeQL database for a repo + language.
func (st *Store) DeleteCodeQLDatabase(repoKey, language string) (bool, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	db := st.CodeQLDatabasesByRepo[repoKey][language]
	if db == nil {
		return false, nil
	}
	if st.Persist != nil {
		if err := st.Persist.Delete("codeql_databases", strconv.Itoa(db.ID)); err != nil {
			return true, fmt.Errorf("delete CodeQL database metadata: %w", err)
		}
	}
	if err := st.deleteCodeQLDatabaseDataLocked(db); err != nil {
		if st.Persist != nil {
			if rollbackErr := st.Persist.Put("codeql_databases", strconv.Itoa(db.ID), db); rollbackErr != nil {
				return true, fmt.Errorf("delete CodeQL database bytes: %w; rollback metadata: %v", err, rollbackErr)
			}
		}
		return true, err
	}
	delete(st.CodeQLDatabasesByRepo[repoKey], language)
	delete(st.CodeQLDatabases, db.ID)
	return true, nil
}

func (st *Store) deleteCodeQLDatabaseDataLocked(db *CodeQLDatabase) error {
	if db == nil || db.StoragePath == "" || st.ObjectByteStore == nil {
		return nil
	}
	if err := st.ObjectByteStore.Delete(context.Background(), db.StoragePath); err != nil {
		return fmt.Errorf("delete CodeQL database bytes %s: %w", db.StoragePath, err)
	}
	return nil
}

// CodeQL variant analyses

// CodeScanningAnalysisStatus is a CodeQL variant-analysis repo task's status;
// GitHub emits only these six values.
type CodeScanningAnalysisStatus string

const (
	CSAnalysisPending    CodeScanningAnalysisStatus = "pending"
	CSAnalysisInProgress CodeScanningAnalysisStatus = "in_progress"
	CSAnalysisSucceeded  CodeScanningAnalysisStatus = "succeeded"
	CSAnalysisFailed     CodeScanningAnalysisStatus = "failed"
	CSAnalysisCanceled   CodeScanningAnalysisStatus = "canceled"
	CSAnalysisTimedOut   CodeScanningAnalysisStatus = "timed_out"
)

// CodeQLVariantAnalysisRepoTask is the per-repository result row of a
// variant analysis.
type CodeQLVariantAnalysisRepoTask struct {
	RepoID            int                        `json:"repo_id"`
	FullName          string                     `json:"full_name"`
	AnalysisStatus    CodeScanningAnalysisStatus `json:"analysis_status"`
	ResultCount       int                        `json:"result_count"`
	DatabaseCommitSHA string                     `json:"database_commit_sha"`
}

// CodeQLVariantAnalysis is a multi-repository variant analysis for a CodeQL
// query pack. StoragePath points at the durable query-pack tarball; QueryPack is
// used only by non-persistent in-memory stores. GitHub runs the query via an
// Actions workflow; bleephub resolves targets synchronously (a repo is queryable
// only with a CodeQL database for the language) and completes immediately.
type CodeQLVariantAnalysis struct {
	ID                  int                             `json:"id"`
	ControllerRepoKey   string                          `json:"controller_repo_key"`
	ActorID             int                             `json:"actor_id"`
	QueryLanguage       string                          `json:"query_language"`
	QueryPack           string                          `json:"-"`
	QueryPackSize       int64                           `json:"query_pack_size"`
	StoragePath         string                          `json:"storage_path,omitempty"`
	Status              string                          `json:"status"` // in_progress | succeeded | failed | cancelled
	FailureReason       string                          `json:"failure_reason"`
	ScannedRepositories []CodeQLVariantAnalysisRepoTask `json:"scanned_repositories"`
	NotFoundRepos       []string                        `json:"not_found_repos"`    // full names
	NoCodeQLDBRepos     []int                           `json:"no_codeql_db_repos"` // repo IDs
	CreatedAt           time.Time                       `json:"created_at"`
	UpdatedAt           time.Time                       `json:"updated_at"`
	CompletedAt         *time.Time                      `json:"completed_at"`
}

// CreateCodeQLVariantAnalysis resolves the requested repos and stores a
// completed variant analysis. Nonexistent repos go to NotFoundRepos, those
// without a CodeQL database for the language to NoCodeQLDBRepos, the rest are
// scanned; when none is scannable the analysis fails with no_repos_queried.
func (st *Store) CreateCodeQLVariantAnalysis(controllerRepoKey string, actorID int, language string, queryPack []byte, repoFullNames []string) (*CodeQLVariantAnalysis, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.Persist != nil && st.ObjectByteStore == nil {
		return nil, fmt.Errorf("CodeQL variant-analysis query-pack byte storage requires BLEEPHUB_OBJECT_S3_BUCKET when persistence is enabled")
	}

	now := st.CurrentTime()
	id := st.NextCodeQLVariantAnalysisID
	storagePath := CodeQLVariantAnalysisQueryPackDataKey(id)
	if st.ObjectByteStore != nil {
		if err := st.ObjectByteStore.Put(context.Background(), storagePath, queryPack); err != nil {
			return nil, fmt.Errorf("write CodeQL variant-analysis query-pack bytes: %w", err)
		}
	}
	va := &CodeQLVariantAnalysis{
		ID:                id,
		ControllerRepoKey: controllerRepoKey,
		ActorID:           actorID,
		QueryLanguage:     language,
		QueryPackSize:     int64(len(queryPack)),
		StoragePath:       storagePath,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if st.ObjectByteStore == nil {
		va.QueryPack = base64.StdEncoding.EncodeToString(queryPack)
	}
	st.NextCodeQLVariantAnalysisID++

	for _, full := range repoFullNames {
		repo := st.ReposByName[full]
		if repo == nil {
			va.NotFoundRepos = append(va.NotFoundRepos, full)
			continue
		}
		db := st.CodeQLDatabasesByRepo[full][language]
		if db == nil {
			va.NoCodeQLDBRepos = append(va.NoCodeQLDBRepos, repo.ID)
			continue
		}
		va.ScannedRepositories = append(va.ScannedRepositories, CodeQLVariantAnalysisRepoTask{
			RepoID:            repo.ID,
			FullName:          full,
			AnalysisStatus:    CSAnalysisSucceeded,
			DatabaseCommitSHA: db.CommitOID,
		})
	}

	completed := now
	va.CompletedAt = &completed
	if len(va.ScannedRepositories) == 0 {
		va.Status = "failed"
		va.FailureReason = "no_repos_queried"
	} else {
		va.Status = "succeeded"
	}

	st.CodeQLVariantAnalyses[va.ID] = va
	if st.Persist != nil {
		st.Persist.MustPut("codeql_variant_analyses", strconv.Itoa(va.ID), va)
	}
	return va, nil
}

// ListRepoFullNamesByOwner returns the sorted full names of every repo owned by
// the login. Backs the CodeQL variant-analysis repository_owners selector.
func (st *Store) ListRepoFullNamesByOwner(owner string) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	prefix := owner + "/"
	var out []string
	for full := range st.ReposByName {
		if strings.HasPrefix(full, prefix) {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out
}

// GetCodeQLVariantAnalysis returns a variant analysis scoped to its controller repo.
func (st *Store) GetCodeQLVariantAnalysis(controllerRepoKey string, id int) *CodeQLVariantAnalysis {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	va := st.CodeQLVariantAnalyses[id]
	if va == nil || va.ControllerRepoKey != controllerRepoKey {
		return nil
	}
	return va
}

// ReadCodeQLVariantAnalysisQueryPack reads the uploaded query-pack tarball for
// a CodeQL variant analysis.
func (st *Store) ReadCodeQLVariantAnalysisQueryPack(ctx context.Context, va *CodeQLVariantAnalysis) ([]byte, error) {
	if va == nil {
		return nil, fmt.Errorf("CodeQL variant analysis is nil")
	}
	if st.ObjectByteStore != nil {
		return st.ObjectByteStore.Get(ctx, va.StoragePath)
	}
	if va.StoragePath != "" && va.QueryPack == "" && va.QueryPackSize > 0 {
		return nil, fmt.Errorf("CodeQL variant-analysis query-pack bytes require configured object storage")
	}
	pack, err := base64.StdEncoding.DecodeString(va.QueryPack)
	if err != nil {
		return nil, fmt.Errorf("stored query pack is corrupt: %w", err)
	}
	return pack, nil
}
