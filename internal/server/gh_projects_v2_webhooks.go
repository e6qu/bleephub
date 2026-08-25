package bleephub

import (
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Projects v2 webhooks. GitHub delivers three event names for this family —
// projects_v2, projects_v2_item and projects_v2_status_update — to the hooks
// of the account that owns the project. None of them carry a repository,
// because a project does not belong to one.

// emitProjectV2Event renders and delivers one projects_v2-family event.
//
// Delivery is to the owning organization's hooks. A user-owned project has no
// hook target on this instance, so the event is rendered and dropped rather
// than misdelivered to an unrelated organization.
func (s *Server) emitProjectV2Event(event store.ProjectV2Event) {
	if event.Project == nil || event.Event == "" {
		return
	}
	if event.Project.OwnerType != "Organization" {
		return
	}
	org := s.store.GetOrgByID(event.Project.OwnerID)
	if org == nil {
		return
	}

	payload := map[string]interface{}{
		"action":       event.Action,
		"organization": orgWebhookPayload(org, s.publicOrigin()),
	}
	if event.Sender != nil {
		payload["sender"] = store.UserToJSON(event.Sender, s.publicOrigin())
	}
	switch event.Event {
	case store.ProjectV2EventProject:
		payload["projects_v2"] = s.projectV2WebhookPayload(event.Project)
	case store.ProjectV2EventItem:
		if event.Item == nil {
			return
		}
		payload["projects_v2_item"] = s.projectV2ItemWebhookPayload(event.Item, event.Project)
	case store.ProjectV2EventStatusUpdate:
		if event.StatusUpdate == nil {
			return
		}
		payload["projects_v2_status_update"] = s.projectV2StatusUpdateWebhookPayload(event.StatusUpdate, event.Project)
	default:
		return
	}
	if changes := projectV2ChangesPayload(event.Changes); changes != nil {
		payload["changes"] = changes
	}
	s.emitOrgWebhookEvent(org.Login, event.Event, event.Action, payload)
}

// projectV2ChangesPayload renders the before/after diff GitHub puts on the
// `edited` actions. Each entry is a {from, to} pair under the changed field's
// name.
func projectV2ChangesPayload(changes map[string]store.ProjectV2Change) map[string]interface{} {
	if len(changes) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(changes))
	for name, change := range changes {
		out[name] = map[string]interface{}{"from": change.From, "to": change.To}
	}
	return out
}

// projectV2WebhookPayload renders the `projects_v2` object.
func (s *Server) projectV2WebhookPayload(p *store.ProjectV2) map[string]interface{} {
	out := map[string]interface{}{
		"id":                p.ID,
		"node_id":           p.NodeID,
		"number":            p.Number,
		"title":             p.Title,
		"public":            p.Public,
		"created_at":        p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":        p.UpdatedAt.UTC().Format(time.RFC3339),
		"short_description": nullableWebhookString(p.ShortDescription),
		"description":       nullableWebhookString(p.Readme),
		"owner":             s.projectV2OwnerWebhookPayload(p),
		"creator":           s.projectV2CreatorJSON(p.CreatorID, s.publicOrigin()),
		// A deleted project is delivered by value on the deleted action; the
		// row is gone by then, so these are always null on the wire.
		"deleted_at": nil,
		"deleted_by": nil,
	}
	if p.ClosedAt != nil {
		out["closed_at"] = p.ClosedAt.UTC().Format(time.RFC3339)
	} else {
		out["closed_at"] = nil
	}
	return out
}

// projectV2OwnerWebhookPayload renders the account the project belongs to, in
// the nested-object form the rest of the webhook surface uses.
func (s *Server) projectV2OwnerWebhookPayload(p *store.ProjectV2) interface{} {
	if p.OwnerType == "Organization" {
		if org := s.store.GetOrgByID(p.OwnerID); org != nil {
			return orgWebhookPayload(org, s.publicOrigin())
		}
		return nil
	}
	if u := s.store.GetUserByID(p.OwnerID); u != nil {
		return store.UserToJSON(u, s.publicOrigin())
	}
	return nil
}

// projectV2ItemWebhookPayload renders the `projects_v2_item` object. The item
// names its project and its content by node id rather than embedding them,
// which is how GitHub keeps this payload small.
func (s *Server) projectV2ItemWebhookPayload(it *store.ProjectV2Item, project *store.ProjectV2) map[string]interface{} {
	out := map[string]interface{}{
		"id":              it.ID,
		"node_id":         it.NodeID,
		"project_node_id": project.NodeID,
		"content_type":    it.ContentType,
		"creator":         s.projectV2CreatorJSON(it.CreatorID, s.publicOrigin()),
		"created_at":      it.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      it.UpdatedAt.UTC().Format(time.RFC3339),
		"content_node_id": s.projectV2ContentNodeID(it),
	}
	if it.ArchivedAt != nil {
		out["archived_at"] = it.ArchivedAt.UTC().Format(time.RFC3339)
	} else {
		out["archived_at"] = nil
	}
	return out
}

// projectV2ContentNodeID is the node id of the issue or pull request an item
// wraps. A draft issue has no separate row, so the item's own node id is its
// content id — which is what GitHub sends.
func (s *Server) projectV2ContentNodeID(it *store.ProjectV2Item) interface{} {
	switch it.ContentType {
	case "Issue":
		if issue := s.store.GetIssue(it.ContentID); issue != nil {
			return issue.NodeID
		}
	case "PullRequest":
		if pr := s.store.GetPullRequest(it.ContentID); pr != nil {
			return pr.NodeID
		}
	default:
		return it.NodeID
	}
	return nil
}

// projectV2StatusUpdateWebhookPayload renders the
// `projects_v2_status_update` object.
func (s *Server) projectV2StatusUpdateWebhookPayload(u *store.ProjectV2StatusUpdate, project *store.ProjectV2) map[string]interface{} {
	return map[string]interface{}{
		"id":              u.ID,
		"node_id":         u.NodeID,
		"project_node_id": project.NodeID,
		"creator":         s.projectV2CreatorJSON(u.CreatorID, s.publicOrigin()),
		"created_at":      u.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      u.UpdatedAt.UTC().Format(time.RFC3339),
		"status":          nullableWebhookString(u.Status),
		"start_date":      nullableWebhookString(u.StartDate),
		"target_date":     nullableWebhookString(u.TargetDate),
		"body":            nullableWebhookString(u.Body),
	}
}

// nullableWebhookString sends an unset optional string as JSON null, which is
// what GitHub sends for a field the project genuinely has no value for.
func nullableWebhookString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}
