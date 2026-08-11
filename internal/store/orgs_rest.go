package store

// OrgAsSimpleUserJSON converts an Org to the simple-user shape GitHub uses
// as the owner field of repositories owned by an organization. The fields
// are identical to UserToJSON; only the type differs.
func OrgAsSimpleUserJSON(org *Org) map[string]interface{} {
	api := "/api/v3/users/" + org.Login
	return map[string]interface{}{
		"login":               org.Login,
		"id":                  org.ID,
		"node_id":             org.NodeID,
		"avatar_url":          org.AvatarURL,
		"gravatar_id":         "",
		"url":                 api,
		"html_url":            "/" + org.Login,
		"followers_url":       api + "/followers",
		"following_url":       api + "/following{/other_user}",
		"gists_url":           api + "/gists{/gist_id}",
		"starred_url":         api + "/starred{/owner}{/repo}",
		"subscriptions_url":   api + "/subscriptions",
		"organizations_url":   api + "/orgs",
		"repos_url":           api + "/repos",
		"events_url":          api + "/events{/privacy}",
		"received_events_url": api + "/received_events",
		"type":                org.Type,
		"site_admin":          false,
		"name":                org.Name,
		"email":               org.Email,
		"user_view_type":      "public",
	}
}
