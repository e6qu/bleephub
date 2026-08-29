package graphqlapi

// Repository settings, metadata and viewer-standing fields. Every member reads
// the same repository row the REST repository shape serves.

import (
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addRepositorySettingFields installs the merge, branch, description and status
// members of Repository.
func (s *Resolver) addRepositorySettingFields(types *accountSurfaceTypes) {
	repoType := types.repository
	uri := s.graphQLStringScalar("URI")
	html := s.graphQLStringScalar("HTML")

	// merge configuration
	repoType.AddFieldConfig("autoMergeAllowed",
		s.repoBoolField(func(r *store.Repo) bool { return r.AllowAutoMerge }))
	repoType.AddFieldConfig("allowUpdateBranch",
		s.repoBoolField(func(r *store.Repo) bool { return r.AllowUpdateBranch }))
	repoType.AddFieldConfig("squashPrTitleUsedAsDefault",
		s.repoBoolField(func(r *store.Repo) bool { return r.UseSquashPRTitleAsDefault }))
	repoType.AddFieldConfig("mergeCommitTitle", s.repoEnumField(
		s.sharedEnum("MergeCommitTitle", "MERGE_MESSAGE", "PR_TITLE"),
		func(r *store.Repo) string { return repoMergeSetting(r.MergeCommitTitle, "MERGE_MESSAGE") }))
	repoType.AddFieldConfig("mergeCommitMessage", s.repoEnumField(
		s.sharedEnum("MergeCommitMessage", "BLANK", "PR_BODY", "PR_TITLE"),
		func(r *store.Repo) string { return repoMergeSetting(r.MergeCommitMessage, "PR_TITLE") }))
	repoType.AddFieldConfig("squashMergeCommitTitle", s.repoEnumField(
		s.sharedEnum("SquashMergeCommitTitle", "COMMIT_OR_PR_TITLE", "PR_TITLE"),
		func(r *store.Repo) string { return repoMergeSetting(r.SquashMergeCommitTitle, "COMMIT_OR_PR_TITLE") }))
	repoType.AddFieldConfig("squashMergeCommitMessage", s.repoEnumField(
		s.sharedEnum("SquashMergeCommitMessage", "BLANK", "COMMIT_MESSAGES", "PR_BODY"),
		func(r *store.Repo) string { return repoMergeSetting(r.SquashMergeCommitMessage, "COMMIT_MESSAGES") }))

	// feature switches
	repoType.AddFieldConfig("hasPullRequestsEnabled",
		s.repoBoolField(func(r *store.Repo) bool { return r.HasPullRequests }))
	repoType.AddFieldConfig("webCommitSignoffRequired",
		s.repoBoolField(func(r *store.Repo) bool { return r.WebCommitSignoffRequired }))
	repoType.AddFieldConfig("isBlankIssuesEnabled",
		s.repoBoolField(func(r *store.Repo) bool { return s.repositoryAllowsBlankIssues(r) }))
	repoType.AddFieldConfig("isInOrganization",
		s.repoBoolField(func(r *store.Repo) bool { return r.OwnerType == "Organization" }))
	repoType.AddFieldConfig("isUserConfigurationRepository",
		s.repoBoolField(func(r *store.Repo) bool {
			// The profile-config repository has its name equal to its owner's login (the profile README repo).
			owner, name, ok := store.SplitRepoFullName(r.FullName)
			return ok && r.OwnerType != "Organization" && owner == name
		}))
	repoType.AddFieldConfig("forkingAllowed",
		s.repoBoolField(func(r *store.Repo) bool { return s.repositoryForkingAllowed(r) }))
	repoType.AddFieldConfig("hasSponsorshipsEnabled",
		s.repoBoolField(func(r *store.Repo) bool { return s.repositoryHasSponsorships(r) }))

	// lifecycle state: no repository is ever disabled, mirrored or locked here (no
	// suspension, mirror import or read-only migration); REST reports the same
	// (`disabled: false`, `mirror_url: null`).
	repoType.AddFieldConfig("isDisabled",
		s.repoBoolField(func(*store.Repo) bool { return false }))
	repoType.AddFieldConfig("isMirror",
		s.repoBoolField(func(*store.Repo) bool { return false }))
	repoType.AddFieldConfig("isLocked",
		s.repoBoolField(func(*store.Repo) bool { return false }))
	repoType.AddFieldConfig("mirrorUrl", &graphql.Field{
		Type:    uri,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	repoType.AddFieldConfig("lockReason", &graphql.Field{
		Type: s.sharedEnum("RepositoryLockReason",
			"BILLING", "MIGRATING", "MOVING", "RENAME", "TRADE_RESTRICTION", "TRANSFERRING_OWNERSHIP"),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})

	// description rendering
	repoType.AddFieldConfig("descriptionHTML", &graphql.Field{
		Type: graphql.NewNonNull(html),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return renderAccountMarkdown(repo.Description), nil
		},
	})
	repoType.AddFieldConfig("shortDescriptionHTML", &graphql.Field{
		Type: graphql.NewNonNull(html),
		Args: graphql.FieldConfigArgument{
			"limit": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 200},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			limit := 200
			if n, ok := intArg(p.Args, "limit"); ok {
				limit = n
			}
			return renderAccountMarkdown(truncateRunes(repo.Description, limit)), nil
		},
	})

	// size
	repoType.AddFieldConfig("diskUsage", &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// diskUsage is in kilobytes, the unit REST's `size` already carries.
			return int(s.store.RepoSize(repo.FullName)), nil
		},
	})

	// interaction limits
	repoType.AddFieldConfig("interactionAbility", &graphql.Field{
		Type: s.gqlInteractionAbilityType(types),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// A repository restriction wins; otherwise the owning org's
			// restriction applies as an ORGANIZATION-origin ability.
			if ability := s.interactionAbilitySource(repo.InteractionLimit, repo.InteractionLimitExpiry, "REPOSITORY"); ability != nil {
				return ability, nil
			}
			if repo.OwnerType != "Organization" {
				return nil, nil
			}
			owner, _, _ := store.SplitRepoFullName(repo.FullName)
			limit := s.store.GetOrgInteractionLimit(owner)
			if limit == nil {
				return nil, nil
			}
			expiry := limit.ExpiresAt
			return optionalObject(s.interactionAbilitySource(limit.Limit, &expiry, "ORGANIZATION")), nil
		},
	})

	// security policy
	repoType.AddFieldConfig("isSecurityPolicyEnabled", &graphql.Field{
		Type: graphql.Boolean,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			_, _, found := s.repositoryHealthFile(repo, securityPolicyPaths)
			return found, nil
		},
	})
	repoType.AddFieldConfig("securityPolicyUrl", &graphql.Field{
		Type: uri,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			path, _, found := s.repositoryHealthFile(repo, securityPolicyPaths)
			if !found {
				return nil, nil
			}
			return externalURL("/" + repo.FullName + "/blob/" + repo.DefaultBranch + "/" + path), nil
		},
	})

	// social preview: no upload exists, so every repository uses the generated card at its avatar path.
	repoType.AddFieldConfig("usesCustomOpenGraphImage",
		s.repoBoolField(func(*store.Repo) bool { return false }))
	repoType.AddFieldConfig("openGraphImageUrl", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			return externalURL("/" + repo.FullName + ".png"), nil
		},
	})
}

// repoMergeSetting maps a stored merge-commit setting onto GitHub's enum,
// defaulting when the repository never set one.
func repoMergeSetting(stored, dflt string) string {
	if stored == "" {
		return dflt
	}
	return strings.ToUpper(stored)
}

// repositoryForkingAllowed reports whether the repository may be forked. A
// public repo always can; a private one only where the enterprise policy allows
// private forking — the same gate POST /repos/{o}/{r}/forks enforces.
func (s *Resolver) repositoryForkingAllowed(repo *store.Repo) bool {
	if !repo.Private {
		return true
	}
	policy, _ := s.store.EnterprisePolicyForRepo(repo)
	return policy.AllowPrivateRepositoryForking != store.EnterprisePolicyDisabled
}

// repositoryHasSponsorships reports whether the repository presents a sponsor
// button. An explicit updateRepository setting wins; otherwise it is derived (as
// GitHub derives it until touched): the owner has a Sponsors listing, or a
// FUNDING file is present.
func (s *Resolver) repositoryHasSponsorships(repo *store.Repo) bool {
	if repo.HasSponsorships != nil {
		return *repo.HasSponsorships
	}
	owner, _, ok := store.SplitRepoFullName(repo.FullName)
	if ok && s.store.Sponsors.GetSponsorsListingForAccount(owner) != nil {
		return true
	}
	return len(s.repositoryFundingLinks(repo)) > 0
}

// viewer standing

// addRepositoryViewerFields installs the viewer* members of Repository, each
// answering from the same authorization predicates REST enforces rather than a
// rendered source map.
func (s *Resolver) addRepositoryViewerFields(types *accountSurfaceTypes) {
	repoType := types.repository

	viewerBool := func(decide func(p graphql.ResolveParams, repo *store.Repo) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				repo, err := s.repoFromSource(p.Source)
				if err != nil {
					return nil, err
				}
				return decide(p, repo), nil
			},
		}
	}

	repoType.AddFieldConfig("viewerCanAdminister", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			return s.viewerCanAdminRepo(p.Context, repo)
		}))
	repoType.AddFieldConfig("viewerCanCreateIssues", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			// Issue creation needs the tracker on, the repo unarchived, and
			// write access.
			if !repo.HasIssues || repo.Archived {
				return false
			}
			return s.viewerHasRepoPermission(p.Context, repo, store.ScopeIssues, store.PermWrite)
		}))
	repoType.AddFieldConfig("viewerCanUpdateTopics", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			return s.viewerCanAdminRepo(p.Context, repo)
		}))
	repoType.AddFieldConfig("viewerCanSubscribe", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			return s.ghUserFromContext(p.Context) != nil && s.viewerCanReadRepo(p.Context, repo)
		}))
	repoType.AddFieldConfig("viewerCanCreateProjects", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			return repo.HasProjects && !repo.Archived &&
				s.viewerHasRepoPermission(p.Context, repo, store.ScopeAdministration, store.PermWrite)
		}))
	repoType.AddFieldConfig("viewerCanSeeIssueFields", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			// Custom issue fields are an org feature, visible where the repo is
			// org-owned and readable.
			return repo.OwnerType == "Organization" && s.viewerCanReadRepo(p.Context, repo)
		}))
	repoType.AddFieldConfig("viewerHasStarred", viewerBool(
		func(p graphql.ResolveParams, repo *store.Repo) bool {
			viewer := s.ghUserFromContext(p.Context)
			if viewer == nil {
				return false
			}
			owner, name, ok := store.SplitRepoFullName(repo.FullName)
			return ok && s.store.IsRepoStarredBy(viewer.ID, owner, name)
		}))

	repoType.AddFieldConfig("viewerSubscription", &graphql.Field{
		Type: s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			viewer := s.ghUserFromContext(p.Context)
			if viewer == nil {
				return nil, nil
			}
			sub := s.store.GetRepoSubscription(viewer.ID, repo.ID)
			if sub == nil {
				return nil, nil
			}
			switch {
			case sub.Ignored:
				return "IGNORED", nil
			case sub.Subscribed:
				return "SUBSCRIBED", nil
			default:
				return "UNSUBSCRIBED", nil
			}
		},
	})

	repoType.AddFieldConfig("viewerDefaultMergeMethod", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("PullRequestMergeMethod", "MERGE", "REBASE", "SQUASH")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			// The method the merge button offers first, in GitHub's precedence.
			switch {
			case repo.AllowMergeCommit:
				return "MERGE", nil
			case repo.AllowSquashMerge:
				return "SQUASH", nil
			case repo.AllowRebaseMerge:
				return "REBASE", nil
			default:
				return "MERGE", nil
			}
		},
	})

	repoType.AddFieldConfig("viewerDefaultCommitEmail", &graphql.Field{
		Type: graphql.String,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			if viewer == nil {
				return nil, nil
			}
			for _, email := range s.store.ListUserEmails(viewer.ID) {
				if email.Primary {
					return email.Email, nil
				}
			}
			return nilStr(viewer.Email), nil
		},
	})
	repoType.AddFieldConfig("viewerPossibleCommitEmails", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(graphql.String)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			if viewer == nil {
				return nil, nil
			}
			// Every verified address is offered as a commit author identity.
			out := []string{}
			seen := map[string]bool{}
			for _, email := range s.store.ListUserEmails(viewer.ID) {
				if !email.Verified || seen[email.Email] {
					continue
				}
				seen[email.Email] = true
				out = append(out, email.Email)
			}
			if len(out) == 0 && viewer.Email != "" {
				out = append(out, viewer.Email)
			}
			return out, nil
		},
	})

	// No content-warning moderation state exists; the field is served only so
	// selecting it is not an unknown-field validation error.
	repoType.AddFieldConfig("viewerContentWarning", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "ContentWarning",
			Fields: graphql.Fields{
				"category":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"customSubCategory": &graphql.Field{Type: graphql.String},
				"subCategory":       &graphql.Field{Type: graphql.String},
				"subTitle":          &graphql.Field{Type: graphql.String},
				"title":             &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"type":              &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			},
		}),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
}
