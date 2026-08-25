package main

import (
	"errors"
	"fmt"
	"net/http"

	github "github.com/google/go-github/v88/github"
)

// runBranches covers branch protection in the depth a merge gate actually
// configures it, plus rulesets — the newer surface that carries the merge
// queue — and the branch rename redirect.
func runBranches(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "branches"
	sc := newScratch(client, set.owner, "conformance-branches")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the branch repository fixture could not be provisioned",
			"repos.updateBranchProtection (full)", "repos.getBranchProtection",
			"repos.getRequiredStatusChecks", "repos.updateRequiredStatusChecks",
			"repos.listRequiredStatusChecksContexts", "repos.addRequiredStatusChecksContexts",
			"repos.getPullRequestReviewEnforcement", "repos.updatePullRequestReviewEnforcement",
			"repos.getAdminEnforcement", "repos.removeAdminEnforcement",
			"repos.requireSignaturesOnProtectedBranch", "repos.removeBranchProtection",
			"repos.createRuleset", "repos.getRuleset", "repos.updateRuleset", "repos.getAllRulesets",
			"repos.deleteRuleset", "repos.createRuleset (merge queue)", "repos.renameBranch")
		return
	}

	rec.check(domain, "repos.updateBranchProtection (full)",
		"PUT /repos/{owner}/{repo}/branches/{branch}/protection", func() error {
			protection, _, err := client.Repositories.UpdateBranchProtection(ctx, sc.owner, sc.repo, sc.branch,
				&github.ProtectionRequest{
					RequiredStatusChecks: &github.RequiredStatusChecks{
						Strict: true,
						Checks: &[]*github.RequiredStatusCheck{{Context: "ci/conformance"}},
					},
					RequiredPullRequestReviews: &github.PullRequestReviewsEnforcementRequest{
						DismissStaleReviews:          true,
						RequireCodeOwnerReviews:      true,
						RequiredApprovingReviewCount: 2,
					},
					EnforceAdmins:                  true,
					Restrictions:                   nil,
					RequireLinearHistory:           github.Ptr(true),
					AllowForcePushes:               github.Ptr(false),
					AllowDeletions:                 github.Ptr(false),
					RequiredConversationResolution: github.Ptr(true),
					LockBranch:                     github.Ptr(false),
				})
			if err != nil {
				return err
			}
			if !protection.GetRequiredStatusChecks().Strict {
				return deviate("strict true", "false", "required_status_checks.strict does not round trip")
			}
			reviews := protection.GetRequiredPullRequestReviews()
			if reviews == nil {
				return deviate("required_pull_request_reviews present", "absent",
					"the review requirement was dropped from the response")
			}
			if reviews.RequiredApprovingReviewCount != 2 {
				return deviate("required_approving_review_count 2",
					fmt.Sprintf("%d", reviews.RequiredApprovingReviewCount),
					"the approval count does not round trip, so a merge gate would use the wrong threshold")
			}
			if !reviews.DismissStaleReviews {
				return deviate("dismiss_stale_reviews true", "false", "dismiss_stale_reviews does not round trip")
			}
			if !reviews.RequireCodeOwnerReviews {
				return deviate("require_code_owner_reviews true", "false", "require_code_owner_reviews does not round trip")
			}
			if !protection.GetEnforceAdmins().Enabled {
				return deviate("enforce_admins.enabled true", "false", "enforce_admins does not round trip")
			}
			if !protection.GetRequireLinearHistory().Enabled {
				return deviate("required_linear_history.enabled true", "false",
					"required_linear_history does not round trip")
			}
			if !protection.GetRequiredConversationResolution().Enabled {
				return deviate("required_conversation_resolution.enabled true", "false",
					"required_conversation_resolution does not round trip")
			}
			if protection.GetAllowForcePushes().Enabled {
				return deviate("allow_force_pushes.enabled false", "true",
					"allow_force_pushes does not round trip, so a client cannot tell force pushes are blocked")
			}
			return nil
		})

	rec.check(domain, "repos.getBranchProtection", "GET /repos/{owner}/{repo}/branches/{branch}/protection", func() error {
		protection, _, err := client.Repositories.GetBranchProtection(ctx, sc.owner, sc.repo, sc.branch)
		if err != nil {
			return err
		}
		if protection.GetURL() == "" {
			return deviate("url populated", "empty", "the protection object carries no self link")
		}
		checks := protection.GetRequiredStatusChecks()
		if checks == nil || checks.Checks == nil || len(*checks.Checks) != 1 {
			return deviate("the one required check", "absent",
				"the required status check written above is not read back through checks[]")
		}
		if (*checks.Checks)[0].Context != "ci/conformance" {
			return deviate("ci/conformance", (*checks.Checks)[0].Context, "the required check context is wrong")
		}
		return nil
	})

	rec.check(domain, "repos.getBranch (protected flag)", "GET /repos/{owner}/{repo}/branches/{branch}", func() error {
		branch, _, err := client.Repositories.GetBranch(ctx, sc.owner, sc.repo, sc.branch, 0)
		if err != nil {
			return err
		}
		if !branch.GetProtected() {
			return deviate("protected true", "false",
				"a branch with protection does not report protected, so a client cannot warn before pushing")
		}
		if branch.GetProtection() == nil {
			return deviate("protection object present", "absent",
				"a protected branch omits the inline protection object clients read")
		}
		return nil
	})

	rec.check(domain, "repos.listBranches (protected filter)", "GET /repos/{owner}/{repo}/branches?protected=true", func() error {
		branches, _, err := client.Repositories.ListBranches(ctx, sc.owner, sc.repo,
			&github.BranchListOptions{Protected: github.Ptr(true)})
		if err != nil {
			return err
		}
		if len(branches) != 1 {
			return deviate("exactly the one protected branch", fmt.Sprintf("%d", len(branches)),
				"the protected filter is ignored")
		}
		return nil
	})

	rec.check(domain, "repos.getRequiredStatusChecks",
		"GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks", func() error {
			checks, _, err := client.Repositories.GetRequiredStatusChecks(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if !checks.Strict {
				return deviate("strict true", "false", "the standalone required-status-checks resource lost strict")
			}
			if checks.URL == nil || *checks.URL == "" {
				return deviate("url populated", "empty", "the required-status-checks resource carries no self link")
			}
			return nil
		})

	rec.check(domain, "repos.listRequiredStatusChecksContexts",
		"GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts", func() error {
			contexts, _, err := client.Repositories.ListRequiredStatusChecksContexts(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if len(contexts) != 1 || contexts[0] != "ci/conformance" {
				return deviate("[ci/conformance]", fmt.Sprintf("%v", contexts),
					"the contexts resource does not reflect the configured checks")
			}
			return nil
		})

	rec.check(domain, "repos.addRequiredStatusChecksContexts",
		"POST /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts", func() error {
			var contexts []string
			if _, err := decodeInto(client, http.MethodPost,
				fmt.Sprintf("repos/%s/%s/branches/%s/protection/required_status_checks/contexts", sc.owner, sc.repo, sc.branch),
				map[string]any{"contexts": []string{"ci/second"}}, &contexts); err != nil {
				return err
			}
			if len(contexts) != 2 {
				return deviate("two contexts", fmt.Sprintf("%v", contexts),
					"adding a context did not extend the list")
			}
			return nil
		})

	rec.check(domain, "repos.removeRequiredStatusChecksContexts",
		"DELETE /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts", func() error {
			var contexts []string
			if _, err := decodeInto(client, http.MethodDelete,
				fmt.Sprintf("repos/%s/%s/branches/%s/protection/required_status_checks/contexts", sc.owner, sc.repo, sc.branch),
				map[string]any{"contexts": []string{"ci/second"}}, &contexts); err != nil {
				return err
			}
			for _, context := range contexts {
				if context == "ci/second" {
					return deviate("ci/second removed", "still present", "removing a context did not take effect")
				}
			}
			return nil
		})

	rec.check(domain, "repos.updateRequiredStatusChecks",
		"PATCH /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks", func() error {
			checks, _, err := client.Repositories.UpdateRequiredStatusChecks(ctx, sc.owner, sc.repo, sc.branch,
				&github.RequiredStatusChecksRequest{Strict: github.Ptr(false)})
			if err != nil {
				return err
			}
			if checks.Strict {
				return deviate("strict false", "true", "the strict update did not persist")
			}
			return nil
		})

	rec.check(domain, "repos.getPullRequestReviewEnforcement",
		"GET /repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews", func() error {
			reviews, _, err := client.Repositories.GetPullRequestReviewEnforcement(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if reviews.RequiredApprovingReviewCount != 2 {
				return deviate("2", fmt.Sprintf("%d", reviews.RequiredApprovingReviewCount),
					"the standalone review-enforcement resource reports a different threshold")
			}
			return nil
		})

	rec.check(domain, "repos.updatePullRequestReviewEnforcement",
		"PATCH /repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews", func() error {
			reviews, _, err := client.Repositories.UpdatePullRequestReviewEnforcement(ctx, sc.owner, sc.repo, sc.branch,
				&github.PullRequestReviewsEnforcementUpdate{RequiredApprovingReviewCount: 1})
			if err != nil {
				return err
			}
			if reviews.RequiredApprovingReviewCount != 1 {
				return deviate("1", fmt.Sprintf("%d", reviews.RequiredApprovingReviewCount),
					"the review-threshold update did not persist")
			}
			return nil
		})

	rec.check(domain, "repos.getAdminEnforcement",
		"GET /repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins", func() error {
			enforcement, _, err := client.Repositories.GetAdminEnforcement(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if !enforcement.Enabled {
				return deviate("enabled true", "false", "the standalone enforce_admins resource disagrees with the protection object")
			}
			return wantField("enforce_admins.url", enforcement.GetURL())
		})

	rec.check(domain, "repos.removeAdminEnforcement / addAdminEnforcement",
		"DELETE and POST /repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins", func() error {
			// Re-assert a known protection first: this operation must stand on
			// its own, not inherit whatever the operations above left behind.
			if err := reprotect(client, sc); err != nil {
				return err
			}
			if _, err := client.Repositories.RemoveAdminEnforcement(ctx, sc.owner, sc.repo, sc.branch); err != nil {
				return err
			}
			enforcement, _, err := client.Repositories.GetAdminEnforcement(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if enforcement.Enabled {
				return deviate("enabled false", "true", "removing admin enforcement did not take effect")
			}
			enforcement, _, err = client.Repositories.AddAdminEnforcement(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if !enforcement.Enabled {
				return deviate("enabled true", "false", "re-adding admin enforcement did not take effect")
			}
			return nil
		})

	rec.check(domain, "repos.requireSignaturesOnProtectedBranch",
		"POST /repos/{owner}/{repo}/branches/{branch}/protection/required_signatures", func() error {
			signatures, _, err := client.Repositories.RequireSignaturesOnProtectedBranch(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if !signatures.GetEnabled() {
				return deviate("enabled true", "false", "requiring signed commits did not take effect")
			}
			got, _, err := client.Repositories.GetSignaturesProtectedBranch(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if !got.GetEnabled() {
				return deviate("enabled true", "false", "the signature requirement is not read back")
			}
			if _, err := client.Repositories.OptionalSignaturesOnProtectedBranch(ctx, sc.owner, sc.repo, sc.branch); err != nil {
				return err
			}
			return nil
		})

	rec.check(domain, "repos.removeBranchProtection",
		"DELETE /repos/{owner}/{repo}/branches/{branch}/protection", func() error {
			if err := reprotect(client, sc); err != nil {
				return err
			}
			resp, err := client.Repositories.RemoveBranchProtection(ctx, sc.owner, sc.repo, sc.branch)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusNoContent, "protection removal"); err != nil {
				return err
			}
			// go-github maps the documented 404 "Branch not protected" onto its
			// own sentinel rather than an ErrorResponse, so that is the value a
			// real caller branches on.
			_, _, err = client.Repositories.GetBranchProtection(ctx, sc.owner, sc.repo, sc.branch)
			if errors.Is(err, github.ErrBranchNotProtected) {
				return nil
			}
			return wantHTTPError(err, http.StatusNotFound, "reading protection after it was removed")
		})

	// --- Rulesets ---------------------------------------------------------
	var rulesetID int64
	rec.check(domain, "repos.createRuleset", "POST /repos/{owner}/{repo}/rulesets", func() error {
		ruleset, _, err := client.Repositories.CreateRuleset(ctx, sc.owner, sc.repo, github.RepositoryRuleset{
			Name:        "conformance-ruleset",
			Target:      github.Ptr(github.RulesetTargetBranch),
			Enforcement: github.RulesetEnforcementActive,
			Conditions: &github.RepositoryRulesetConditions{
				RefName: &github.RepositoryRulesetRefConditionParameters{
					Include: []string{"~DEFAULT_BRANCH"},
					Exclude: []string{},
				},
			},
			Rules: &github.RepositoryRulesetRules{
				Deletion:       &github.EmptyRuleParameters{},
				NonFastForward: &github.EmptyRuleParameters{},
				PullRequest: &github.PullRequestRuleParameters{
					RequiredApprovingReviewCount:   1,
					DismissStaleReviewsOnPush:      true,
					RequiredReviewThreadResolution: true,
				},
			},
		})
		if err != nil {
			return err
		}
		rulesetID = ruleset.GetID()
		if rulesetID == 0 {
			return deviate("a non-zero id", "0", "the created ruleset has no id")
		}
		if ruleset.Name != "conformance-ruleset" {
			return deviate("conformance-ruleset", ruleset.Name, "the ruleset name does not round trip")
		}
		if ruleset.Enforcement != github.RulesetEnforcementActive {
			return deviate("active", string(ruleset.Enforcement), "the ruleset enforcement does not round trip")
		}
		if ruleset.Rules == nil || ruleset.Rules.PullRequest == nil {
			return deviate("the pull_request rule", "absent",
				"the ruleset response drops the rules it was created with")
		}
		if ruleset.Rules.PullRequest.RequiredApprovingReviewCount != 1 {
			return deviate("required_approving_review_count 1",
				fmt.Sprintf("%d", ruleset.Rules.PullRequest.RequiredApprovingReviewCount),
				"pull_request rule parameters do not round trip")
		}
		return nil
	})

	if rulesetID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/rulesets/{id}", "the ruleset fixture was not created",
			"repos.getRuleset", "repos.updateRuleset", "repos.deleteRuleset")
	} else {
		rec.check(domain, "repos.getRuleset", "GET /repos/{owner}/{repo}/rulesets/{id}", func() error {
			ruleset, _, err := client.Repositories.GetRuleset(ctx, sc.owner, sc.repo, rulesetID, false)
			if err != nil {
				return err
			}
			if ruleset.GetID() != rulesetID {
				return deviate(fmt.Sprintf("%d", rulesetID), fmt.Sprintf("%d", ruleset.GetID()),
					"the wrong ruleset came back")
			}
			if ruleset.Conditions == nil || ruleset.Conditions.RefName == nil {
				return deviate("the ref_name condition", "absent", "the ruleset conditions were not persisted")
			}
			return nil
		})

		rec.check(domain, "repos.updateRuleset", "PUT /repos/{owner}/{repo}/rulesets/{id}", func() error {
			ruleset, _, err := client.Repositories.UpdateRuleset(ctx, sc.owner, sc.repo, rulesetID, github.RepositoryRuleset{
				Name:        "conformance-ruleset-renamed",
				Target:      github.Ptr(github.RulesetTargetBranch),
				Enforcement: github.RulesetEnforcementEvaluate,
			})
			if err != nil {
				return err
			}
			if ruleset.Name != "conformance-ruleset-renamed" {
				return deviate("conformance-ruleset-renamed", ruleset.Name, "the ruleset rename did not persist")
			}
			if ruleset.Enforcement != github.RulesetEnforcementEvaluate {
				return deviate("evaluate", string(ruleset.Enforcement), "the enforcement change did not persist")
			}
			return nil
		})
	}

	rec.check(domain, "repos.getAllRulesets", "GET /repos/{owner}/{repo}/rulesets", func() error {
		rulesets, _, err := client.Repositories.GetAllRulesets(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if len(rulesets) < 1 {
			return deviate("at least the ruleset just created", "none", "the ruleset listing is empty")
		}
		return nil
	})

	rec.check(domain, "repos.createRuleset (merge queue)", "POST /repos/{owner}/{repo}/rulesets", func() error {
		// The merge queue is configured through a ruleset rule, which is the
		// only place the Representational State Transfer surface exposes it.
		ruleset, _, err := client.Repositories.CreateRuleset(ctx, sc.owner, sc.repo, github.RepositoryRuleset{
			Name:        "conformance-merge-queue",
			Target:      github.Ptr(github.RulesetTargetBranch),
			Enforcement: github.RulesetEnforcementActive,
			Conditions: &github.RepositoryRulesetConditions{
				RefName: &github.RepositoryRulesetRefConditionParameters{
					Include: []string{"~DEFAULT_BRANCH"},
					Exclude: []string{},
				},
			},
			Rules: &github.RepositoryRulesetRules{
				MergeQueue: &github.MergeQueueRuleParameters{
					CheckResponseTimeoutMinutes:  60,
					GroupingStrategy:             github.MergeGroupingStrategyAllGreen,
					MaxEntriesToBuild:            5,
					MaxEntriesToMerge:            5,
					MergeMethod:                  github.MergeQueueMergeMethodSquash,
					MinEntriesToMerge:            1,
					MinEntriesToMergeWaitMinutes: 5,
				},
			},
		})
		if err != nil {
			return err
		}
		if ruleset.Rules == nil || ruleset.Rules.MergeQueue == nil {
			return deviate("the merge_queue rule", "absent",
				"the merge queue rule is dropped, so a client cannot configure a merge queue")
		}
		if ruleset.Rules.MergeQueue.MergeMethod != github.MergeQueueMergeMethodSquash {
			return deviate("SQUASH", string(ruleset.Rules.MergeQueue.MergeMethod),
				"the merge queue merge method does not round trip")
		}
		if ruleset.Rules.MergeQueue.MaxEntriesToBuild != 5 {
			return deviate("max_entries_to_build 5", fmt.Sprintf("%d", ruleset.Rules.MergeQueue.MaxEntriesToBuild),
				"merge queue parameters do not round trip")
		}
		return nil
	})

	if rulesetID != 0 {
		rec.check(domain, "repos.deleteRuleset", "DELETE /repos/{owner}/{repo}/rulesets/{id}", func() error {
			resp, err := client.Repositories.DeleteRuleset(ctx, sc.owner, sc.repo, rulesetID)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusNoContent, "ruleset deletion"); err != nil {
				return err
			}
			_, _, err = client.Repositories.GetRuleset(ctx, sc.owner, sc.repo, rulesetID, false)
			return wantHTTPError(err, http.StatusNotFound, "reading a deleted ruleset")
		})
	}

	// --- Rename and the redirect it leaves behind -------------------------
	rec.check(domain, "repos.renameBranch", "POST /repos/{owner}/{repo}/branches/{branch}/rename", func() error {
		branch, _, err := client.Repositories.RenameBranch(ctx, sc.owner, sc.repo, sc.branch, "renamed-default")
		if err != nil {
			return err
		}
		if branch.GetName() != "renamed-default" {
			return deviate("renamed-default", branch.GetName(), "the rename response names the old branch")
		}
		repository, _, err := client.Repositories.Get(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if repository.GetDefaultBranch() != "renamed-default" {
			return deviate("renamed-default", repository.GetDefaultBranch(),
				"renaming the default branch did not move the repository's default_branch")
		}
		return nil
	})
}

// runOrgRulesets covers the organization-scoped ruleset surface, which a
// platform team uses to apply one policy across every repository.
func runOrgRulesets(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "branches"
	if set.org == "" {
		skipAll(rec, domain, "POST /orgs/{org}/rulesets", "the organization fixture is unavailable",
			"orgs.createRepositoryRuleset", "orgs.getRepositoryRuleset", "orgs.listAllRepositoryRulesets",
			"orgs.updateRepositoryRuleset", "orgs.deleteRepositoryRuleset")
		return
	}

	var rulesetID int64
	rec.check(domain, "orgs.createRepositoryRuleset", "POST /orgs/{org}/rulesets", func() error {
		ruleset, _, err := client.Organizations.CreateRepositoryRuleset(ctx, set.org, github.RepositoryRuleset{
			Name:        "org-conformance-ruleset",
			Target:      github.Ptr(github.RulesetTargetBranch),
			Enforcement: github.RulesetEnforcementActive,
			Conditions: &github.RepositoryRulesetConditions{
				RefName: &github.RepositoryRulesetRefConditionParameters{
					Include: []string{"~ALL"},
					Exclude: []string{},
				},
				RepositoryName: &github.RepositoryRulesetRepositoryNamesConditionParameters{
					Include: []string{"~ALL"},
					Exclude: []string{},
				},
			},
			Rules: &github.RepositoryRulesetRules{
				Deletion: &github.EmptyRuleParameters{},
			},
		})
		if err != nil {
			return err
		}
		rulesetID = ruleset.GetID()
		if rulesetID == 0 {
			return deviate("a non-zero id", "0", "the created organization ruleset has no id")
		}
		if ruleset.SourceType == nil || *ruleset.SourceType != github.RulesetSourceTypeOrganization {
			return deviate("Organization", fmt.Sprintf("%v", ruleset.SourceType),
				"an organization ruleset does not report source_type Organization")
		}
		return nil
	})

	rec.check(domain, "orgs.listAllRepositoryRulesets", "GET /orgs/{org}/rulesets", func() error {
		rulesets, _, err := client.Organizations.ListAllRepositoryRulesets(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if len(rulesets) < 1 {
			return deviate("at least the ruleset just created", "none", "the organization ruleset listing is empty")
		}
		return nil
	})

	if rulesetID == 0 {
		skipAll(rec, domain, "GET /orgs/{org}/rulesets/{id}", "the organization ruleset fixture was not created",
			"orgs.getRepositoryRuleset", "orgs.updateRepositoryRuleset", "orgs.deleteRepositoryRuleset")
		return
	}

	rec.check(domain, "orgs.getRepositoryRuleset", "GET /orgs/{org}/rulesets/{id}", func() error {
		ruleset, _, err := client.Organizations.GetRepositoryRuleset(ctx, set.org, rulesetID)
		if err != nil {
			return err
		}
		if ruleset.GetID() != rulesetID {
			return deviate(fmt.Sprintf("%d", rulesetID), fmt.Sprintf("%d", ruleset.GetID()),
				"the wrong organization ruleset came back")
		}
		return nil
	})

	rec.check(domain, "orgs.updateRepositoryRuleset", "PUT /orgs/{org}/rulesets/{id}", func() error {
		ruleset, _, err := client.Organizations.UpdateRepositoryRuleset(ctx, set.org, rulesetID, github.RepositoryRuleset{
			Name:        "org-conformance-ruleset",
			Target:      github.Ptr(github.RulesetTargetBranch),
			Enforcement: github.RulesetEnforcementDisabled,
		})
		if err != nil {
			return err
		}
		if ruleset.Enforcement != github.RulesetEnforcementDisabled {
			return deviate("disabled", string(ruleset.Enforcement), "the enforcement change did not persist")
		}
		return nil
	})

	rec.check(domain, "orgs.deleteRepositoryRuleset", "DELETE /orgs/{org}/rulesets/{id}", func() error {
		resp, err := client.Organizations.DeleteRepositoryRuleset(ctx, set.org, rulesetID)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "organization ruleset deletion")
	})
}

// reprotect re-applies a known branch protection so an operation that changes
// or removes protection starts from a defined state instead of inheriting the
// previous operation's leftovers.
func reprotect(client *github.Client, sc *scratch) error {
	_, _, err := client.Repositories.UpdateBranchProtection(ctx, sc.owner, sc.repo, sc.branch,
		&github.ProtectionRequest{
			RequiredStatusChecks: &github.RequiredStatusChecks{
				Strict:   true,
				Contexts: &[]string{"ci/conformance"},
			},
			RequiredPullRequestReviews: &github.PullRequestReviewsEnforcementRequest{
				RequiredApprovingReviewCount: 1,
			},
			EnforceAdmins: true,
		})
	return err
}
