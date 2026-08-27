package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/gitstore"
)

// SecurityAdvisory is a repository-scoped security advisory.
type SecurityAdvisory struct {
	ID                     int                             `json:"id"`
	NodeID                 string                          `json:"node_id"`
	GHSAID                 string                          `json:"ghsa_id"`
	RepoID                 int                             `json:"repo_id"`
	AuthorID               int                             `json:"author_id"`
	Title                  string                          `json:"title,omitempty"`
	Summary                string                          `json:"summary"`
	Description            string                          `json:"description"`
	Severity               string                          `json:"severity"`
	CVSSScore              float64                         `json:"cvss_score"`
	CVSSVector             string                          `json:"cvss_vector"`
	CWEs                   []string                        `json:"cwes"`
	State                  string                          `json:"state"`
	CreatedAt              time.Time                       `json:"created_at"`
	UpdatedAt              time.Time                       `json:"updated_at"`
	PublishedAt            *time.Time                      `json:"published_at,omitempty"`
	CVEID                  string                          `json:"cve_id"`
	HTMLURL                string                          `json:"html_url"`
	URL                    string                          `json:"url"`
	SubmissionAccepted     bool                            `json:"submission_accepted"`
	PrivateForkID          int                             `json:"private_fork_id"`
	CollaboratingUsers     []string                        `json:"collaborating_users,omitempty"`
	CollaboratingTeams     []string                        `json:"collaborating_teams,omitempty"`
	VulnerableVersionRange string                          `json:"vulnerable_version_range"`
	Vulnerabilities        []SecurityAdvisoryVulnerability `json:"vulnerabilities,omitempty"`
	Credits                []SecurityAdvisoryCredit        `json:"credits,omitempty"`
}

// SecurityAdvisoryCredit is one credited participant ({login, type}). bleephub
// auto-accepts credits, so no per-credit state is stored; rendered
// credits_detailed state is always "accepted".
type SecurityAdvisoryCredit struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// ValidAdvisoryCreditType reports whether t is a security-advisory-credit-types
// enum value.
func ValidAdvisoryCreditType(t string) bool {
	switch t {
	case "analyst", "finder", "reporter", "coordinator", "remediation_developer",
		"remediation_reviewer", "remediation_verifier", "tool", "sponsor", "other":
		return true
	}
	return false
}

type SecurityAdvisoryVulnerability struct {
	PackageName            string   `json:"package_name"`
	PackageEcosystem       string   `json:"package_ecosystem"`
	VulnerableVersionRange string   `json:"vulnerable_version_range"`
	FirstPatchedVersion    string   `json:"first_patched_version,omitempty"`
	VulnerableFunctions    []string `json:"vulnerable_functions,omitempty"`
}

// SecurityAdvisoryReport records a vulnerability report that spawned an advisory.
type SecurityAdvisoryReport struct {
	ID                     int       `json:"id"`
	AdvisoryID             int       `json:"advisory_id"`
	ReporterID             int       `json:"reporter_id"`
	Summary                string    `json:"summary"`
	Description            string    `json:"description"`
	Severity               string    `json:"severity"`
	CVSSScore              float64   `json:"cvss_score"`
	CVSSVector             string    `json:"cvss_vector"`
	CWEs                   []string  `json:"cwes"`
	VulnerableVersionRange string    `json:"vulnerable_version_range"`
	CreatedAt              time.Time `json:"created_at"`
}

// CreateAdvisoryReq is the request body for creating a security advisory.
type CreateAdvisoryReq struct {
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	// CVEID lets a reporter who already holds a CVE name it at creation.
	CVEID     string  `json:"cve_id"`
	CVSSScore float64 `json:"cvss_score"`
	// CVSSVector must be spelt cvss_vector_string (the member
	// repository-advisory-create/-update use); cvss_vector silently discarded
	// every SDK's vector.
	CVSSVector string   `json:"cvss_vector_string"`
	CWEs       []string `json:"cwe_ids"`
	State      string   `json:"state"`
	// StartPrivateFork requests the temporary private fork for fixing.
	StartPrivateFork       bool   `json:"start_private_fork"`
	VulnerableVersionRange string `json:"vulnerable_version_range"`
	Vulnerabilities        []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		VulnerableVersionRange string   `json:"vulnerable_version_range"`
		FirstPatchedVersion    string   `json:"first_patched_version"`
		PatchedVersions        string   `json:"patched_versions"`
		VulnerableFunctions    []string `json:"vulnerable_functions"`
	} `json:"vulnerabilities"`
	Credits            []SecurityAdvisoryCredit `json:"credits"`
	CollaboratingUsers []string                 `json:"collaborating_users"`
	CollaboratingTeams []string                 `json:"collaborating_teams"`
}

func ValidAdvisorySeverity(s string) bool {
	switch s {
	case "critical", "high", "medium", "low":
		return true
	}
	return false
}

func ValidAdvisoryState(s string) bool {
	switch s {
	case "draft", "triage", "published", "closed", "withdrawn":
		return true
	}
	return false
}

func generateGHSAID() (string, error) {
	h, err := RandomHex(6)
	if err != nil {
		return "", fmt.Errorf("generate GitHub Security Advisory id: %w", err)
	}
	return fmt.Sprintf("GHSA-%s-%s-%s", h[0:4], h[4:8], h[8:12]), nil
}

func generateCVEID(now time.Time) (string, error) {
	b, err := RandomBytes(4)
	if err != nil {
		return "", fmt.Errorf("generate CVE id: %w", err)
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if n < 0 {
		n = -n
	}
	return fmt.Sprintf("CVE-%d-%04d", now.UTC().Year(), n%10000), nil
}

func (st *Store) CreateSecurityAdvisory(repoID, authorID int, req CreateAdvisoryReq) *SecurityAdvisory {
	adv, err := st.CreateSecurityAdvisoryE(repoID, authorID, req)
	if err != nil {
		panic(err)
	}
	return adv
}

func (st *Store) CreateSecurityAdvisoryE(repoID, authorID int, req CreateAdvisoryReq) (*SecurityAdvisory, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil, nil
	}
	if req.Severity == "" {
		req.Severity = "medium"
	}
	if !ValidAdvisorySeverity(req.Severity) {
		return nil, nil
	}
	state := req.State
	if state == "" {
		state = "draft"
	}
	if !ValidAdvisoryState(state) {
		return nil, nil
	}
	ghsaID, err := generateGHSAID()
	if err != nil {
		return nil, err
	}

	now := st.CurrentTime()
	adv := &SecurityAdvisory{
		ID:                     st.NextSecurityAdvisoryID,
		NodeID:                 fmt.Sprintf("GSA_kwCN%07d", st.NextSecurityAdvisoryID),
		GHSAID:                 ghsaID,
		RepoID:                 repoID,
		AuthorID:               authorID,
		Summary:                req.Summary,
		Description:            req.Description,
		Severity:               req.Severity,
		CVEID:                  req.CVEID,
		CVSSScore:              req.CVSSScore,
		CVSSVector:             req.CVSSVector,
		CollaboratingUsers:     append([]string(nil), req.CollaboratingUsers...),
		CollaboratingTeams:     append([]string(nil), req.CollaboratingTeams...),
		CWEs:                   req.CWEs,
		State:                  state,
		CreatedAt:              now,
		UpdatedAt:              now,
		VulnerableVersionRange: req.VulnerableVersionRange,
	}
	if state == "published" {
		adv.PublishedAt = &now
	}
	for _, v := range req.Vulnerabilities {
		if v.Package.Name == "" || v.Package.Ecosystem == "" || v.VulnerableVersionRange == "" {
			continue
		}
		patched := v.FirstPatchedVersion
		if patched == "" {
			patched = v.PatchedVersions
		}
		adv.Vulnerabilities = append(adv.Vulnerabilities, SecurityAdvisoryVulnerability{
			PackageName:            v.Package.Name,
			PackageEcosystem:       v.Package.Ecosystem,
			VulnerableVersionRange: v.VulnerableVersionRange,
			FirstPatchedVersion:    patched,
			VulnerableFunctions:    append([]string(nil), v.VulnerableFunctions...),
		})
	}
	if adv.CWEs == nil {
		adv.CWEs = []string{}
	}
	if len(req.Credits) > 0 {
		adv.Credits = append([]SecurityAdvisoryCredit(nil), req.Credits...)
	}
	st.NextSecurityAdvisoryID++

	if st.SecurityAdvisoriesByRepo[repo.FullName] == nil {
		st.SecurityAdvisoriesByRepo[repo.FullName] = map[string]*SecurityAdvisory{}
	}
	st.SecurityAdvisoriesByRepo[repo.FullName][adv.GHSAID] = adv
	st.SecurityAdvisories[adv.ID] = adv

	st.persistSecurityAdvisory(adv)
	return adv, nil
}

// cloneSecurityAdvisory deep-copies an advisory for handing outside the store
// lock (STORE-021). SecurityAdvisoryVulnerability carries a VulnerableFunctions
// slice, so each element needs its own copy, not just the outer slice.
func cloneSecurityAdvisory(a *SecurityAdvisory) *SecurityAdvisory {
	if a == nil {
		return nil
	}
	clone := *a
	if a.CWEs != nil {
		clone.CWEs = append([]string(nil), a.CWEs...)
	}
	if a.CollaboratingUsers != nil {
		clone.CollaboratingUsers = append([]string(nil), a.CollaboratingUsers...)
	}
	if a.CollaboratingTeams != nil {
		clone.CollaboratingTeams = append([]string(nil), a.CollaboratingTeams...)
	}
	if a.Vulnerabilities != nil {
		clone.Vulnerabilities = make([]SecurityAdvisoryVulnerability, len(a.Vulnerabilities))
		for i, vulnerability := range a.Vulnerabilities {
			clone.Vulnerabilities[i] = vulnerability
			if vulnerability.VulnerableFunctions != nil {
				clone.Vulnerabilities[i].VulnerableFunctions =
					append([]string(nil), vulnerability.VulnerableFunctions...)
			}
		}
	}
	if a.Credits != nil {
		clone.Credits = append([]SecurityAdvisoryCredit(nil), a.Credits...)
	}
	if a.PublishedAt != nil {
		published := *a.PublishedAt
		clone.PublishedAt = &published
	}
	return &clone
}

// ListSecurityAdvisories returns a repo's advisories, newest first.
func (st *Store) ListSecurityAdvisories(repoID int) []*SecurityAdvisory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	out := make([]*SecurityAdvisory, 0, len(st.SecurityAdvisoriesByRepo[repo.FullName]))
	for _, a := range st.SecurityAdvisoriesByRepo[repo.FullName] {
		out = append(out, cloneSecurityAdvisory(a))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return snapshotSecurityAdvisories(out)
}

func (st *Store) GetSecurityAdvisoryByGHSA(repoID int, ghsaID string) *SecurityAdvisory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	return cloneSecurityAdvisory(st.SecurityAdvisoriesByRepo[repo.FullName][ghsaID])
}

// UpdateSecurityAdvisory applies fn to the advisory and persists it.
func (st *Store) UpdateSecurityAdvisory(id int, fn func(*SecurityAdvisory)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	adv := st.SecurityAdvisories[id]
	if adv == nil {
		return false
	}
	fn(adv)
	if adv.CWEs == nil {
		adv.CWEs = []string{}
	}
	adv.UpdatedAt = st.CurrentTime()
	st.persistSecurityAdvisory(adv)
	return true
}

// RequestCVE assigns a CVE ID to the advisory.
func (st *Store) RequestCVE(id int) bool {
	ok, err := st.RequestCVEE(id)
	if err != nil {
		panic(err)
	}
	return ok
}

func (st *Store) RequestCVEE(id int) (bool, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	adv := st.SecurityAdvisories[id]
	if adv == nil || adv.CVEID != "" {
		return false, nil
	}
	cveID, err := generateCVEID(st.CurrentTime())
	if err != nil {
		return false, err
	}
	adv.CVEID = cveID
	adv.UpdatedAt = st.CurrentTime()
	st.persistSecurityAdvisory(adv)
	return true, nil
}

// CreateTemporaryFork creates the private fork maintainers collaborate on.
func (st *Store) CreateTemporaryFork(repoID int, ghsaID string) *Repo {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	byRepo := st.SecurityAdvisoriesByRepo[repo.FullName]
	if byRepo == nil {
		return nil
	}
	adv := byRepo[ghsaID]
	if adv == nil {
		return nil
	}
	author := st.Users[adv.AuthorID]
	if author == nil {
		return nil
	}

	owner := author
	baseName := fmt.Sprintf("%s-%s-%s", owner.Login, repo.Name, strings.ToLower(ghsaID))
	name := baseName
	fullName := owner.Login + "/" + name
	for i := 2; ; i++ {
		if st.RepoByNameLocked(fullName) == nil {
			break
		}
		name = fmt.Sprintf("%s-%d", baseName, i)
		fullName = owner.Login + "/" + name
	}

	sourceID := repo.ID
	if repo.SourceID != 0 {
		sourceID = repo.SourceID
	}

	forkID := st.ReserveGlobalID("next_repo", &st.NextRepo)
	fork := &Repo{
		ID:                        forkID,
		NodeID:                    fmt.Sprintf("R_kgDO%08d", forkID),
		Name:                      name,
		FullName:                  fullName,
		Description:               repo.Description,
		Homepage:                  repo.Homepage,
		DefaultBranch:             repo.DefaultBranch,
		Visibility:                "private",
		Language:                  repo.Language,
		Owner:                     owner,
		OwnerID:                   owner.ID,
		OwnerType:                 "User",
		Private:                   true,
		Fork:                      true,
		ParentID:                  repo.ID,
		SourceID:                  sourceID,
		HasIssues:                 repo.HasIssues,
		HasProjects:               repo.HasProjects,
		HasWiki:                   repo.HasWiki,
		HasDiscussions:            BoolPointer(RepoHasDiscussions(repo)),
		HasPullRequests:           repo.HasPullRequests,
		AllowSquashMerge:          repo.AllowSquashMerge,
		AllowMergeCommit:          repo.AllowMergeCommit,
		AllowRebaseMerge:          repo.AllowRebaseMerge,
		AllowAutoMerge:            repo.AllowAutoMerge,
		AllowUpdateBranch:         repo.AllowUpdateBranch,
		DeleteBranchOnMerge:       repo.DeleteBranchOnMerge,
		UseSquashPRTitleAsDefault: repo.UseSquashPRTitleAsDefault,
		SquashMergeCommitTitle:    repo.SquashMergeCommitTitle,
		SquashMergeCommitMessage:  repo.SquashMergeCommitMessage,
		MergeCommitTitle:          repo.MergeCommitTitle,
		MergeCommitMessage:        repo.MergeCommitMessage,
		PullRequestCreationPolicy: repo.PullRequestCreationPolicy,
		LicenseKey:                repo.LicenseKey,
		LicenseName:               repo.LicenseName,
		LicenseSPDX:               repo.LicenseSPDX,
		Topics:                    append([]string(nil), repo.Topics...),
		Stargazers:                map[int]time.Time{},
		NextIssueNumber:           1,
		NextMilestoneNumber:       1,
		CreatedAt:                 st.CurrentTime(),
		UpdatedAt:                 st.CurrentTime(),
		PushedAt:                  st.CurrentTime(),
	}

	srcStor := st.GitStorages[repo.FullName]
	if srcStor == nil {
		return nil
	}
	stor, err := gitstore.OpenOrInitGitStorage(context.Background(), fullName)
	if err != nil {
		st.Logger.Error().Str("repo", fullName).Err(err).Msg("security advisory fork: open git storage failed")
		return nil
	}
	if err := copyGitStorage(srcStor, stor); err != nil {
		st.Logger.Error().Str("repo", fullName).Err(err).Msg("security advisory fork: copy git storage failed")
		return nil
	}

	st.Repos[fork.ID] = fork
	st.ReposByName[fullName] = fork
	st.IndexRepoNameLocked(fullName)
	st.GitStorages[fullName] = stor
	// The copied storage carries the source's HEAD; point it at the fork's own
	// default branch so clones check that out.
	if err := SetGitHeadBranch(stor, fork.DefaultBranch); err != nil {
		st.Logger.Error().Str("repo", fullName).Err(err).Msg("security advisory fork: could not point git HEAD at the default branch")
	}

	// One transaction: PrivateForkID, the fork repo row, and its discussion
	// categories commit together, so a crash cannot record a fork ID whose repo
	// never landed (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	st.ensureDefaultDiscussionCategoriesBatchLocked(batch, fork.ID)

	adv.PrivateForkID = fork.ID
	batch.Put("security_advisories", strconv.Itoa(adv.ID), adv)
	batch.Put("repos", strconv.Itoa(fork.ID), fork)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "repos", Key: strconv.Itoa(fork.ID), Err: err})
	}
	return fork
}

func (st *Store) CreateSecurityAdvisoryReport(report SecurityAdvisoryReport) *SecurityAdvisoryReport {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	report.ID = st.NextSecurityAdvisoryReportID
	st.NextSecurityAdvisoryReportID++
	st.SecurityAdvisoryReports[report.ID] = &report
	if st.Persist != nil {
		st.Persist.MustPut("security_advisory_reports", strconv.Itoa(report.ID), &report)
	}
	return &report
}

func (st *Store) persistSecurityAdvisory(a *SecurityAdvisory) {
	if st.Persist != nil {
		st.Persist.MustPut("security_advisories", strconv.Itoa(a.ID), a)
	}
}
