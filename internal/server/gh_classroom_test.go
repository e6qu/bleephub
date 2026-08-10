package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestGitHubClassroomSurface(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	fixedNow := time.Date(2035, time.June, 15, 12, 0, 0, 0, time.UTC)
	previousClock := srv.replaceClockNow(func() time.Time { return fixedNow })
	t.Cleanup(func() { srv.replaceClockNow(previousClock) })
	org := srv.createTestOrg(t)
	starterRepo := srv.createTestRepo(t)
	starterRecord := srv.store.GetRepo(starterRepo.owner, starterRepo.name)
	if err := srv.initRepoFiles(context.Background(), starterRecord, starterRecord.DefaultBranch, "Classroom starter", "", "", true); err != nil {
		t.Fatalf("initialize starter repository: %v", err)
	}
	student := srv.createTestUser(t, "classroom-student")
	studentToken := srv.store.CreateToken(student.ID, "repo,read:org").Value
	deadline := fixedNow.Add(-time.Minute)

	classroom := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms", defaultToken, map[string]interface{}{
		"name": "Programming Go", "organization": org,
	}), http.StatusCreated)
	classroomID := strconv.Itoa(int(classroom["id"].(float64)))
	decodeJSONWithStatus(t, srv.put(t, "/classroom-data/classrooms/"+classroomID+"/roster", defaultToken, map[string]interface{}{
		"students": []map[string]interface{}{{"login": student.Login, "roster_identifier": "student-1"}},
	}), http.StatusOK)

	assignment := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms/"+classroomID+"/assignments", defaultToken, map[string]interface{}{
		"title":                   "Intro to Binaries",
		"type":                    "individual",
		"starter_code_repository": starterRepo.fullName(),
		"public_repo":             true,
		"invitations_enabled":     true,
		"editor":                  "codespaces",
		"language":                "go",
		"deadline":                deadline,
		"autograding_tests": []map[string]interface{}{
			{"name": "Tests", "command": "go test ./...", "points": 10},
		},
	}), http.StatusCreated)
	assignmentID := strconv.Itoa(int(assignment["id"].(float64)))
	invite := assignment["invite_link"].(string)
	code := invite[strings.LastIndex(invite, "/")+1:]
	acceptance := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/invitations/"+code+"/accept", studentToken, map[string]interface{}{}), http.StatusCreated)
	studentRepo := acceptance["repository"].(map[string]interface{})["full_name"].(string)

	// GET /classrooms
	resp := srv.get(t, "/api/v3/classrooms", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("list classrooms status = %d", resp.StatusCode)
	}
	classrooms := decodeJSONArray(t, resp)
	var listed map[string]interface{}
	for _, c := range classrooms {
		if c["name"] == "Programming Go" {
			listed = c
		}
	}
	if listed == nil {
		t.Fatal("seeded classroom missing from GET /classrooms")
	}
	if listed["archived"] != false || listed["url"] == nil {
		t.Fatalf("classroom shape: %v", listed)
	}

	// GET /classrooms requires authentication on real GitHub.
	resp = srv.get(t, "/api/v3/classrooms", "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated list status = %d, want 401", resp.StatusCode)
	}

	// GET /classrooms/{classroom_id}
	resp = srv.get(t, "/api/v3/classrooms/"+classroomID, defaultToken)
	full := decodeJSONWithStatus(t, resp, 200)
	orgJSON, _ := full["organization"].(map[string]interface{})
	if orgJSON == nil || orgJSON["login"] != org {
		t.Fatalf("classroom organization = %v", full["organization"])
	}

	// GET /classrooms/{classroom_id}/assignments
	resp = srv.get(t, "/api/v3/classrooms/"+classroomID+"/assignments", defaultToken)
	assignments := decodeJSONArray(t, resp)
	if len(assignments) != 1 {
		t.Fatalf("classroom has %d assignments, want 1", len(assignments))
	}
	simple := assignments[0]
	if simple["title"] != "Intro to Binaries" || simple["slug"] != "intro-to-binaries" {
		t.Fatalf("assignment shape: %v", simple)
	}
	// Counters derive from the accepted assignment.
	if simple["accepted"] != float64(1) || simple["submitted"] != float64(1) || simple["passing"] != float64(0) {
		t.Fatalf("assignment counters: accepted=%v submitted=%v passing=%v",
			simple["accepted"], simple["submitted"], simple["passing"])
	}

	// GET /assignments/{assignment_id} (full shape with starter code repo)
	resp = srv.get(t, "/api/v3/assignments/"+assignmentID, defaultToken)
	fullAssignment := decodeJSONWithStatus(t, resp, 200)
	starter, _ := fullAssignment["starter_code_repository"].(map[string]interface{})
	if starter == nil || starter["full_name"] != starterRepo.fullName() {
		t.Fatalf("starter_code_repository = %v", fullAssignment["starter_code_repository"])
	}
	nested, _ := fullAssignment["classroom"].(map[string]interface{})
	if nested == nil || nested["organization"] == nil {
		t.Fatal("full assignment must nest the full classroom")
	}

	// GET /assignments/{assignment_id}/accepted_assignments
	resp = srv.get(t, "/api/v3/assignments/"+assignmentID+"/accepted_assignments", defaultToken)
	accepted := decodeJSONArray(t, resp)
	if len(accepted) != 1 {
		t.Fatalf("accepted assignments = %d, want 1", len(accepted))
	}
	students, _ := accepted[0]["students"].([]interface{})
	if len(students) != 1 {
		t.Fatalf("students = %v", accepted[0]["students"])
	}
	studentJSON, _ := students[0].(map[string]interface{})
	if studentJSON["login"] != student.Login {
		t.Fatalf("student login = %v", studentJSON["login"])
	}
	repoJSON, _ := accepted[0]["repository"].(map[string]interface{})
	if repoJSON == nil || repoJSON["full_name"] != studentRepo {
		t.Fatalf("accepted repository = %v", accepted[0]["repository"])
	}
	if accepted[0]["grade"] != "0/10" || accepted[0]["commit_count"] != float64(0) {
		t.Fatalf("accepted shape: %v", accepted[0])
	}

	// GET /assignments/{assignment_id}/grades
	resp = srv.get(t, "/api/v3/assignments/"+assignmentID+"/grades", defaultToken)
	grades := decodeJSONArray(t, resp)
	if len(grades) != 1 {
		t.Fatalf("grades = %d, want 1", len(grades))
	}
	g := grades[0]
	if g["github_username"] != student.Login || g["roster_identifier"] != "student-1" {
		t.Fatalf("grade identity: %v", g)
	}
	if g["points_awarded"] != float64(0) || g["points_available"] != float64(10) {
		t.Fatalf("grade points: awarded=%v available=%v", g["points_awarded"], g["points_available"])
	}
	if g["assignment_name"] != "Intro to Binaries" {
		t.Fatalf("grade assignment_name = %v", g["assignment_name"])
	}
}

func TestGitHubClassroomNotFound(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	for _, path := range []string{
		"/api/v3/classrooms/999999",
		"/api/v3/assignments/999999",
		"/api/v3/assignments/999999/grades",
		"/api/v3/assignments/999999/accepted_assignments",
	} {
		resp := srv.get(t, path, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestGitHubClassroomRequiresOwningOrganizationAdmin(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	orgLogin := srv.createTestOrg(t)
	classroom := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms", defaultToken, map[string]interface{}{
		"name": "Private Coursework", "organization": orgLogin,
	}), http.StatusCreated)
	classroomID := strconv.Itoa(int(classroom["id"].(float64)))
	starterRepo := srv.createTestRepo(t)
	assignment := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms/"+classroomID+"/assignments", defaultToken, map[string]interface{}{
		"title":                   "Private Assignment",
		"type":                    "individual",
		"starter_code_repository": starterRepo.fullName(),
	}), http.StatusCreated)
	assignmentID := strconv.Itoa(int(assignment["id"].(float64)))

	outsider := srv.createTestUser(t, "classroom-outsider")
	outsiderToken := srv.store.CreateToken(outsider.ID, "read:org").Value

	list := decodeJSONArray(t, srv.get(t, "/api/v3/classrooms", outsiderToken))
	for _, item := range list {
		if item["id"] == classroom["id"] {
			t.Fatalf("outsider list disclosed classroom: %v", item)
		}
	}

	for _, tc := range []struct {
		path  string
		token string
		want  int
	}{
		{"/api/v3/classrooms/" + classroomID, "", 401},
		{"/api/v3/classrooms/" + classroomID, outsiderToken, 404},
		{"/api/v3/classrooms/" + classroomID + "/assignments", outsiderToken, 404},
		{"/api/v3/assignments/" + assignmentID, outsiderToken, 404},
		{"/api/v3/assignments/" + assignmentID + "/accepted_assignments", outsiderToken, 404},
		{"/api/v3/assignments/" + assignmentID + "/grades", outsiderToken, 404},
	} {
		resp := srv.get(t, tc.path, tc.token)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s as token %q = %d, want %d", tc.path, tc.token, resp.StatusCode, tc.want)
		}
	}
}

func TestGitHubClassroomBrowserWorkflowCreatesRealAssignmentRepository(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	orgLogin := srv.createTestOrg(t)
	starterFullName := srv.createTestRepo(t)
	starter := srv.store.GetRepo(starterFullName.owner, starterFullName.name)
	if err := srv.initRepoFiles(context.Background(), starter, starter.DefaultBranch, "Classroom starter", "", "", true); err != nil {
		t.Fatalf("initialize starter repository: %v", err)
	}
	student := srv.createTestUser(t, "classroom-real-student")
	studentToken := srv.store.CreateToken(student.ID, "repo,read:org").Value

	classroom := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms", defaultToken, map[string]interface{}{
		"name":         "Systems Programming",
		"organization": orgLogin,
	}), http.StatusCreated)
	classroomID := strconv.Itoa(int(classroom["id"].(float64)))

	roster := decodeJSONWithStatus(t, srv.put(t, "/classroom-data/classrooms/"+classroomID+"/roster", defaultToken, map[string]interface{}{
		"students": []map[string]interface{}{{"login": student.Login, "roster_identifier": "student@example.edu"}},
	}), http.StatusOK)
	if entries, ok := roster["roster"].([]interface{}); !ok || len(entries) != 1 {
		t.Fatalf("roster = %v", roster["roster"])
	}

	assignment := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms/"+classroomID+"/assignments", defaultToken, map[string]interface{}{
		"title":                          "Pointers and Processes",
		"type":                           "individual",
		"starter_code_repository":        starterFullName.fullName(),
		"feedback_pull_requests_enabled": true,
		"students_are_repo_admins":       true,
		"public_repo":                    false,
	}), http.StatusCreated)
	inviteLink := assignment["invite_link"].(string)
	inviteCode := inviteLink[strings.LastIndex(inviteLink, "/")+1:]

	accepted := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/invitations/"+inviteCode+"/accept", studentToken, map[string]interface{}{}), http.StatusCreated)
	repository := accepted["repository"].(map[string]interface{})
	if repository["full_name"] != orgLogin+"/pointers-and-processes-"+student.Login || repository["private"] != true {
		t.Fatalf("assignment repository = %v", repository)
	}
	repo := srv.store.GetRepo(orgLogin, "pointers-and-processes-"+student.Login)
	if repo == nil || repo.TemplateRepoID != starter.ID {
		t.Fatalf("generated repository = %+v", repo)
	}
	stor := srv.store.GetGitStorage(orgLogin, repo.Name)
	if _, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch)); err != nil {
		t.Fatalf("generated default branch: %v", err)
	}
	if _, err := stor.Reference(plumbing.NewBranchReferenceName("feedback")); err != nil {
		t.Fatalf("feedback branch: %v", err)
	}
	if permission := srv.store.GetRepoCollaboratorPermission(orgLogin, repo.Name, student.Login); permission != "admin" {
		t.Fatalf("student permission = %q, want admin", permission)
	}
	if prs := srv.store.ListPullRequests(repo.ID, "all"); len(prs) != 1 || prs[0].Title != "Feedback" {
		t.Fatalf("feedback pull requests = %+v", prs)
	}

	// Acceptance is idempotent and never creates a second repository.
	again := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/invitations/"+inviteCode+"/accept", studentToken, map[string]interface{}{}), http.StatusOK)
	if again["id"] != accepted["id"] {
		t.Fatalf("repeat acceptance id = %v, want %v", again["id"], accepted["id"])
	}
}

func TestGitHubClassroomGroupAcceptanceLinksRosterIdentifiers(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	org := srv.createTestOrg(t)
	starterFullName := srv.createTestRepo(t)
	starter := srv.store.GetRepo(starterFullName.owner, starterFullName.name)
	if err := srv.initRepoFiles(context.Background(), starter, starter.DefaultBranch, "Group starter", "", "", true); err != nil {
		t.Fatalf("initialize starter: %v", err)
	}
	students := []*User{srv.createTestUser(t, "classroom-group-one"), srv.createTestUser(t, "classroom-group-two"), srv.createTestUser(t, "classroom-group-three")}
	tokens := make([]string, len(students))
	for i, student := range students {
		tokens[i] = srv.store.CreateToken(student.ID, "repo").Value
	}

	classroom := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms", defaultToken, map[string]interface{}{"name": "Group Course", "organization": org}), http.StatusCreated)
	classroomID := strconv.Itoa(int(classroom["id"].(float64)))
	decodeJSONWithStatus(t, srv.put(t, "/classroom-data/classrooms/"+classroomID+"/roster", defaultToken, map[string]interface{}{
		"students": []map[string]interface{}{{"roster_identifier": "student-a"}, {"roster_identifier": "student-b"}, {"roster_identifier": "student-c"}},
	}), http.StatusOK)
	assignment := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms/"+classroomID+"/assignments", defaultToken, map[string]interface{}{
		"title": "Team Project", "type": "group", "starter_code_repository": starterFullName.fullName(), "max_members": 2, "max_teams": 1,
	}), http.StatusCreated)
	assignmentID := int(assignment["id"].(float64))
	invite := assignment["invite_link"].(string)
	code := invite[strings.LastIndex(invite, "/")+1:]
	invitation := decodeJSONWithStatus(t, srv.get(t, "/classroom-data/invitations/"+code, tokens[0]), http.StatusOK)
	if invitation["roster_identifier_required"] != true {
		t.Fatalf("invitation roster link requirement = %v", invitation["roster_identifier_required"])
	}

	first := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/invitations/"+code+"/accept", tokens[0], map[string]interface{}{"group_name": "Compiler Crew", "roster_identifier": "student-a"}), http.StatusCreated)
	second := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/invitations/"+code+"/accept", tokens[1], map[string]interface{}{"group_name": "Compiler Crew", "roster_identifier": "student-b"}), http.StatusOK)
	if first["id"] != second["id"] {
		t.Fatalf("group acceptance ids differ: %v vs %v", first["id"], second["id"])
	}
	resp := srv.post(t, "/classroom-data/invitations/"+code+"/accept", tokens[2], map[string]interface{}{"group_name": "Compiler Crew", "roster_identifier": "student-c"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("full team acceptance = %d, want 422", resp.StatusCode)
	}

	accepted := decodeJSONArray(t, srv.get(t, fmt.Sprintf("/api/v3/assignments/%d/accepted_assignments", assignmentID), defaultToken))
	linked := accepted[0]["students"].([]interface{})
	if len(linked) != 2 {
		t.Fatalf("group students = %v", linked)
	}
	dashboard := decodeJSONWithStatus(t, srv.get(t, "/classroom-data", defaultToken), http.StatusOK)
	for _, rawClassroom := range dashboard["classrooms"].([]interface{}) {
		item := rawClassroom.(map[string]interface{})
		if item["id"] != classroom["id"] {
			continue
		}
		roster := item["roster"].([]interface{})
		third := roster[2].(map[string]interface{})
		if third["login"] != "" {
			t.Fatalf("rejected student claimed roster entry: %v", third)
		}
	}
}

func TestGitHubClassroomTransitionExportImportAndNoOperatorSeeds(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	org := srv.createTestOrg(t)
	classroomName := "Transition Export " + strconv.FormatUint(testutil.NextTestID(), 10)
	decodeJSONWithStatus(t, srv.post(t, "/classroom-data/classrooms", defaultToken, map[string]interface{}{
		"name": classroomName, "organization": org,
	}), http.StatusCreated)

	exported := decodeJSONWithStatus(t, srv.get(t, "/classroom-data/export", defaultToken), http.StatusOK)
	if exported["format"] != "bleephub-classroom-transition-v1" {
		t.Fatalf("transition format = %v", exported["format"])
	}
	classrooms := exported["classrooms"].([]interface{})
	if len(classrooms) == 0 {
		t.Fatal("transition export omitted classrooms")
	}
	found := 0
	for _, raw := range classrooms {
		item, _ := raw.(map[string]interface{})
		if item != nil && item["name"] == classroomName {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("transition export contains classroom %q %d times, want exactly once", classroomName, found)
	}
	imported := decodeJSONWithStatus(t, srv.post(t, "/classroom-data/import", defaultToken, exported), http.StatusCreated)
	if got := len(imported["classrooms"].([]interface{})); got != len(classrooms) {
		t.Fatalf("imported %d classrooms, want %d", got, len(classrooms))
	}

	for _, path := range []string{"/internal/classrooms", "/internal/classrooms/1/assignments", "/internal/assignments/1/accepted_assignments"} {
		resp := srv.post(t, path, defaultToken, map[string]interface{}{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("obsolete operator route %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestGitHubClassroomPersistenceReloadPreservesTransitionState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	p1, err := NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("attach persistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.LookupUserByLogin("admin")
	student := admin
	org := st1.CreateOrg(admin, "persistent-classroom", "Persistent Classroom", "")
	starter := st1.CreateRepo(admin, "persistent-starter", "", false)
	studentRepo := st1.CreateOrgRepo(org, student, "assignment-student", "", true)
	classroom := st1.CreateClassroom("Persistent Course", org.ID, false)
	st1.UpdateClassroom(classroom.ID, func(current *Classroom) {
		current.Roster = []ClassroomStudent{{UserID: student.ID, RosterIdentifier: "student-42"}}
	})
	assignment := st1.CreateClassroomAssignment(&ClassroomAssignment{ClassroomID: classroom.ID, Title: "Persistent Assignment", Type: "individual", Slug: "persistent-assignment", InviteCode: "persistent-code", InvitationsEnabled: true, StarterCodeRepoID: starter.ID, AutogradingTests: []ClassroomAutogradingTest{{Name: "Tests", Command: "go test ./...", Points: 25}}})
	acceptedAt := time.Date(2035, time.June, 15, 12, 0, 0, 0, time.UTC)
	accepted := st1.CreateClassroomAcceptedAssignment(&ClassroomAcceptedAssignment{AssignmentID: assignment.ID, Students: []ClassroomStudent{{UserID: student.ID, RosterIdentifier: "student-42"}}, RepoID: studentRepo.ID, AcceptedAt: acceptedAt, BaselineSHA: strings.Repeat("a", 40)})
	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	p2, err := NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer p2.Close()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}
	gotClassroom := st2.getClassroom(classroom.ID)
	gotAssignment := st2.getClassroomAssignment(assignment.ID)
	gotAccepted := st2.ClassroomAcceptedAssignments[accepted.ID]
	if gotClassroom == nil || len(gotClassroom.Roster) != 1 || gotClassroom.Roster[0].RosterIdentifier != "student-42" {
		t.Fatalf("reloaded classroom = %+v", gotClassroom)
	}
	if gotAssignment == nil || len(gotAssignment.AutogradingTests) != 1 || gotAssignment.AutogradingTests[0].Points != 25 {
		t.Fatalf("reloaded assignment = %+v", gotAssignment)
	}
	if gotAccepted == nil || gotAccepted.BaselineSHA != strings.Repeat("a", 40) || !gotAccepted.AcceptedAt.Equal(acceptedAt) {
		t.Fatalf("reloaded acceptance = %+v", gotAccepted)
	}
}
