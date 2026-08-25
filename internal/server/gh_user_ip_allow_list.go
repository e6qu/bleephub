package bleephub

// The account's own IP allow list.
//
// An enterprise and an organization each keep an allow list, and GitHub models
// both as IpAllowListOwner (Enterprise | Organization | App) so they are
// writable over GraphQL. A *user's* list has no owner in that union: it is
// reachable only from the account's own settings, which is why it lives under
// /ui-data rather than being invented into /api/v3 or into the GraphQL schema.
//
// The list is enforced only when the enterprise turns user-level enforcement
// on (ipAllowListUserLevelEnforcementEnabled). Until it does, an account may
// keep a list — the enterprise decides whether it binds, the account decides
// what is on it — and enforcement begins the moment the enterprise says so,
// with no second write to activate it.
//
// The management endpoints themselves are deliberately outside the enforced
// surface (the gate wraps /api/ routes only): an account that adds a range it
// is not currently inside must still be able to correct the mistake.

import (
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHUserIPAllowListRoutes() {
	s.route("GET /ui-data/user/ip-allow-list", s.handleListUserIPAllowList)
	s.route("POST /ui-data/user/ip-allow-list", s.handleCreateUserIPAllowListEntry)
	s.route("PATCH /ui-data/user/ip-allow-list/{entry_id}", s.handleUpdateUserIPAllowListEntry)
	s.route("DELETE /ui-data/user/ip-allow-list/{entry_id}", s.handleDeleteUserIPAllowListEntry)
}

// userIPAllowListEntryJSON is one row, shaped like the GraphQL
// IpAllowListEntry the enterprise and organization lists serve, so the two
// surfaces describe an entry the same way.
func userIPAllowListEntryJSON(entry *store.IPAllowListEntry) map[string]interface{} {
	return map[string]interface{}{
		"id":               entry.ID,
		"node_id":          entry.NodeID,
		"allow_list_value": entry.AllowListValue,
		"name":             entry.Name,
		"is_active":        entry.IsActive,
		"created_at":       entry.CreatedAt,
		"updated_at":       entry.UpdatedAt,
	}
}

// userIPAllowListJSON is the whole page: the entries, and whether the
// enterprise currently enforces them. Reporting the enforcement state is what
// keeps the page honest — a list nothing enforces must not look like one that
// is protecting the account.
func (s *Server) userIPAllowListJSON(userID int) map[string]interface{} {
	entries := s.store.ListIPAllowListEntries(store.IPAllowListOwnerUser, userID)
	rows := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, userIPAllowListEntryJSON(entry))
	}
	enforced := false
	if e := s.primaryEnterprise(); e != nil {
		enforced = e.Policy.IPAllowListUserLevelEnforcementEnabled == store.EnterprisePolicyEnabled
	}
	return map[string]interface{}{"entries": rows, "enforced": enforced}
}

func (s *Server) handleListUserIPAllowList(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	writeJSON(w, http.StatusOK, s.userIPAllowListJSON(viewer.ID))
}

// userIPAllowListEntryRequest is the body both writes take. IsActive is a
// pointer so a PATCH that omits it leaves the entry's state alone.
type userIPAllowListEntryRequest struct {
	AllowListValue string `json:"allow_list_value"`
	Name           string `json:"name"`
	IsActive       *bool  `json:"is_active"`
}

func (s *Server) handleCreateUserIPAllowListEntry(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	var req userIPAllowListEntryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !ValidIPAllowListValue(req.AllowListValue) {
		store.WriteGHValidationError(w, "IpAllowListEntry", "allow_list_value", "invalid")
		return
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	entry := s.store.CreateIPAllowListEntry(store.IPAllowListOwnerUser, viewer.ID, req.AllowListValue, req.Name, active)
	s.recordAuditEvent("user.ip_allow_list_entry.create", viewer.Login, "",
		map[string]interface{}{"entry_id": entry.ID})
	writeJSON(w, http.StatusCreated, userIPAllowListEntryJSON(entry))
}

// userIPAllowListEntryFromPath resolves the entry named in the path, refusing
// anything that is not this account's own. An entry belonging to an enterprise
// or an organization answers 404 here, the same as one that does not exist, so
// the account settings page cannot be used to probe another owner's list.
func (s *Server) userIPAllowListEntryFromPath(w http.ResponseWriter, r *http.Request, viewer *store.User) *store.IPAllowListEntry {
	id, err := strconv.Atoi(r.PathValue("entry_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	entry := s.store.ListIPAllowListEntryByID(id)
	if entry == nil || entry.OwnerType != store.IPAllowListOwnerUser || entry.OwnerID != viewer.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return entry
}

func (s *Server) handleUpdateUserIPAllowListEntry(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	entry := s.userIPAllowListEntryFromPath(w, r, viewer)
	if entry == nil {
		return
	}
	var req userIPAllowListEntryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	value, name, active := entry.AllowListValue, entry.Name, entry.IsActive
	if req.AllowListValue != "" {
		if !ValidIPAllowListValue(req.AllowListValue) {
			store.WriteGHValidationError(w, "IpAllowListEntry", "allow_list_value", "invalid")
			return
		}
		value = req.AllowListValue
	}
	if req.Name != "" {
		name = req.Name
	}
	if req.IsActive != nil {
		active = *req.IsActive
	}
	updated := s.store.UpdateIPAllowListEntry(entry.ID, value, name, active)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, userIPAllowListEntryJSON(updated))
}

func (s *Server) handleDeleteUserIPAllowListEntry(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	entry := s.userIPAllowListEntryFromPath(w, r, viewer)
	if entry == nil {
		return
	}
	if s.store.DeleteIPAllowListEntry(entry.ID) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("user.ip_allow_list_entry.destroy", viewer.Login, "",
		map[string]interface{}{"entry_id": entry.ID})
	w.WriteHeader(http.StatusNoContent)
}
