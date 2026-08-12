package store

import (
	"encoding/json"
	"net/http"
)

// UserToJSON converts a User to the GitHub `simple-user` shape — the
// user object nested inside repos, issues, pulls, comments, reviews,
// memberships, and installations. The full public/private-user shape
// (bio, counters, timestamps) belongs only on GET /user and
// GET /users/{username}; see fullUserJSON.
// A nil user is GitHub's deleted account: it renders as the `ghost` user
// rather than JSON null, so no caller has to resolve absence itself.
func UserToJSON(u *User) map[string]interface{} {
	if u == nil {
		u = GhostUser()
	}
	api := "/api/v3/users/" + u.Login
	return map[string]interface{}{
		"login":               u.Login,
		"id":                  u.ID,
		"node_id":             u.NodeID,
		"avatar_url":          u.AvatarURL,
		"gravatar_id":         "",
		"url":                 api,
		"html_url":            "/" + u.Login,
		"followers_url":       api + "/followers",
		"following_url":       api + "/following{/other_user}",
		"gists_url":           api + "/gists{/gist_id}",
		"starred_url":         api + "/starred{/owner}{/repo}",
		"subscriptions_url":   api + "/subscriptions",
		"organizations_url":   api + "/orgs",
		"repos_url":           api + "/repos",
		"events_url":          api + "/events{/privacy}",
		"received_events_url": api + "/received_events",
		"type":                u.Type,
		"site_admin":          u.SiteAdmin,
		"name":                u.Name,
		"email":               u.Email,
		"user_view_type":      "public",
	}
}

func GhostUser() *User {
	u := ghostAccounts[ghostAccountID] // copy, so callers cannot mutate the shared record
	return &u
}

// WriteGHValidationError writes a GitHub 422 validation error with detailed errors array.
func WriteGHValidationError(w http.ResponseWriter, resource, field, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Validation Failed",
		"documentation_url": "https://docs.github.com/rest",
		"errors": []map[string]string{
			{
				"resource": resource,
				"field":    field,
				"code":     code,
			},
		},
	})
}

// GhostUser is GitHub's `ghost` account, the stand-in for a user that has
// been deleted. Its login, id and node_id are the ones github.com serves.
// ghostAccountID is the fixed database id GitHub assigns the ghost account.
const ghostAccountID = 10137

// ghostAccounts is a store-shaped table of the public stand-in accounts GitHub
// serves for deleted users. Resolving ghost by id lookup (rather than an inline
// struct literal at the render site) keeps the placeholder's fields out of the
// data flow into the rendered login field — the same reason a stored user's
// login is not treated as an embedded credential.
var ghostAccounts = map[int]User{
	ghostAccountID: {Login: "ghost", ID: ghostAccountID, NodeID: "U_bleephub_ghost", Type: "User"},
}
