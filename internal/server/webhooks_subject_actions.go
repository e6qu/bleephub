package bleephub

import "github.com/e6qu/bleephub/internal/store"

// Per-action webhook fan-out for the two subjects that share it.
//
// GitHub's `issues` and `pull_request` events are not one-event-per-API-call:
// each state transition is its own delivery, with the members that transition
// carries (`changes` on edited, `label` on labeled/unlabeled, `assignee` on
// assigned/unassigned, `milestone` on milestoned/demilestoned). Issues and pull
// requests differ only in event name and payload builder, so both drive the
// same emitter and no action can be implemented for one and forgotten for the
// other.
type subjectEmitter struct {
	s     *Server
	repo  *store.Repo
	event string
	// build renders the shared payload for one action; it is called per
	// delivery so every payload carries its own `action` value.
	build func(action string) map[string]interface{}
}

// issueEmitter fans out `issues` actions for one issue.
func (s *Server) issueEmitter(repo *store.Repo, issue *store.Issue, sender *store.User) subjectEmitter {
	return subjectEmitter{s: s, repo: repo, event: "issues", build: func(action string) map[string]interface{} {
		return buildIssuesPayload(s.store, repo, issue, sender, action, s.publicOrigin())
	}}
}

// pullRequestEmitter fans out `pull_request` actions for one pull request.
func (s *Server) pullRequestEmitter(repo *store.Repo, pr *store.PullRequest, sender *store.User) subjectEmitter {
	return subjectEmitter{s: s, repo: repo, event: "pull_request", build: func(action string) map[string]interface{} {
		return buildPullRequestPayload(s.store, repo, pr, sender, action, s.publicOrigin())
	}}
}

// emit delivers one action, merging that action's extra members into the
// payload the shared builder produced.
func (e subjectEmitter) emit(action string, extra map[string]interface{}) {
	if e.repo == nil {
		return
	}
	payload := e.build(action)
	if payload == nil {
		return
	}
	for key, value := range extra {
		payload[key] = value
	}
	e.s.emitWebhookEvent(e.repo.FullName, e.event, action, payload)
}

// emitChanges turns one mutation into GitHub's per-change action sequence.
// The order mirrors the order the handlers record their timeline events in:
// the edit itself, then triage (labels, assignees, milestone), then the state
// transition.
func (e subjectEmitter) emitChanges(change store.SubjectChange) {
	if changes := editedChangesPayload(change); len(changes) > 0 {
		e.emit("edited", map[string]interface{}{"changes": changes})
	}
	if change.LabelsTo != nil {
		e.emitLabelDelta(change.LabelsFrom, *change.LabelsTo)
	}
	if change.AssigneesTo != nil {
		e.emitAssigneeDelta(change.AssigneesFrom, *change.AssigneesTo)
	}
	if change.MilestoneTo != nil {
		e.emitMilestoneChange(change.MilestoneFrom, *change.MilestoneTo)
	}
	switch {
	case change.StateFrom == "OPEN" && change.StateTo == "CLOSED":
		e.emit("closed", nil)
	case change.StateFrom == "CLOSED" && change.StateTo == "OPEN":
		e.emit("reopened", nil)
	}
}

// editedChangesPayload renders the `changes` member of an `edited` payload:
// only the fields the mutation actually changed appear, each as {from: old}.
// A base-branch change also reports the old ref under `base.ref.from`, which
// is where GitHub puts it for a pull request.
func editedChangesPayload(change store.SubjectChange) map[string]interface{} {
	changes := map[string]interface{}{}
	if change.TitleFrom != nil {
		changes["title"] = map[string]interface{}{"from": *change.TitleFrom}
	}
	if change.BodyFrom != nil {
		changes["body"] = map[string]interface{}{"from": *change.BodyFrom}
	}
	if change.BaseRefFrom != nil {
		changes["base"] = map[string]interface{}{
			"ref": map[string]interface{}{"from": *change.BaseRefFrom},
		}
	}
	return changes
}

// emitLabelDelta emits one labeled action per label that entered the set and
// one unlabeled per label that left it, each carrying the `label` member.
func (e subjectEmitter) emitLabelDelta(before, after []int) {
	added, removed := intSetDelta(before, after)
	for _, id := range added {
		e.emitLabelAction("labeled", id)
	}
	for _, id := range removed {
		e.emitLabelAction("unlabeled", id)
	}
}

func (e subjectEmitter) emitLabelAction(action string, labelID int) {
	label := e.s.store.GetLabel(labelID)
	if label == nil {
		return
	}
	// Hypermedia on a nested webhook object is relative, matching the
	// repository and sender objects the shared builder already emitted.
	e.emit(action, map[string]interface{}{"label": issueLabelToJSON(label, "", e.repo.FullName)})
}

// emitAssigneeDelta emits one assigned action per user added and one
// unassigned per user removed, each carrying the `assignee` member.
func (e subjectEmitter) emitAssigneeDelta(before, after []int) {
	added, removed := intSetDelta(before, after)
	for _, id := range added {
		e.emitAssigneeAction("assigned", id)
	}
	for _, id := range removed {
		e.emitAssigneeAction("unassigned", id)
	}
}

func (e subjectEmitter) emitAssigneeAction(action string, userID int) {
	assignee := e.s.store.GetUserByID(userID)
	if assignee == nil {
		return
	}
	e.emit(action, map[string]interface{}{"assignee": senderPayload(assignee, e.s.publicOrigin())})
}

// emitMilestoneChange emits milestoned when a milestone is attached and
// demilestoned when one is detached; the `milestone` member is the milestone
// that was attached, or on a detach the one that was removed. Replacing one
// milestone with another reports both, as GitHub does.
func (e subjectEmitter) emitMilestoneChange(before, after int) {
	if before == after {
		return
	}
	if before != 0 {
		e.emitMilestoneAction("demilestoned", before)
	}
	if after != 0 {
		e.emitMilestoneAction("milestoned", after)
	}
}

func (e subjectEmitter) emitMilestoneAction(action string, milestoneID int) {
	milestone := e.s.store.GetMilestone(milestoneID)
	if milestone == nil {
		return
	}
	e.emit(action, map[string]interface{}{
		"milestone": milestoneToJSON(milestone, e.s.store, "", e.repo.FullName),
	})
}

// emitReviewRequestDelta emits one review_requested per reviewer added and one
// review_request_removed per reviewer dropped. GitHub delivers a separate
// event per reviewer, carrying `requested_reviewer` for a user and
// `requested_team` for a team.
func (e subjectEmitter) emitReviewRequestDelta(beforeUsers, afterUsers, beforeTeams, afterTeams []int) {
	addedUsers, removedUsers := intSetDelta(beforeUsers, afterUsers)
	addedTeams, removedTeams := intSetDelta(beforeTeams, afterTeams)
	e.emitReviewerActions("review_requested", addedUsers, addedTeams)
	e.emitReviewerActions("review_request_removed", removedUsers, removedTeams)
}

func (e subjectEmitter) emitReviewerActions(action string, userIDs, teamIDs []int) {
	for _, id := range userIDs {
		if reviewer := e.s.store.GetUserByID(id); reviewer != nil {
			e.emit(action, map[string]interface{}{"requested_reviewer": senderPayload(reviewer, e.s.publicOrigin())})
		}
	}
	if len(teamIDs) == 0 {
		return
	}
	org := e.s.store.GetOrg(ownerFromRepoFullName(e.repo.FullName))
	if org == nil {
		// A team reviewer only exists on an org repo; without the org there is
		// no team object to render, so the user-side events still stand alone.
		return
	}
	for _, id := range teamIDs {
		if team := e.s.store.GetTeamByID(id); team != nil {
			e.emit(action, map[string]interface{}{"requested_team": teamSimpleJSON(team, org, e.s.store, "")})
		}
	}
}

// emitLockAction emits the locked/unlocked action for whichever of the issue or
// pull request the number resolves to. The REST lock endpoint locks both
// through one `/issues/{n}/lock` surface (pull requests are issues internally),
// so the event name is only known after resolving the number.
func (s *Server) emitLockAction(repo *store.Repo, number int, sender *store.User, locked bool) {
	action := "unlocked"
	if locked {
		action = "locked"
	}
	if issue := s.store.GetIssueByNumber(repo.ID, number); issue != nil {
		s.issueEmitter(repo, issue, sender).emit(action, nil)
		return
	}
	if pr := s.store.GetPullRequestByNumber(repo.ID, number); pr != nil {
		s.pullRequestEmitter(repo, pr, sender).emit(action, nil)
	}
}

// intSetDelta reports which ids are in after but not before (added) and which
// are in before but not after (removed), preserving each slice's order so the
// resulting action sequence is deterministic.
func intSetDelta(before, after []int) (added, removed []int) {
	old, next := intSet(before), intSet(after)
	for _, id := range after {
		if !old[id] {
			added = append(added, id)
		}
	}
	for _, id := range before {
		if !next[id] {
			removed = append(removed, id)
		}
	}
	return added, removed
}

// intSet builds a membership set from an int slice.
func intSet(ids []int) map[int]bool {
	m := make(map[int]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}
