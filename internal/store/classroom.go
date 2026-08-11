package store

import (
	"sort"
	"strconv"
	"time"
)

// Classroom is a GitHub Classroom classroom owned by an organization.
type Classroom struct {
	ID       int                `json:"id"`
	Name     string             `json:"name"`
	Archived bool               `json:"archived"`
	OrgID    int                `json:"org_id"`
	Roster   []ClassroomStudent `json:"roster,omitempty"`
}

// ClassroomAssignment is an assignment within a classroom.
type ClassroomAssignment struct {
	ID                          int                        `json:"id"`
	ClassroomID                 int                        `json:"classroom_id"`
	Title                       string                     `json:"title"`
	Type                        string                     `json:"type"` // "individual" or "group"
	Slug                        string                     `json:"slug"`
	InviteCode                  string                     `json:"invite_code"`
	InvitationsEnabled          bool                       `json:"invitations_enabled"`
	PublicRepo                  bool                       `json:"public_repo"`
	StudentsAreRepoAdmins       bool                       `json:"students_are_repo_admins"`
	FeedbackPullRequestsEnabled bool                       `json:"feedback_pull_requests_enabled"`
	MaxTeams                    *int                       `json:"max_teams"`
	MaxMembers                  *int                       `json:"max_members"`
	Editor                      string                     `json:"editor"`
	Language                    string                     `json:"language"`
	Deadline                    *time.Time                 `json:"deadline"`
	StarterCodeRepoID           int                        `json:"starter_code_repo_id"`
	AutogradingTests            []ClassroomAutogradingTest `json:"autograding_tests,omitempty"`
}

// ClassroomAcceptedAssignment records a student's (or team's) acceptance of
// an assignment, backed by the real repository the acceptance created.
type ClassroomAcceptedAssignment struct {
	ID           int                `json:"id"`
	AssignmentID int                `json:"assignment_id"`
	Students     []ClassroomStudent `json:"students"`
	RepoID       int                `json:"repo_id"`
	GroupName    string             `json:"group_name"`
	AcceptedAt   time.Time          `json:"accepted_at"`
	BaselineSHA  string             `json:"baseline_sha"`
	SubmittedAt  time.Time          `json:"submitted_at,omitempty"` // reloads pre-transition Classroom records
}

func (st *Store) CreateClassroom(name string, orgID int, archived bool) *Classroom {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := &Classroom{ID: st.NextClassroomID, Name: name, Archived: archived, OrgID: orgID}
	st.NextClassroomID++
	st.Classrooms[c.ID] = c
	if st.Persist != nil {
		st.Persist.MustPut("classrooms", strconv.Itoa(c.ID), c)
	}
	return c
}

func (st *Store) UpdateClassroom(id int, update func(*Classroom)) *Classroom {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	c := st.Classrooms[id]
	if c == nil {
		return nil
	}
	update(c)
	if st.Persist != nil {
		st.Persist.MustPut("classrooms", strconv.Itoa(c.ID), c)
	}
	return c
}

// DeleteClassroom removes the Classroom product metadata and its assignments.
// Assignment repositories remain ordinary organization repositories, matching
// GitHub Classroom's promise that deleting Classroom data does not delete the
// repositories students worked in.
func (st *Store) DeleteClassroom(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Classrooms[id] == nil {
		return false
	}
	// One transaction: the classroom and every assignment and accepted-assignment
	// beneath it are deleted together, so a crash cannot orphan an assignment or
	// acceptance under a deleted classroom (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	delete(st.Classrooms, id)
	batch.Delete("classrooms", strconv.Itoa(id))
	for assignmentID, assignment := range st.ClassroomAssignments {
		if assignment.ClassroomID != id {
			continue
		}
		delete(st.ClassroomAssignments, assignmentID)
		batch.Delete("classroom_assignments", strconv.Itoa(assignmentID))
		for acceptedID, accepted := range st.ClassroomAcceptedAssignments {
			if accepted.AssignmentID == assignmentID {
				delete(st.ClassroomAcceptedAssignments, acceptedID)
				batch.Delete("classroom_accepted_assignments", strconv.Itoa(acceptedID))
			}
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "classrooms", Err: err})
	}
	return true
}

func (st *Store) CreateClassroomAssignment(a *ClassroomAssignment) *ClassroomAssignment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	a.ID = st.NextClassroomAssignmentID
	st.NextClassroomAssignmentID++
	st.ClassroomAssignments[a.ID] = a
	if st.Persist != nil {
		st.Persist.MustPut("classroom_assignments", strconv.Itoa(a.ID), a)
	}
	return a
}

func (st *Store) UpdateClassroomAssignment(id int, update func(*ClassroomAssignment)) *ClassroomAssignment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	a := st.ClassroomAssignments[id]
	if a == nil {
		return nil
	}
	update(a)
	if st.Persist != nil {
		st.Persist.MustPut("classroom_assignments", strconv.Itoa(a.ID), a)
	}
	return a
}

func (st *Store) DeleteClassroomAssignment(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.ClassroomAssignments[id] == nil {
		return false
	}
	// One transaction: the assignment and every acceptance of it are deleted
	// together, so a crash cannot orphan an acceptance (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	delete(st.ClassroomAssignments, id)
	batch.Delete("classroom_assignments", strconv.Itoa(id))
	for acceptedID, accepted := range st.ClassroomAcceptedAssignments {
		if accepted.AssignmentID == id {
			delete(st.ClassroomAcceptedAssignments, acceptedID)
			batch.Delete("classroom_accepted_assignments", strconv.Itoa(acceptedID))
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "classroom_assignments", Err: err})
	}
	return true
}

func (st *Store) CreateClassroomAcceptedAssignment(a *ClassroomAcceptedAssignment) *ClassroomAcceptedAssignment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	a.ID = st.NextClassroomAcceptedID
	st.NextClassroomAcceptedID++
	st.ClassroomAcceptedAssignments[a.ID] = a
	if st.Persist != nil {
		st.Persist.MustPut("classroom_accepted_assignments", strconv.Itoa(a.ID), a)
	}
	return a
}

func (st *Store) UpdateClassroomAcceptedAssignment(id int, update func(*ClassroomAcceptedAssignment)) *ClassroomAcceptedAssignment {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	a := st.ClassroomAcceptedAssignments[id]
	if a == nil {
		return nil
	}
	update(a)
	if st.Persist != nil {
		st.Persist.MustPut("classroom_accepted_assignments", strconv.Itoa(a.ID), a)
	}
	return a
}

// classroomAcceptedFor returns the accepted assignments for an assignment,
// oldest first.
func (st *Store) ClassroomAcceptedFor(assignmentID int) []*ClassroomAcceptedAssignment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ClassroomAcceptedAssignment
	for _, a := range st.ClassroomAcceptedAssignments {
		if a.AssignmentID == assignmentID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (st *Store) GetClassroom(id int) *Classroom {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.Classrooms[id]
}

func (st *Store) GetClassroomAssignment(id int) *ClassroomAssignment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.ClassroomAssignments[id]
}

type ClassroomAutogradingTest struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Points  int    `json:"points"`
}

// ClassroomStudent links an accepted assignment to a student user with the
// classroom roster identifier.
type ClassroomStudent struct {
	UserID           int    `json:"user_id"`
	RosterIdentifier string `json:"roster_identifier"`
}
