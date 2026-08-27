package store

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// UserToJSON renders a User as GitHub's `simple-user` shape (the user nested
// inside repos, issues, pulls, and so on); the fuller shape belongs only on GET
// /user and GET /users/{username}, see fullUserJSON. A nil user renders as the
// `ghost` account rather than JSON null.
//
// Hypermedia members are absolute against baseURL: `simple-user` declares them
// format: uri, and clients build their next request by resolving an object's
// own `url`, so a relative value would double the /api/v3 prefix.
func UserToJSON(u *User, baseURL string) map[string]interface{} {
	if u == nil {
		u = GhostUser()
	}
	api := baseURL + "/api/v3/users/" + u.Login
	return map[string]interface{}{
		"login":               u.Login,
		"id":                  u.ID,
		"node_id":             u.NodeID,
		"avatar_url":          AvatarURLFor(u.AvatarURL, u.ID, baseURL),
		"gravatar_id":         "",
		"url":                 api,
		"html_url":            baseURL + "/" + u.Login,
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

// AvatarURLFor resolves an account's `avatar_url`. An account with no stored
// avatar still must name one (the field is required, format: uri), defaulting
// to the instance-served <base>/avatars/u/{id}?v=4 as GitHub Enterprise does.
// An already-absolute stored value is preserved; a relative one joins the base.
func AvatarURLFor(stored string, id int, baseURL string) string {
	switch {
	case strings.HasPrefix(stored, "http://"), strings.HasPrefix(stored, "https://"):
		return stored
	// Reject a protocol-relative "//host" or "/\host" so a leading-slash path
	// can never point off-origin, then join under the base.
	case strings.HasPrefix(stored, "/") && !strings.HasPrefix(stored, "//") && !strings.HasPrefix(stored, `/\`):
		return baseURL + stored
	case stored != "":
		return baseURL + "/" + strings.TrimLeft(stored, `/\`)
	}
	return baseURL + "/avatars/u/" + strconv.Itoa(id) + "?v=4"
}

func GhostUser() *User {
	u := ghostAccounts[ghostAccountID] // copy: callers must not mutate the shared record
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

// ghostAccountID is the fixed database id GitHub assigns the `ghost` account,
// the stand-in for a deleted user.
const ghostAccountID = 10137

// ghostAccounts tables the deleted-user stand-in accounts. Resolving ghost by
// id lookup keeps the placeholder's fields out of the data flow into the
// rendered login.
var ghostAccounts = map[int]User{
	ghostAccountID: {Login: "ghost", ID: ghostAccountID, NodeID: "U_bleephub_ghost", Type: "User"},
}
