package bleephub

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func newInviteCodeE() (string, error) {
	code, err := store.RandomHex(6)
	if err != nil {
		return "", fmt.Errorf("generate Classroom invite code: %w", err)
	}
	return code, nil
}

// GitHub Classroom REST surface: read-only and org-admin scoped, matching the
// public API (Classroom has no create REST endpoints; writes are browser-only).
func (s *Server) registerGHClassroomRoutes() {
	s.route("GET /api/v3/classrooms", s.classroomLocked(s.handleListClassrooms))
	s.route("GET /api/v3/classrooms/{classroom_id}", s.classroomLocked(s.handleGetClassroom))
	s.route("GET /api/v3/classrooms/{classroom_id}/assignments", s.classroomLocked(s.handleListClassroomAssignments))
	s.route("GET /api/v3/assignments/{assignment_id}", s.classroomLocked(s.handleGetClassroomAssignment))
	s.route("GET /api/v3/assignments/{assignment_id}/accepted_assignments", s.classroomLocked(s.handleListClassroomAcceptedAssignments))
	s.route("GET /api/v3/assignments/{assignment_id}/grades", s.classroomLocked(s.handleListClassroomAssignmentGrades))

}

func (s *Server) classroomLocked(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.classroomMu.Lock()
		defer s.classroomMu.Unlock()
		next(w, r)
	}
}

func classroomURL(baseURL string, c *store.Classroom) string {
	return baseURL + "/ui/classrooms/" + strconv.Itoa(c.ID)
}

func simpleClassroomJSON(c *store.Classroom, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"id":       c.ID,
		"name":     c.Name,
		"archived": c.Archived,
		"url":      classroomURL(baseURL, c),
	}
}

// classroomJSON renders the `classroom` shape: simple-classroom plus the
// owning organization.
func (s *Server) classroomJSON(c *store.Classroom, baseURL string) map[string]interface{} {
	out := simpleClassroomJSON(c, baseURL)
	org := s.store.GetOrgByID(c.OrgID)
	if org == nil {
		return nil
	}
	out["organization"] = map[string]interface{}{
		"id":         org.ID,
		"login":      org.Login,
		"node_id":    org.NodeID,
		"html_url":   baseURL + "/" + org.Login,
		"name":       nullOrString(org.Name),
		"avatar_url": org.AvatarURL,
	}
	return out
}

func simpleClassroomRepositoryJSON(repo *store.Repo, baseURL string) map[string]interface{} {
	if repo == nil {
		return nil
	}
	return map[string]interface{}{
		"id":             repo.ID,
		"full_name":      repo.FullName,
		"html_url":       baseURL + "/" + repo.FullName,
		"node_id":        repo.NodeID,
		"private":        repo.Private,
		"default_branch": repo.DefaultBranch,
	}
}

func (s *Server) classroomAssignmentCounters(assignmentID int) (accepted, submitted, passing int) {
	a := s.store.GetClassroomAssignment(assignmentID)
	for _, aa := range s.store.ClassroomAcceptedFor(assignmentID) {
		accepted++
		state := s.classroomAcceptedState(a, aa)
		if state.submitted {
			submitted++
		}
		if state.passing {
			passing++
		}
	}
	return
}

type classroomAcceptedDerivedState struct {
	submitted   bool
	passing     bool
	commitCount int
	grade       string
	awarded     int
	available   int
	submittedAt time.Time
}

// classroomAcceptedState derives Classroom reporting purely from the student
// repository and completed Actions jobs, never from asserted state.
func (s *Server) classroomAcceptedState(a *store.ClassroomAssignment, aa *store.ClassroomAcceptedAssignment) classroomAcceptedDerivedState {
	acceptedAt := classroomAcceptedAt(aa)
	state := classroomAcceptedDerivedState{submittedAt: acceptedAt}
	if a == nil {
		return state
	}
	for _, test := range a.AutogradingTests {
		state.available += test.Points
	}
	repo := s.store.GetRepoByID(aa.RepoID)
	if repo == nil {
		state.grade = fmt.Sprintf("%d/%d", state.awarded, state.available)
		return state
	}
	if commits, ok := s.defaultBranchCommits(repo); ok {
		for _, commit := range commits {
			if aa.BaselineSHA != "" && commit.Hash.String() == aa.BaselineSHA {
				break
			}
			state.commitCount++
			if commit.Committer.When.After(state.submittedAt) {
				state.submittedAt = commit.Committer.When
			}
		}
	}
	state.submitted = a.Deadline != nil && !s.currentTime().Before(*a.Deadline)

	jobResults := map[string]store.Result{}
	s.store.Mu.RLock()
	var latest *store.Workflow
	for _, workflow := range s.store.Workflows {
		if workflow.RepoFullName == repo.FullName && workflow.Status == store.WorkflowStatusCompleted && (latest == nil || workflow.CreatedAt.After(latest.CreatedAt)) {
			latest = workflow
		}
	}
	if latest != nil {
		for key, job := range latest.Jobs {
			if job.Status == store.JobStatusCompleted {
				jobResults[key] = job.Result
			}
		}
	}
	s.store.Mu.RUnlock()
	for index, test := range a.AutogradingTests {
		if jobResults[fmt.Sprintf("autograding-%d", index+1)] == store.ResultSuccess {
			state.awarded += test.Points
		}
	}
	state.passing = state.available > 0 && state.awarded == state.available
	state.grade = fmt.Sprintf("%d/%d", state.awarded, state.available)
	return state
}

func classroomAcceptedAt(accepted *store.ClassroomAcceptedAssignment) time.Time {
	if !accepted.AcceptedAt.IsZero() {
		return accepted.AcceptedAt
	}
	return accepted.SubmittedAt
}

// classroomAssignmentJSON renders `classroom-assignment` (full classroom +
// starter code repo) when full, else `simple-classroom-assignment`.
func (s *Server) classroomAssignmentJSON(a *store.ClassroomAssignment, baseURL string, full bool) map[string]interface{} {
	accepted, submitted, passing := s.classroomAssignmentCounters(a.ID)
	var deadline interface{}
	if a.Deadline != nil {
		deadline = a.Deadline.UTC().Format(time.RFC3339)
	}
	var maxTeams, maxMembers interface{}
	if a.MaxTeams != nil {
		maxTeams = *a.MaxTeams
	}
	if a.MaxMembers != nil {
		maxMembers = *a.MaxMembers
	}
	classroom := s.store.GetClassroom(a.ClassroomID)
	out := map[string]interface{}{
		"id":                             a.ID,
		"public_repo":                    a.PublicRepo,
		"title":                          a.Title,
		"type":                           a.Type,
		"invite_link":                    baseURL + "/a/" + a.InviteCode,
		"invitations_enabled":            a.InvitationsEnabled,
		"slug":                           a.Slug,
		"students_are_repo_admins":       a.StudentsAreRepoAdmins,
		"feedback_pull_requests_enabled": a.FeedbackPullRequestsEnabled,
		"max_teams":                      maxTeams,
		"max_members":                    maxMembers,
		"editor":                         a.Editor,
		"accepted":                       accepted,
		"submitted":                      submitted,
		"passing":                        passing,
		"language":                       a.Language,
		"deadline":                       deadline,
	}
	if full {
		out["classroom"] = s.classroomJSON(classroom, baseURL)
		out["starter_code_repository"] = simpleClassroomRepositoryJSON(s.store.GetRepoByID(a.StarterCodeRepoID), baseURL)
	} else {
		out["classroom"] = simpleClassroomJSON(classroom, baseURL)
	}
	return out
}

// --- Read handlers ---

func (s *Server) handleListClassrooms(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	s.store.Mu.RLock()
	all := make([]*store.Classroom, 0, len(s.store.Classrooms))
	for _, c := range s.store.Classrooms {
		all = append(all, c)
	}
	s.store.Mu.RUnlock()
	classrooms := make([]*store.Classroom, 0, len(all))
	for _, c := range all {
		org := s.store.GetOrgByID(c.OrgID)
		if org != nil && (user.SiteAdmin || s.viewerCanAdminOrg(r.Context(), org.Login)) {
			classrooms = append(classrooms, c)
		}
	}
	sort.Slice(classrooms, func(i, j int) bool { return classrooms[i].ID < classrooms[j].ID })

	page := paginateAndLink(w, r, classrooms)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, c := range page {
		out = append(out, simpleClassroomJSON(c, base))
	}
	writeJSON(w, http.StatusOK, out)
}

// classroomForAdmin resolves a classroom, returning 404 to callers who do not
// administer its owning org (these are org-admin endpoints, not public).
func (s *Server) classroomForAdmin(w http.ResponseWriter, r *http.Request, id int) *store.Classroom {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return nil
	}
	c := s.store.GetClassroom(id)
	if c == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	org := s.store.GetOrgByID(c.OrgID)
	if org == nil || (!user.SiteAdmin && !s.viewerCanAdminOrg(r.Context(), org.Login)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return c
}

func (s *Server) classroomAssignmentForAdmin(w http.ResponseWriter, r *http.Request, id int) *store.ClassroomAssignment {
	a := s.store.GetClassroomAssignment(id)
	if a == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if s.classroomForAdmin(w, r, a.ClassroomID) == nil {
		return nil
	}
	return a
}

func (s *Server) handleGetClassroom(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("classroom_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.classroomForAdmin(w, r, id)
	if c == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.classroomJSON(c, s.baseURL(r)))
}

func (s *Server) handleListClassroomAssignments(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("classroom_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := s.classroomForAdmin(w, r, id)
	if c == nil {
		return
	}
	s.store.Mu.RLock()
	assignments := make([]*store.ClassroomAssignment, 0)
	for _, a := range s.store.ClassroomAssignments {
		if a.ClassroomID == c.ID {
			assignments = append(assignments, a)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(assignments, func(i, j int) bool { return assignments[i].ID < assignments[j].ID })

	page := paginateAndLink(w, r, assignments)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, a := range page {
		out = append(out, s.classroomAssignmentJSON(a, base, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetClassroomAssignment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("assignment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	a := s.classroomAssignmentForAdmin(w, r, id)
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.classroomAssignmentJSON(a, s.baseURL(r), true))
}

func (s *Server) handleListClassroomAcceptedAssignments(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("assignment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	a := s.classroomAssignmentForAdmin(w, r, id)
	if a == nil {
		return
	}
	accepted := s.store.ClassroomAcceptedFor(a.ID)
	page := paginateAndLink(w, r, accepted)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, aa := range page {
		state := s.classroomAcceptedState(a, aa)
		students := make([]map[string]interface{}, 0, len(aa.Students))
		for _, cs := range aa.Students {
			u := s.store.GetUserByID(cs.UserID)
			if u == nil {
				writeGHError(w, http.StatusInternalServerError, "Classroom student record references a missing user")
				return
			}
			students = append(students, map[string]interface{}{
				"id":         u.ID,
				"login":      u.Login,
				"avatar_url": u.AvatarURL,
				"html_url":   base + "/" + u.Login,
			})
		}
		repo := s.store.GetRepoByID(aa.RepoID)
		if repo == nil {
			writeGHError(w, http.StatusInternalServerError, "Classroom acceptance references a missing repository")
			return
		}
		out = append(out, map[string]interface{}{
			"id":           aa.ID,
			"submitted":    state.submitted,
			"passing":      state.passing,
			"commit_count": state.commitCount,
			"grade":        state.grade,
			"students":     students,
			"repository":   simpleClassroomRepositoryJSON(repo, base),
			"assignment":   s.classroomAssignmentJSON(a, base, false),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListClassroomAssignmentGrades(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("assignment_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	a := s.classroomAssignmentForAdmin(w, r, id)
	if a == nil {
		return
	}
	base := s.baseURL(r)
	starter := s.store.GetRepoByID(a.StarterCodeRepoID)
	if starter == nil {
		writeGHError(w, http.StatusInternalServerError, "Classroom assignment references a missing starter repository")
		return
	}
	assignmentURL := base + "/a/" + a.InviteCode

	out := make([]map[string]interface{}, 0)
	for _, aa := range s.store.ClassroomAcceptedFor(a.ID) {
		repo := s.store.GetRepoByID(aa.RepoID)
		if repo == nil {
			writeGHError(w, http.StatusInternalServerError, "Classroom acceptance references a missing repository")
			return
		}
		state := s.classroomAcceptedState(a, aa)
		for _, cs := range aa.Students {
			u := s.store.GetUserByID(cs.UserID)
			if u == nil {
				writeGHError(w, http.StatusInternalServerError, "Classroom acceptance references a missing user")
				return
			}
			row := map[string]interface{}{
				"assignment_name":         a.Title,
				"assignment_url":          assignmentURL,
				"starter_code_url":        base + "/" + starter.FullName,
				"github_username":         u.Login,
				"roster_identifier":       cs.RosterIdentifier,
				"student_repository_name": repo.Name,
				"student_repository_url":  base + "/" + repo.FullName,
				"submission_timestamp":    state.submittedAt.UTC().Format(time.RFC3339),
				"points_awarded":          state.awarded,
				"points_available":        state.available,
			}
			if a.Type == "group" {
				row["group_name"] = aa.GroupName
			}
			out = append(out, row)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
