package bleephub

// Where Actions policies take effect. A policy that was stored and never
// consulted would be worse than none: an owner would believe workflows were
// restricted while anyone could still start them. So every path that creates a
// run asks here first — an event reaching a workflow, a workflow_dispatch, a
// re-run — and the scheduler asks the store directly, having no actor to name.

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// actionsPolicyVerdict decides whether actor may start the workflow at
// workflowPath for event. A nil actor is a trigger nobody is behind.
func (s *Server) actionsPolicyVerdict(repo *store.Repo, workflowPath, event string, actor *store.User) store.ActionsPolicyVerdict {
	policies := s.store.ApplicableActionsPolicies(repo, workflowPath)
	if len(policies) == 0 {
		return store.ActionsPolicyVerdict{}
	}
	return store.EvaluateActionsPolicies(policies, event, func(allowed store.ActionsPolicyActor) bool {
		return s.actionsPolicyActorAdmits(allowed, repo, actor)
	})
}

// actionsPolicyActorAdmits reports whether actor is the listed actor, or is
// covered by it: a member of the listed team, a holder of the listed role, the
// bot of the listed app or installation.
func (s *Server) actionsPolicyActorAdmits(allowed store.ActionsPolicyActor, repo *store.Repo, actor *store.User) bool {
	if actor == nil {
		return false
	}
	switch allowed.Type {
	case "User", "Bot":
		return actor.ID == allowed.ID
	case "Team":
		team := s.store.GetTeamByID(allowed.ID)
		if team == nil {
			return false
		}
		org := s.store.GetOrgByID(team.OrgID)
		if org == nil {
			return false
		}
		_, member := s.store.GetTeamMembership(org.Login, team.Slug, actor.ID)
		return member
	case "EnterpriseTeam", "BusinessTeam":
		for _, team := range s.store.ListEnterpriseTeams() {
			if team.ID == allowed.ID {
				return s.store.IsEnterpriseTeamMember(team, actor.ID)
			}
		}
		return false
	case "RepositoryRole":
		return userHoldsRepositoryRole(s.store, actor, repo, allowed.ID)
	case "App":
		// An app acts as its bot, whose id is the app's, negated.
		return actor.Type == "Bot" && actor.ID == -allowed.ID
	case "IntegrationInstallation":
		installation := s.store.GetInstallation(allowed.ID)
		return installation != nil && actor.Type == "Bot" && actor.ID == -installation.AppID
	}
	return false
}

// eventSender recovers the actor an event payload names, or nil.
func (s *Server) eventSender(payload map[string]interface{}) *store.User {
	sender, _ := payload["sender"].(map[string]interface{})
	if sender == nil {
		return nil
	}
	login, _ := sender["login"].(string)
	if login == "" {
		return nil
	}
	if user := s.store.LookupUserByLogin(login); user != nil {
		return user
	}
	// An app's bot is derived from the app rather than stored as a user.
	if id, ok := sender["id"].(int); ok && id < 0 {
		return &store.User{ID: id, Login: login, Type: "Bot"}
	}
	if id, ok := sender["id"].(float64); ok && id < 0 {
		return &store.User{ID: int(id), Login: login, Type: "Bot"}
	}
	return nil
}

// workflowFilePath is the repository path of a workflow file, which is what a
// policy's workflow_path patterns are written against. The trigger path knows
// workflow files by their bare name.
func workflowFilePath(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	return ".github/workflows/" + name
}

// refuseByActionsPolicy answers a request to start a run that the policies do
// not admit the caller to start, reporting whether it did. The policy is named
// so that the caller learns what to ask an administrator about.
func (s *Server) refuseByActionsPolicy(w http.ResponseWriter, r *http.Request, repoKey, workflowPath, event string) bool {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return false
	}
	verdict := s.actionsPolicyVerdict(repo, workflowFilePath(workflowPath), event, ghUserFromContext(r.Context()))
	if !verdict.Refused {
		return false
	}
	writeGHError(w, http.StatusForbidden, "The Actions policy \""+verdict.PolicyName+"\" does not allow you to trigger this workflow.")
	return true
}
