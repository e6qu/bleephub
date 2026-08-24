package main

import (
	"fmt"
	"net/http"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runDeployments covers deployments, their statuses, and the environments and
// protection rules that gate them — the surface every deployment tool drives.
func runDeployments(client *github.Client, rec *recorder, set *fixtureSet, guest *principal) {
	const domain = "deployments"
	sc := newScratch(client, set.owner, "conformance-deployments")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the deployment repository fixture could not be provisioned",
			"repos.createDeployment (full)", "repos.getDeployment", "repos.listDeployments (environment filter)",
			"repos.createDeploymentStatus", "repos.listDeploymentStatuses", "repos.getDeploymentStatus",
			"repos.deleteDeployment", "repos.createUpdateEnvironment (protection rules)",
			"repos.getEnvironment", "repos.listEnvironments", "repos.createDeploymentBranchPolicy",
			"repos.listDeploymentBranchPolicies", "repos.deleteEnvironment")
		return
	}

	// An environment must exist before a deployment can name it.
	rec.check(domain, "repos.createUpdateEnvironment (protection rules)",
		"PUT /repos/{owner}/{repo}/environments/{environment_name}", func() error {
			reviewers := []*github.EnvReviewers{}
			if guest.ok() {
				user, _, err := client.Users.Get(ctx, guest.login)
				if err != nil {
					return err
				}
				reviewers = append(reviewers, &github.EnvReviewers{
					Type: github.Ptr("User"), ID: github.Ptr(user.GetID()),
				})
			}
			environment, _, err := client.Repositories.CreateUpdateEnvironment(ctx, sc.owner, sc.repo, "production",
				&github.CreateUpdateEnvironment{
					WaitTimer:         github.Ptr(1),
					Reviewers:         reviewers,
					CanAdminsBypass:   github.Ptr(false),
					PreventSelfReview: github.Ptr(true),
					DeploymentBranchPolicy: &github.BranchPolicy{
						ProtectedBranches:    github.Ptr(false),
						CustomBranchPolicies: github.Ptr(true),
					},
				})
			if err != nil {
				return err
			}
			if environment.GetName() != "production" {
				return deviate("production", environment.GetName(), "the environment name is wrong")
			}
			if environment.GetNodeID() == "" {
				return deviate("node_id populated", "empty", "an environment carries no node_id")
			}
			if len(environment.ProtectionRules) == 0 {
				return deviate("the wait timer and reviewer protection rules", "none",
					"the environment reports no protection rules, so a client cannot show what gates a deployment")
			}
			foundWait := false
			for _, rule := range environment.ProtectionRules {
				if rule.GetType() == "wait_timer" && rule.GetWaitTimer() == 1 {
					foundWait = true
				}
			}
			if !foundWait {
				return deviate("a wait_timer protection rule of 1 minute", "absent",
					"the wait timer does not round trip as a protection rule")
			}
			if environment.GetDeploymentBranchPolicy() == nil ||
				!environment.GetDeploymentBranchPolicy().GetCustomBranchPolicies() {
				return deviate("deployment_branch_policy.custom_branch_policies true", "absent or false",
					"the deployment branch policy does not round trip")
			}
			return nil
		})

	rec.check(domain, "repos.getEnvironment", "GET /repos/{owner}/{repo}/environments/{environment_name}", func() error {
		environment, _, err := client.Repositories.GetEnvironment(ctx, sc.owner, sc.repo, "production")
		if err != nil {
			return err
		}
		if environment.GetID() == 0 {
			return deviate("a non-zero id", "0", "the environment has no id")
		}
		if environment.GetHTMLURL() == "" {
			return deviate("html_url populated", "empty", "an environment carries no html_url")
		}
		return nil
	})

	rec.check(domain, "repos.listEnvironments (total_count)", "GET /repos/{owner}/{repo}/environments", func() error {
		environments, _, err := client.Repositories.ListEnvironments(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if environments.GetTotalCount() < 1 {
			return deviate("at least the environment just created",
				fmt.Sprintf("total_count %d", environments.GetTotalCount()),
				"the environment listing is empty")
		}
		return nil
	})

	var branchPolicyID int64
	rec.check(domain, "repos.createDeploymentBranchPolicy",
		"POST /repos/{owner}/{repo}/environments/{env}/deployment-branch-policies", func() error {
			policy, _, err := client.Repositories.CreateDeploymentBranchPolicy(ctx, sc.owner, sc.repo, "production",
				&github.DeploymentBranchPolicyRequest{Name: github.Ptr("release/*")})
			if err != nil {
				return err
			}
			branchPolicyID = policy.GetID()
			if policy.GetName() != "release/*" {
				return deviate("release/*", policy.GetName(), "the branch policy pattern does not round trip")
			}
			return nil
		})

	rec.check(domain, "repos.listDeploymentBranchPolicies",
		"GET /repos/{owner}/{repo}/environments/{env}/deployment-branch-policies", func() error {
			policies, _, err := client.Repositories.ListDeploymentBranchPolicies(ctx, sc.owner, sc.repo, "production", nil)
			if err != nil {
				return err
			}
			if policies.GetTotalCount() < 1 {
				return deviate("the branch policy just created",
					fmt.Sprintf("total_count %d", policies.GetTotalCount()),
					"the branch policy listing is empty")
			}
			return nil
		})

	if branchPolicyID != 0 {
		rec.check(domain, "repos.updateDeploymentBranchPolicy / deleteDeploymentBranchPolicy",
			"PUT and DELETE /repos/{owner}/{repo}/environments/{env}/deployment-branch-policies/{id}", func() error {
				policy, _, err := client.Repositories.UpdateDeploymentBranchPolicy(ctx, sc.owner, sc.repo, "production",
					branchPolicyID, &github.DeploymentBranchPolicyRequest{Name: github.Ptr("hotfix/*")})
				if err != nil {
					return err
				}
				if policy.GetName() != "hotfix/*" {
					return deviate("hotfix/*", policy.GetName(), "the branch policy update did not persist")
				}
				resp, err := client.Repositories.DeleteDeploymentBranchPolicy(ctx, sc.owner, sc.repo, "production", branchPolicyID)
				if err != nil {
					return err
				}
				return wantStatus(resp, http.StatusNoContent, "deleting a deployment branch policy")
			})
	} else {
		rec.skip1(domain, "repos.updateDeploymentBranchPolicy / deleteDeploymentBranchPolicy",
			"PUT and DELETE /repos/{owner}/{repo}/environments/{env}/deployment-branch-policies/{id}",
			"the branch policy fixture was not created")
	}

	rec.check(domain, "repos.getAllDeploymentProtectionRules",
		"GET /repos/{owner}/{repo}/environments/{env}/deployment_protection_rules", func() error {
			rules, _, err := client.Repositories.GetAllDeploymentProtectionRules(ctx, sc.owner, sc.repo, "production")
			if err != nil {
				return err
			}
			if rules == nil {
				return deviate("a protection rule envelope", "nil",
					"the custom deployment protection rule listing did not decode")
			}
			return nil
		})

	// --- Deployments -------------------------------------------------------
	var deploymentID int64
	rec.check(domain, "repos.createDeployment (full)", "POST /repos/{owner}/{repo}/deployments", func() error {
		deployment, _, err := client.Repositories.CreateDeployment(ctx, sc.owner, sc.repo, &github.DeploymentRequest{
			Ref:                   github.Ptr(sc.branch),
			Task:                  github.Ptr("deploy"),
			AutoMerge:             github.Ptr(false),
			RequiredContexts:      &[]string{},
			Environment:           github.Ptr("production"),
			Description:           github.Ptr("created by the conformance harness"),
			Payload:               map[string]any{"release": "conformance"},
			ProductionEnvironment: github.Ptr(true),
		})
		if err != nil {
			return err
		}
		deploymentID = deployment.GetID()
		if deploymentID == 0 {
			return deviate("a non-zero id", "0", "the created deployment has no id")
		}
		if deployment.GetEnvironment() != "production" {
			return deviate("production", deployment.GetEnvironment(), "the deployment environment does not round trip")
		}
		if deployment.GetTask() != "deploy" {
			return deviate("deploy", deployment.GetTask(), "the deployment task does not round trip")
		}
		if deployment.GetSHA() == "" {
			return deviate("sha populated", "empty",
				"a deployment does not resolve its ref to a sha, so a client cannot tell what was deployed")
		}
		if deployment.GetCreator().GetLogin() == "" {
			return deviate("creator.login populated", "empty", "a deployment has no creator")
		}
		if deployment.GetStatusesURL() == "" {
			return deviate("statuses_url populated", "empty",
				"a deployment omits the link a client follows to post its statuses")
		}
		return wantField("deployment.node_id", deployment.GetNodeID())
	})

	if deploymentID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/deployments/{deployment_id}",
			"the deployment fixture was not created",
			"repos.getDeployment", "repos.listDeployments (environment filter)",
			"repos.createDeploymentStatus", "repos.listDeploymentStatuses",
			"repos.getDeploymentStatus", "repos.deleteDeployment")
		return
	}

	rec.check(domain, "repos.getDeployment", "GET /repos/{owner}/{repo}/deployments/{deployment_id}", func() error {
		deployment, _, err := client.Repositories.GetDeployment(ctx, sc.owner, sc.repo, deploymentID)
		if err != nil {
			return err
		}
		if deployment.GetID() != deploymentID {
			return deviate(fmt.Sprintf("%d", deploymentID), fmt.Sprintf("%d", deployment.GetID()),
				"the wrong deployment came back")
		}
		if deployment.Payload == nil {
			return deviate("the payload the deployment was created with", "absent",
				"the deployment payload is not stored, so a deployment tool loses its own metadata")
		}
		return nil
	})

	rec.check(domain, "repos.listDeployments (environment filter)",
		"GET /repos/{owner}/{repo}/deployments?environment=", func() error {
			deployments, _, err := client.Repositories.ListDeployments(ctx, sc.owner, sc.repo,
				&github.DeploymentsListOptions{Environment: "production"})
			if err != nil {
				return err
			}
			if len(deployments) != 1 {
				return deviate("the one production deployment", fmt.Sprintf("%d", len(deployments)),
					"the environment filter does not select the deployment")
			}
			none, _, err := client.Repositories.ListDeployments(ctx, sc.owner, sc.repo,
				&github.DeploymentsListOptions{Environment: "no-such-environment"})
			if err != nil {
				return err
			}
			if len(none) != 0 {
				return deviate("no deployments", fmt.Sprintf("%d", len(none)),
					"the environment filter is ignored")
			}
			return nil
		})

	var statusID int64
	rec.check(domain, "repos.createDeploymentStatus",
		"POST /repos/{owner}/{repo}/deployments/{deployment_id}/statuses", func() error {
			status, _, err := client.Repositories.CreateDeploymentStatus(ctx, sc.owner, sc.repo, deploymentID,
				&github.DeploymentStatusRequest{
					State:          github.Ptr("in_progress"),
					Description:    github.Ptr("rolling out"),
					LogURL:         github.Ptr("https://example.invalid/logs"),
					EnvironmentURL: github.Ptr("https://example.invalid/production"),
				})
			if err != nil {
				return err
			}
			statusID = status.GetID()
			if status.GetState() != "in_progress" {
				return deviate("in_progress", status.GetState(), "the deployment status state does not round trip")
			}
			if status.GetLogURL() != "https://example.invalid/logs" {
				return deviate("the log_url sent", status.GetLogURL(), "log_url does not round trip")
			}
			if status.GetEnvironmentURL() != "https://example.invalid/production" {
				return deviate("the environment_url sent", status.GetEnvironmentURL(),
					"environment_url does not round trip, so a client cannot link to the deployed environment")
			}
			if status.GetDeploymentURL() == "" {
				return deviate("deployment_url populated", "empty",
					"a deployment status does not link back to its deployment")
			}
			return wantField("deployment_status.node_id", status.GetNodeID())
		})

	rec.check(domain, "repos.listDeploymentStatuses",
		"GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses", func() error {
			statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, sc.owner, sc.repo, deploymentID, nil)
			if err != nil {
				return err
			}
			if len(statuses) != 1 {
				return deviate("the one status posted", fmt.Sprintf("%d", len(statuses)),
					"the deployment status listing is wrong")
			}
			return nil
		})

	if statusID != 0 {
		rec.check(domain, "repos.getDeploymentStatus",
			"GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses/{status_id}", func() error {
				status, _, err := client.Repositories.GetDeploymentStatus(ctx, sc.owner, sc.repo, deploymentID, statusID)
				if err != nil {
					return err
				}
				if status.GetID() != statusID {
					return deviate(fmt.Sprintf("%d", statusID), fmt.Sprintf("%d", status.GetID()),
						"the wrong deployment status came back")
				}
				return nil
			})
	} else {
		rec.skip1(domain, "repos.getDeploymentStatus",
			"GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses/{status_id}",
			"the deployment status fixture was not created")
	}

	rec.check(domain, "repos.deleteDeployment", "DELETE /repos/{owner}/{repo}/deployments/{deployment_id}", func() error {
		// GitHub refuses to delete an active deployment, so it is first marked
		// inactive — which is the sequence a real deployment tool follows.
		if _, _, err := client.Repositories.CreateDeploymentStatus(ctx, sc.owner, sc.repo, deploymentID,
			&github.DeploymentStatusRequest{State: github.Ptr("inactive")}); err != nil {
			return err
		}
		resp, err := client.Repositories.DeleteDeployment(ctx, sc.owner, sc.repo, deploymentID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "deleting a deployment"); err != nil {
			return err
		}
		_, _, err = client.Repositories.GetDeployment(ctx, sc.owner, sc.repo, deploymentID)
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted deployment")
	})

	rec.check(domain, "repos.deleteEnvironment", "DELETE /repos/{owner}/{repo}/environments/{env}", func() error {
		resp, err := client.Repositories.DeleteEnvironment(ctx, sc.owner, sc.repo, "production")
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "deleting an environment"); err != nil {
			return err
		}
		_, _, err = client.Repositories.GetEnvironment(ctx, sc.owner, sc.repo, "production")
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted environment")
	})
}

// runWebhooks covers repository and organization webhooks end to end,
// including the delivery log every integration debugs against.
func runWebhooks(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "webhooks"
	sc := newScratch(client, set.owner, "conformance-webhooks")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the webhook repository fixture could not be provisioned",
			"repos.createWebhook (config round trip)", "repos.getWebhook", "repos.listWebhooks",
			"repos.updateWebhook", "repos.getWebhookConfig", "repos.updateWebhookConfig",
			"repos.pingWebhook", "repos.testPushWebhook", "repos.listWebhookDeliveries",
			"repos.getWebhookDelivery", "repos.redeliverWebhookDelivery", "repos.deleteWebhook")
		return
	}

	var hookID int64
	rec.check(domain, "repos.createWebhook (config round trip)", "POST /repos/{owner}/{repo}/hooks", func() error {
		hook, resp, err := client.Repositories.CreateHook(ctx, sc.owner, sc.repo, &github.Hook{
			Events: []string{"push", "pull_request", "issues"},
			Active: github.Ptr(true),
			Config: &github.HookConfig{
				URL:         github.Ptr("https://example.invalid/webhook"),
				ContentType: github.Ptr("json"),
				InsecureSSL: github.Ptr("1"),
				Secret:      github.Ptr("conformance-secret"),
			},
		})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "creating a webhook"); err != nil {
			return err
		}
		hookID = hook.GetID()
		if hookID == 0 {
			return deviate("a non-zero id", "0", "the created webhook has no id")
		}
		if hook.GetConfig().GetContentType() != "json" {
			return deviate("json", hook.GetConfig().GetContentType(), "content_type does not round trip")
		}
		if hook.GetConfig().GetInsecureSSL() != "1" {
			return deviate("1", hook.GetConfig().GetInsecureSSL(), "insecure_ssl does not round trip")
		}
		if len(hook.Events) != 3 {
			return deviate("three subscribed events", fmt.Sprintf("%v", hook.Events),
				"the event subscription does not round trip")
		}
		if hook.GetPingURL() == "" || hook.GetTestURL() == "" {
			return deviate("ping_url and test_url populated", "empty",
				"a webhook omits the hypermedia clients use to exercise it")
		}
		if hook.GetConfig().GetSecret() == "conformance-secret" {
			return deviate("the secret redacted", "the secret echoed back verbatim",
				"the webhook secret is returned in clear, which leaks a shared credential to any reader of the hook")
		}
		return nil
	})

	if hookID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/hooks/{hook_id}", "the webhook fixture was not created",
			"repos.getWebhook", "repos.listWebhooks", "repos.updateWebhook", "repos.getWebhookConfig",
			"repos.updateWebhookConfig", "repos.pingWebhook", "repos.testPushWebhook",
			"repos.listWebhookDeliveries", "repos.getWebhookDelivery", "repos.redeliverWebhookDelivery",
			"repos.deleteWebhook")
		return
	}

	rec.check(domain, "repos.getWebhook", "GET /repos/{owner}/{repo}/hooks/{hook_id}", func() error {
		hook, _, err := client.Repositories.GetHook(ctx, sc.owner, sc.repo, hookID)
		if err != nil {
			return err
		}
		if hook.GetID() != hookID {
			return deviate(fmt.Sprintf("%d", hookID), fmt.Sprintf("%d", hook.GetID()), "the wrong webhook came back")
		}
		if hook.GetType() != "Repository" {
			return deviate("Repository", hook.GetType(), "the webhook type is wrong")
		}
		return nil
	})

	rec.check(domain, "repos.listWebhooks", "GET /repos/{owner}/{repo}/hooks", func() error {
		hooks, _, err := client.Repositories.ListHooks(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if len(hooks) != 1 {
			return deviate("the one webhook", fmt.Sprintf("%d", len(hooks)), "the webhook listing is wrong")
		}
		return nil
	})

	rec.check(domain, "repos.updateWebhook", "PATCH /repos/{owner}/{repo}/hooks/{hook_id}", func() error {
		hook, _, err := client.Repositories.EditHook(ctx, sc.owner, sc.repo, hookID, &github.Hook{
			Events: []string{"push"},
			Active: github.Ptr(false),
			Config: &github.HookConfig{
				URL:         github.Ptr("https://example.invalid/webhook"),
				ContentType: github.Ptr("form"),
				InsecureSSL: github.Ptr("0"),
			},
		})
		if err != nil {
			return err
		}
		if hook.GetActive() {
			return deviate("active false", "true", "deactivating a webhook did not take effect")
		}
		if hook.GetConfig().GetContentType() != "form" {
			return deviate("form", hook.GetConfig().GetContentType(), "the content_type change did not persist")
		}
		if len(hook.Events) != 1 || hook.Events[0] != "push" {
			return deviate("[push]", fmt.Sprintf("%v", hook.Events), "the event change did not persist")
		}
		return nil
	})

	rec.check(domain, "repos.getWebhookConfig", "GET /repos/{owner}/{repo}/hooks/{hook_id}/config", func() error {
		config, _, err := client.Repositories.GetHookConfiguration(ctx, sc.owner, sc.repo, hookID)
		if err != nil {
			return err
		}
		if config.GetURL() != "https://example.invalid/webhook" {
			return deviate("the configured url", config.GetURL(), "the standalone config resource has the wrong url")
		}
		if config.GetContentType() != "form" {
			return deviate("form", config.GetContentType(),
				"the standalone config resource disagrees with the webhook object")
		}
		return nil
	})

	rec.check(domain, "repos.updateWebhookConfig", "PATCH /repos/{owner}/{repo}/hooks/{hook_id}/config", func() error {
		config, _, err := client.Repositories.EditHookConfiguration(ctx, sc.owner, sc.repo, hookID, &github.HookConfig{
			ContentType: github.Ptr("json"),
		})
		if err != nil {
			return err
		}
		if config.GetContentType() != "json" {
			return deviate("json", config.GetContentType(), "the config update did not persist")
		}
		return nil
	})

	rec.check(domain, "repos.pingWebhook", "POST /repos/{owner}/{repo}/hooks/{hook_id}/pings", func() error {
		resp, err := client.Repositories.PingHook(ctx, sc.owner, sc.repo, hookID)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "pinging a webhook")
	})

	rec.check(domain, "repos.testPushWebhook", "POST /repos/{owner}/{repo}/hooks/{hook_id}/tests", func() error {
		resp, err := client.Repositories.TestHook(ctx, sc.owner, sc.repo, hookID)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "testing a webhook")
	})

	var deliveryID int64
	rec.check(domain, "repos.listWebhookDeliveries", "GET /repos/{owner}/{repo}/hooks/{hook_id}/deliveries", func() error {
		// A delivery is recorded asynchronously with respect to the ping that
		// caused it, so this polls on a bounded deadline rather than assuming
		// the log is already written.
		var deliveries []*github.HookDelivery
		if err := pollUntil("a delivery appears in the webhook's log", 15*time.Second, func() (bool, error) {
			listed, _, err := client.Repositories.ListHookDeliveries(ctx, sc.owner, sc.repo, hookID, nil)
			if err != nil {
				return false, err
			}
			deliveries = listed
			return len(listed) > 0, nil
		}); err != nil {
			return deviate("at least the ping delivery", "none",
				"the delivery log is still empty 15s after a ping, so an integrator has nothing to debug against")
		}
		deliveryID = deliveries[0].GetID()
		delivery := deliveries[0]
		if delivery.GetGUID() == "" {
			return deviate("guid populated", "empty",
				"a delivery has no guid, which is what an integrator correlates with the X-GitHub-Delivery header")
		}
		if delivery.GetEvent() == "" {
			return deviate("event populated", "empty", "a delivery does not say which event it carried")
		}
		if delivery.GetDeliveredAt().IsZero() {
			return deviate("delivered_at populated", "zero", "a delivery has no timestamp")
		}
		return nil
	})

	if deliveryID != 0 {
		rec.check(domain, "repos.getWebhookDelivery",
			"GET /repos/{owner}/{repo}/hooks/{hook_id}/deliveries/{delivery_id}", func() error {
				delivery, _, err := client.Repositories.GetHookDelivery(ctx, sc.owner, sc.repo, hookID, deliveryID)
				if err != nil {
					return err
				}
				if delivery.GetID() != deliveryID {
					return deviate(fmt.Sprintf("%d", deliveryID), fmt.Sprintf("%d", delivery.GetID()),
						"the wrong delivery came back")
				}
				if delivery.Request == nil || delivery.Request.RawPayload == nil || len(*delivery.Request.RawPayload) == 0 {
					return deviate("request.payload populated", "absent",
						"a single delivery omits the request payload, which is the only reason to fetch one")
				}
				if delivery.Request.Headers == nil {
					return deviate("request.headers populated", "absent",
						"a single delivery omits the request headers an integrator inspects")
				}
				return nil
			})

		rec.check(domain, "repos.redeliverWebhookDelivery",
			"POST /repos/{owner}/{repo}/hooks/{hook_id}/deliveries/{delivery_id}/attempts", func() error {
				_, _, err := client.Repositories.RedeliverHookDelivery(ctx, sc.owner, sc.repo, hookID, deliveryID)
				return err
			})
	} else {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/hooks/{hook_id}/deliveries/{delivery_id}",
			"no delivery was recorded to read back",
			"repos.getWebhookDelivery", "repos.redeliverWebhookDelivery")
	}

	rec.check(domain, "repos.deleteWebhook", "DELETE /repos/{owner}/{repo}/hooks/{hook_id}", func() error {
		resp, err := client.Repositories.DeleteHook(ctx, sc.owner, sc.repo, hookID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "deleting a webhook"); err != nil {
			return err
		}
		_, _, err = client.Repositories.GetHook(ctx, sc.owner, sc.repo, hookID)
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted webhook")
	})

	// --- Organization webhooks --------------------------------------------
	if set.org == "" {
		skipAll(rec, domain, "POST /orgs/{org}/hooks", "the organization fixture is unavailable",
			"orgs.createWebhook", "orgs.getWebhook", "orgs.updateWebhook", "orgs.pingWebhook",
			"orgs.listWebhookDeliveries", "orgs.deleteWebhook")
		return
	}

	var orgHookID int64
	rec.check(domain, "orgs.createWebhook", "POST /orgs/{org}/hooks", func() error {
		hook, _, err := client.Organizations.CreateHook(ctx, set.org, &github.Hook{
			Name:   github.Ptr("web"),
			Events: []string{"repository"},
			Active: github.Ptr(true),
			Config: &github.HookConfig{
				URL:         github.Ptr("https://example.invalid/org-webhook"),
				ContentType: github.Ptr("json"),
			},
		})
		if err != nil {
			return err
		}
		orgHookID = hook.GetID()
		if orgHookID == 0 {
			return deviate("a non-zero id", "0", "the created organization webhook has no id")
		}
		if hook.GetType() != "Organization" {
			return deviate("Organization", hook.GetType(), "an organization webhook reports the wrong type")
		}
		return nil
	})

	if orgHookID == 0 {
		skipAll(rec, domain, "GET /orgs/{org}/hooks/{hook_id}", "the organization webhook fixture was not created",
			"orgs.getWebhook", "orgs.updateWebhook", "orgs.pingWebhook",
			"orgs.listWebhookDeliveries", "orgs.deleteWebhook")
		return
	}

	rec.check(domain, "orgs.getWebhook", "GET /orgs/{org}/hooks/{hook_id}", func() error {
		hook, _, err := client.Organizations.GetHook(ctx, set.org, orgHookID)
		if err != nil {
			return err
		}
		if hook.GetID() != orgHookID {
			return deviate(fmt.Sprintf("%d", orgHookID), fmt.Sprintf("%d", hook.GetID()),
				"the wrong organization webhook came back")
		}
		return nil
	})

	rec.check(domain, "orgs.updateWebhook", "PATCH /orgs/{org}/hooks/{hook_id}", func() error {
		hook, _, err := client.Organizations.EditHook(ctx, set.org, orgHookID, &github.Hook{
			Events: []string{"repository", "team"},
		})
		if err != nil {
			return err
		}
		if len(hook.Events) != 2 {
			return deviate("two subscribed events", fmt.Sprintf("%v", hook.Events),
				"the organization webhook event change did not persist")
		}
		return nil
	})

	rec.check(domain, "orgs.pingWebhook", "POST /orgs/{org}/hooks/{hook_id}/pings", func() error {
		resp, err := client.Organizations.PingHook(ctx, set.org, orgHookID)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "pinging an organization webhook")
	})

	rec.check(domain, "orgs.listWebhookDeliveries", "GET /orgs/{org}/hooks/{hook_id}/deliveries", func() error {
		if err := pollUntil("a delivery appears in the organization webhook's log", 15*time.Second,
			func() (bool, error) {
				deliveries, _, err := client.Organizations.ListHookDeliveries(ctx, set.org, orgHookID, nil)
				if err != nil {
					return false, err
				}
				return len(deliveries) > 0, nil
			}); err != nil {
			return deviate("at least the ping delivery", "none",
				"an organization webhook records no deliveries 15s after a ping")
		}
		return nil
	})

	rec.check(domain, "orgs.deleteWebhook", "DELETE /orgs/{org}/hooks/{hook_id}", func() error {
		resp, err := client.Organizations.DeleteHook(ctx, set.org, orgHookID)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "deleting an organization webhook")
	})
}
