package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runDiscovery covers the read-mostly surfaces a client touches around the
// edges of the main object model: licences, gitignore templates, codes of
// conduct, traffic, forks, stars and watchers, notifications, topics,
// autolinks, Pages, packages and Codespaces.
func runDiscovery(client *github.Client, rec *recorder, set *fixtureSet) {
	sc := newScratch(client, set.owner, "conformance-discovery")

	// --- Static metadata ---------------------------------------------------
	rec.check("meta", "licenses.list", "GET /licenses", func() error {
		licenses, _, err := client.Licenses.List(ctx, nil)
		if err != nil {
			return err
		}
		if len(licenses) == 0 {
			return deviate("the common licence list", "empty", "/licenses returned nothing")
		}
		for _, license := range licenses {
			if license.GetKey() == "" || license.GetSPDXID() == "" {
				return deviate("key and spdx_id on every licence", "an incomplete entry",
					"a licence entry is missing the identifiers clients match on")
			}
		}
		return nil
	})

	rec.check("meta", "licenses.get", "GET /licenses/{license}", func() error {
		license, _, err := client.Licenses.Get(ctx, "mit")
		if err != nil {
			return err
		}
		if license.GetSPDXID() != "MIT" {
			return deviate("MIT", license.GetSPDXID(), "the MIT licence has the wrong SPDX identifier")
		}
		if license.GetBody() == "" {
			return deviate("the licence body", "empty",
				"the licence has no body, which is the only reason to fetch one")
		}
		if license.Permissions == nil || len(*license.Permissions) == 0 {
			return deviate("a permissions list", "empty", "the licence carries no permissions list")
		}
		return nil
	})

	rec.check("meta", "gitignore.list", "GET /gitignore/templates", func() error {
		templates, _, err := client.Gitignores.List(ctx)
		if err != nil {
			return err
		}
		if len(templates) == 0 {
			return deviate("the gitignore template list", "empty", "/gitignore/templates returned nothing")
		}
		return nil
	})

	rec.check("meta", "gitignore.get", "GET /gitignore/templates/{name}", func() error {
		template, _, err := client.Gitignores.Get(ctx, "Go")
		if err != nil {
			return err
		}
		if template.GetName() != "Go" {
			return deviate("Go", template.GetName(), "the gitignore template has the wrong name")
		}
		return wantField("gitignore.source", template.GetSource())
	})

	rec.check("meta", "codesOfConduct.list", "GET /codes_of_conduct", func() error {
		var codes []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if _, err := decodeInto(client, http.MethodGet, "codes_of_conduct", nil, &codes); err != nil {
			return err
		}
		if len(codes) == 0 {
			return deviate("the code-of-conduct list", "empty", "/codes_of_conduct returned nothing")
		}
		for _, code := range codes {
			if code.Key == "" || code.Name == "" || code.URL == "" {
				return deviate("key, name and url on every entry", "an incomplete entry",
					"a code-of-conduct entry is missing required fields")
			}
		}
		return nil
	})

	if !sc.ok() {
		skipAll(rec, "repos", "POST /user/repos", "the discovery repository fixture could not be provisioned",
			"repos.getLicense", "repos.listTrafficViews", "repos.listTrafficClones",
			"repos.listTrafficPaths", "repos.listTrafficReferrers", "repos.addAutolink",
			"repos.listAutolinks", "repos.deleteAutolink", "activity.listWatchers",
			"activity.getRepositorySubscription", "repos.enablePages", "repos.getPagesInfo")
		return
	}

	rec.check("repos", "repos.getLicense", "GET /repos/{owner}/{repo}/license", func() error {
		if _, err := commitFile(client, sc, "LICENSE", "add a licence",
			"MIT License\n\nCopyright (c) 2026\n\nPermission is hereby granted, free of charge, "+
				"to any person obtaining a copy of this software and associated documentation files "+
				"(the \"Software\"), to deal in the Software without restriction.\n"); err != nil {
			return err
		}
		var license struct {
			License *struct {
				Key string `json:"key"`
			} `json:"license"`
			Content string `json:"content"`
		}
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s/license", sc.owner, sc.repo), nil, &license); err != nil {
			return err
		}
		if license.License == nil || license.License.Key == "" {
			return deviate("a detected licence", "none",
				"the repository licence resource does not identify the licence file that was just committed")
		}
		if license.Content == "" {
			return deviate("the licence file content", "empty",
				"the repository licence resource carries no content")
		}
		return nil
	})

	// --- Traffic -----------------------------------------------------------
	rec.check("traffic", "repos.listTrafficViews", "GET /repos/{owner}/{repo}/traffic/views", func() error {
		views, _, err := client.Repositories.ListTrafficViews(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if views == nil {
			return deviate("a traffic envelope", "nil", "the traffic view response did not decode")
		}
		return nil
	})

	rec.check("traffic", "repos.listTrafficClones", "GET /repos/{owner}/{repo}/traffic/clones", func() error {
		clones, _, err := client.Repositories.ListTrafficClones(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if clones == nil {
			return deviate("a traffic envelope", "nil", "the traffic clone response did not decode")
		}
		return nil
	})

	rec.check("traffic", "repos.listTrafficPaths", "GET /repos/{owner}/{repo}/traffic/popular/paths", func() error {
		_, _, err := client.Repositories.ListTrafficPaths(ctx, sc.owner, sc.repo)
		return err
	})

	rec.check("traffic", "repos.listTrafficReferrers", "GET /repos/{owner}/{repo}/traffic/popular/referrers", func() error {
		_, _, err := client.Repositories.ListTrafficReferrers(ctx, sc.owner, sc.repo)
		return err
	})

	// --- Autolinks ---------------------------------------------------------
	var autolinkID int64
	rec.check("repos", "repos.addAutolink", "POST /repos/{owner}/{repo}/autolinks", func() error {
		autolink, resp, err := client.Repositories.AddAutolink(ctx, sc.owner, sc.repo, &github.AutolinkOptions{
			KeyPrefix:      github.Ptr("TICKET-"),
			URLTemplate:    github.Ptr("https://example.invalid/browse/TICKET-<num>"),
			IsAlphanumeric: github.Ptr(false),
		})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "creating an autolink"); err != nil {
			return err
		}
		autolinkID = autolink.GetID()
		if autolink.GetKeyPrefix() != "TICKET-" {
			return deviate("TICKET-", autolink.GetKeyPrefix(), "the autolink key prefix does not round trip")
		}
		if autolink.GetURLTemplate() == "" {
			return deviate("url_template populated", "empty", "the autolink has no url_template")
		}
		return nil
	})

	rec.check("repos", "repos.listAutolinks", "GET /repos/{owner}/{repo}/autolinks", func() error {
		autolinks, _, err := client.Repositories.ListAutolinks(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if len(autolinks) != 1 {
			return deviate("the one autolink", fmt.Sprintf("%d", len(autolinks)), "the autolink listing is wrong")
		}
		return nil
	})

	if autolinkID != 0 {
		rec.check("repos", "repos.getAutolink / deleteAutolink",
			"GET and DELETE /repos/{owner}/{repo}/autolinks/{autolink_id}", func() error {
				autolink, _, err := client.Repositories.GetAutolink(ctx, sc.owner, sc.repo, autolinkID)
				if err != nil {
					return err
				}
				if autolink.GetID() != autolinkID {
					return deviate(fmt.Sprintf("%d", autolinkID), fmt.Sprintf("%d", autolink.GetID()),
						"the wrong autolink came back")
				}
				resp, err := client.Repositories.DeleteAutolink(ctx, sc.owner, sc.repo, autolinkID)
				if err != nil {
					return err
				}
				return wantStatus(resp, http.StatusNoContent, "deleting an autolink")
			})
	} else {
		rec.skip1("repos", "repos.getAutolink / deleteAutolink",
			"GET and DELETE /repos/{owner}/{repo}/autolinks/{autolink_id}", "the autolink fixture was not created")
	}

	// --- Stars, watchers and subscriptions ---------------------------------
	rec.check("activity", "activity.isStarred", "GET /user/starred/{owner}/{repo}", func() error {
		if _, err := client.Activity.Star(ctx, sc.owner, sc.repo); err != nil {
			return err
		}
		starred, _, err := client.Activity.IsStarred(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if !starred {
			return deviate("204 (starred)", "404", "starring a repository did not take effect")
		}
		if _, err := client.Activity.Unstar(ctx, sc.owner, sc.repo); err != nil {
			return err
		}
		starred, _, err = client.Activity.IsStarred(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if starred {
			return deviate("404 (not starred)", "204", "unstarring did not take effect")
		}
		return nil
	})

	rec.check("activity", "activity.listWatchers", "GET /repos/{owner}/{repo}/subscribers", func() error {
		_, _, err := client.Activity.ListWatchers(ctx, sc.owner, sc.repo, nil)
		return err
	})

	rec.check("activity", "activity.getRepositorySubscription",
		"GET /repos/{owner}/{repo}/subscription", func() error {
			subscription, _, err := client.Activity.SetRepositorySubscription(ctx, sc.owner, sc.repo,
				&github.Subscription{Subscribed: github.Ptr(true)})
			if err != nil {
				return err
			}
			if !subscription.GetSubscribed() {
				return deviate("subscribed true", "false", "subscribing did not take effect")
			}
			got, _, err := client.Activity.GetRepositorySubscription(ctx, sc.owner, sc.repo)
			if err != nil {
				return err
			}
			if !got.GetSubscribed() {
				return deviate("subscribed true", "false", "the subscription is not read back")
			}
			if got.GetRepositoryURL() == "" {
				return deviate("repository_url populated", "empty",
					"a subscription does not say which repository it is for")
			}
			return nil
		})

	rec.check("activity", "activity.deleteRepositorySubscription",
		"DELETE /repos/{owner}/{repo}/subscription", func() error {
			resp, err := client.Activity.DeleteRepositorySubscription(ctx, sc.owner, sc.repo)
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusNoContent, "deleting a repository subscription")
		})

	// --- Notifications -----------------------------------------------------
	rec.check("activity", "activity.listRepositoryNotifications",
		"GET /repos/{owner}/{repo}/notifications", func() error {
			_, _, err := client.Activity.ListRepositoryNotifications(ctx, sc.owner, sc.repo, nil)
			return err
		})

	rec.check("activity", "activity.markRepositoryNotificationsRead",
		"PUT /repos/{owner}/{repo}/notifications", func() error {
			_, err := client.Activity.MarkRepositoryNotificationsRead(ctx, sc.owner, sc.repo,
				github.Timestamp{Time: time.Now().UTC()})
			return err
		})

	rec.check("activity", "activity.listNotifications (all=true)", "GET /notifications?all=true", func() error {
		_, _, err := client.Activity.ListNotifications(ctx, &github.NotificationListOptions{All: true})
		return err
	})

	// --- Events ------------------------------------------------------------
	rec.check("activity", "activity.listRepositoryEvents", "GET /repos/{owner}/{repo}/events", func() error {
		events, _, err := client.Activity.ListRepositoryEvents(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.GetType() == "" || event.GetID() == "" {
				return deviate("type and id on every event", "an incomplete event",
					"an event is missing the fields clients key on")
			}
			if event.GetActor().GetLogin() == "" {
				return deviate("actor.login populated", "empty", "an event has no actor")
			}
		}
		return nil
	})

	rec.check("activity", "activity.listEventsPerformedByUser", "GET /users/{username}/events", func() error {
		_, _, err := client.Activity.ListEventsPerformedByUser(ctx, set.owner, false, nil)
		return err
	})

	// --- Forks -------------------------------------------------------------
	rec.check("repos", "repos.listForks", "GET /repos/{owner}/{repo}/forks", func() error {
		forks, _, err := client.Repositories.ListForks(ctx, sc.owner, sc.repo,
			&github.RepositoryListForksOptions{Sort: "newest"})
		if err != nil {
			return err
		}
		if forks == nil {
			return deviate("a repository array", "nil", "the fork listing did not decode")
		}
		return nil
	})

	// --- Pages -------------------------------------------------------------
	rec.check("pages", "repos.enablePages", "POST /repos/{owner}/{repo}/pages", func() error {
		pages, _, err := client.Repositories.EnablePages(ctx, sc.owner, sc.repo, &github.Pages{
			BuildType: github.Ptr("legacy"),
			Source:    &github.PagesSource{Branch: github.Ptr(sc.branch), Path: github.Ptr("/")},
		})
		if err != nil {
			return err
		}
		if pages.GetURL() == "" {
			return deviate("url populated", "empty", "the Pages site has no api url")
		}
		if pages.GetHTMLURL() == "" {
			return deviate("html_url populated", "empty",
				"the Pages site has no html_url, which is the address a client shows the user")
		}
		return nil
	})

	rec.check("pages", "repos.getPagesInfo", "GET /repos/{owner}/{repo}/pages", func() error {
		pages, _, err := client.Repositories.GetPagesInfo(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if pages.GetStatus() == "" && pages.Status != nil {
			return deviate("a status", "empty", "the Pages site reports no status")
		}
		if pages.GetSource().GetBranch() != sc.branch {
			return deviate(sc.branch, pages.GetSource().GetBranch(), "the Pages source branch does not round trip")
		}
		return nil
	})

	rec.check("pages", "repos.requestPageBuild / listPagesBuilds",
		"POST and GET /repos/{owner}/{repo}/pages/builds", func() error {
			build, _, err := client.Repositories.RequestPageBuild(ctx, sc.owner, sc.repo)
			if err != nil {
				return err
			}
			if build.GetStatus() == "" {
				return deviate("a build status", "empty", "the requested Pages build reports no status")
			}
			builds, _, err := client.Repositories.ListPagesBuilds(ctx, sc.owner, sc.repo, nil)
			if err != nil {
				return err
			}
			if len(builds) == 0 {
				return deviate("at least the build just requested", "none", "the Pages build listing is empty")
			}
			return nil
		})

	rec.check("pages", "repos.getLatestPagesBuild", "GET /repos/{owner}/{repo}/pages/builds/latest", func() error {
		build, _, err := client.Repositories.GetLatestPagesBuild(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if build.GetURL() == "" {
			return deviate("url populated", "empty", "the latest Pages build has no self link")
		}
		return nil
	})

	rec.check("pages", "repos.updatePages / disablePages",
		"PUT and DELETE /repos/{owner}/{repo}/pages", func() error {
			if _, err := client.Repositories.UpdatePagesGHES(ctx, sc.owner, sc.repo,
				&github.PagesUpdateWithoutCNAME{
					Source: &github.PagesSource{Branch: github.Ptr(sc.branch), Path: github.Ptr("/")},
				}); err != nil {
				return err
			}
			resp, err := client.Repositories.DisablePages(ctx, sc.owner, sc.repo)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusNoContent, "disabling Pages"); err != nil {
				return err
			}
			_, _, err = client.Repositories.GetPagesInfo(ctx, sc.owner, sc.repo)
			return wantHTTPError(err, http.StatusNotFound, "reading a disabled Pages site")
		})

	// --- Packages ----------------------------------------------------------
	rec.check("packages", "users.listPackages", "GET /user/packages?package_type=container", func() error {
		packages, _, err := client.Users.ListPackages(ctx, "", &github.PackageListOptions{
			PackageType: github.Ptr("container"),
		})
		if err != nil {
			return err
		}
		if packages == nil {
			return deviate("a package array", "nil", "the package listing did not decode")
		}
		return nil
	})

	rec.check("packages", "users.getPackage (404 for an absent package)",
		"GET /user/packages/{package_type}/{package_name}", func() error {
			_, _, err := client.Users.GetPackage(ctx, "", "container", "no-such-package")
			return wantHTTPError(err, http.StatusNotFound, "reading an absent package")
		})

	if set.org != "" {
		rec.check("packages", "orgs.listPackages", "GET /orgs/{org}/packages?package_type=container", func() error {
			packages, _, err := client.Organizations.ListPackages(ctx, set.org, &github.PackageListOptions{
				PackageType: github.Ptr("container"),
			})
			if err != nil {
				return err
			}
			if packages == nil {
				return deviate("a package array", "nil", "the organization package listing did not decode")
			}
			return nil
		})
	} else {
		rec.skip1("packages", "orgs.listPackages", "GET /orgs/{org}/packages", "the organization fixture is unavailable")
	}

	// --- Codespaces --------------------------------------------------------
	rec.check("codespaces", "codespaces.listInRepo", "GET /repos/{owner}/{repo}/codespaces", func() error {
		codespaces, _, err := client.Codespaces.ListInRepo(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if codespaces == nil {
			return deviate("a codespaces envelope", "nil", "the codespace listing did not decode")
		}
		return nil
	})

	rec.check("codespaces", "codespaces.list", "GET /user/codespaces", func() error {
		codespaces, _, err := client.Codespaces.List(ctx, nil)
		if err != nil {
			return err
		}
		if codespaces == nil {
			return deviate("a codespaces envelope", "nil", "the user codespace listing did not decode")
		}
		return nil
	})

	rec.check("codespaces", "codespaces.listRepositoryMachineTypes",
		"GET /repos/{owner}/{repo}/codespaces/machines", func() error {
			machines, _, err := client.Codespaces.ListRepositoryMachineTypes(ctx, sc.owner, sc.repo, nil)
			if err != nil {
				return err
			}
			if machines == nil {
				return deviate("a machines envelope", "nil", "the machine type listing did not decode")
			}
			return nil
		})

	rec.check("codespaces", "codespaces.listDevContainerConfigurations",
		"GET /repos/{owner}/{repo}/codespaces/devcontainers", func() error {
			configurations, _, err := client.Codespaces.ListDevContainerConfigurations(ctx, sc.owner, sc.repo, nil)
			if err != nil {
				return err
			}
			if configurations == nil {
				return deviate("a devcontainer envelope", "nil", "the devcontainer listing did not decode")
			}
			return nil
		})

	rec.check("codespaces", "codespaces.getUserPublicKey", "GET /user/codespaces/secrets/public-key", func() error {
		key, _, err := client.Codespaces.GetUserPublicKey(ctx)
		if err != nil {
			return err
		}
		if key.GetKeyID() == "" || key.GetKey() == "" {
			return deviate("key_id and key populated", "empty",
				"the codespace secret public key is unusable, so no client could seal a codespace secret")
		}
		return nil
	})

	rec.check("codespaces", "codespaces.listUserSecrets", "GET /user/codespaces/secrets", func() error {
		secrets, _, err := client.Codespaces.ListUserSecrets(ctx, nil)
		if err != nil {
			return err
		}
		if secrets == nil {
			return deviate("a secrets envelope", "nil", "the codespace secret listing did not decode")
		}
		return nil
	})

	// --- Repository dispatch ----------------------------------------------
	rec.check("repos", "repos.dispatch", "POST /repos/{owner}/{repo}/dispatches", func() error {
		payload := json.RawMessage(`{"reason":"conformance"}`)
		_, resp, err := client.Repositories.Dispatch(ctx, sc.owner, sc.repo, github.DispatchRequestOptions{
			EventType:     "conformance-event",
			ClientPayload: &payload,
		})
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "a repository dispatch")
	})
}

// runWiki asserts a repository's wiki is a real, clonable git repository —
// which is the only interface GitHub gives a client for wiki content.
func runWiki(client *github.Client, rec *recorder, set *fixtureSet) {
	rec.check("repos", "repos.hasWiki round trip", "PATCH /repos/{owner}/{repo} with has_wiki", func() error {
		if set.repo == "" {
			return deviate("a repository fixture", "none", "no repository fixture exists")
		}
		updated, _, err := client.Repositories.Edit(ctx, set.owner, set.repo, &github.Repository{
			HasWiki: github.Ptr(false),
		})
		if err != nil {
			return err
		}
		if updated.GetHasWiki() {
			return deviate("has_wiki false", "true", "disabling the wiki did not take effect")
		}
		updated, _, err = client.Repositories.Edit(ctx, set.owner, set.repo, &github.Repository{
			HasWiki: github.Ptr(true),
		})
		if err != nil {
			return err
		}
		if !updated.GetHasWiki() {
			return deviate("has_wiki true", "false", "re-enabling the wiki did not take effect")
		}
		if !strings.HasSuffix(updated.GetCloneURL(), ".git") {
			return deviate("a clone_url ending in .git", updated.GetCloneURL(),
				"the clone url has no .git suffix, so deriving the wiki remote from it would fail")
		}
		return nil
	})
}
