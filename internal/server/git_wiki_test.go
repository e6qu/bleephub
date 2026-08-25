package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

// wikiGitCLI is the real git binary with a commit identity, so a push carries an
// author the wiki's history can name.
func wikiGitCLI(t *testing.T) gitCLI {
	t.Helper()
	return requireGitCLI(t,
		"GIT_AUTHOR_NAME=Wiki Pusher", "GIT_AUTHOR_EMAIL=wiki@bleephub.invalid",
		"GIT_COMMITTER_NAME=Wiki Pusher", "GIT_COMMITTER_EMAIL=wiki@bleephub.invalid",
	)
}

// wikiRemote is the URL an unmodified client uses for a repository's wiki.
func wikiRemote(baseURL, credential, fullName string) string {
	if credential != "" {
		baseURL = strings.Replace(baseURL, "://", "://"+credential+"@", 1)
	}
	return baseURL + "/" + fullName + store.WikiStorageSuffix
}

// writeWikiWorktree seeds a local repository with the given files and returns
// its directory, ready to push at the wiki.
func writeWikiWorktree(t *testing.T, git gitCLI, dir, branch string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	git.run(dir, "init", "-q", "-b", branch)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git.run(dir, "add", name)
	}
	return dir
}

func readWikiPageJSON(t *testing.T, srv *isolatedServer, fullName, slug string) (map[string]interface{}, int) {
	t.Helper()
	resp := srv.get(t, "/ui-data/repos/"+fullName+"/wiki/pages/"+slug, defaultToken)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := map[string]interface{}{}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode wiki page: %v (%s)", err, body)
		}
	}
	return page, resp.StatusCode
}

// TestWikiOverGitHTTP drives the whole wiki lifecycle with the real git binary:
// an empty wiki clones, a first page pushes, the page clones back, an edit made
// through the browser lane appears in a fresh clone, an edit made by pushing
// appears in the browser lane, and a pushed deletion removes the page.
func TestWikiOverGitHTTP(t *testing.T) {
	t.Parallel()
	git := wikiGitCLI(t)
	srv := newIsolatedServer(t)
	repo := srv.seedRepo(t, "wiki-http", false)
	remote := wikiRemote(srv.baseURL, "admin:"+defaultToken, repo.FullName)
	temp := t.TempDir()

	// A wiki nobody has written yet is still a remote: it clones empty rather
	// than 404ing, which is what lets a client push the first page.
	git.run(temp, "clone", "-q", remote, "empty")
	if _, err := os.Stat(filepath.Join(temp, "empty", ".git")); err != nil {
		t.Fatalf("clone of an empty wiki produced no repository: %v", err)
	}

	work := writeWikiWorktree(t, git, filepath.Join(temp, "work"), repo.DefaultBranch, map[string]string{
		"Home.md": "# Welcome\n",
	})
	git.run(work, "commit", "-q", "-m", "add the wiki home page")
	git.run(work, "push", "-q", remote, repo.DefaultBranch)

	git.run(temp, "clone", "-q", remote, "back")
	cloned, err := os.ReadFile(filepath.Join(temp, "back", "Home.md"))
	if err != nil {
		t.Fatalf("cloned wiki has no Home.md: %v", err)
	}
	if string(cloned) != "# Welcome\n" {
		t.Fatalf("cloned Home.md = %q", cloned)
	}

	// The push is visible to the browser lane without anything having told it.
	page, status := readWikiPageJSON(t, srv, repo.FullName, "home")
	if status != http.StatusOK || page["title"] != "Home" || page["body"] != "# Welcome\n" {
		t.Fatalf("pushed page through the UI lane = %d %v", status, page)
	}

	// An edit made through the browser lane is a commit, so it is in the next
	// clone — no synchronization step, because there is nothing to synchronize.
	resp := srv.put(t, "/ui-data/repos/"+repo.FullName+"/wiki/pages/getting-started", defaultToken,
		map[string]string{"title": "Getting Started", "body": "read me first", "message": "seed"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("UI wiki write = %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	git.run(temp, "clone", "-q", remote, "after-ui-edit")
	seeded, err := os.ReadFile(filepath.Join(temp, "after-ui-edit", "Getting-Started.md"))
	if err != nil {
		t.Fatalf("UI edit is missing from a fresh clone: %v", err)
	}
	if string(seeded) != "read me first" {
		t.Fatalf("Getting-Started.md = %q", seeded)
	}

	// An edit made by pushing is visible to the browser lane, likewise.
	pull := filepath.Join(temp, "after-ui-edit")
	if err := os.WriteFile(filepath.Join(pull, "Home.md"), []byte("# Welcome back\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git.run(pull, "add", "Home.md")
	git.run(pull, "commit", "-q", "-m", "edit the home page")
	git.run(pull, "push", "-q", remote, "HEAD:"+repo.DefaultBranch)

	page, status = readWikiPageJSON(t, srv, repo.FullName, "home")
	if status != http.StatusOK || page["body"] != "# Welcome back\n" {
		t.Fatalf("pushed edit through the UI lane = %d %v", status, page)
	}

	// A pushed deletion removes the page from the browser lane too.
	git.run(pull, "rm", "-q", "Home.md")
	git.run(pull, "commit", "-q", "-m", "remove the home page")
	git.run(pull, "push", "-q", remote, "HEAD:"+repo.DefaultBranch)

	if _, status := readWikiPageJSON(t, srv, repo.FullName, "home"); status != http.StatusNotFound {
		t.Fatalf("deleted page still readable: status = %d", status)
	}
	if page, status := readWikiPageJSON(t, srv, repo.FullName, "getting-started"); status != http.StatusOK {
		t.Fatalf("unrelated page lost by the deletion: %d %v", status, page)
	}

	// The wiki is served by the repository's own upload-pack, so the protocol
	// surface that implementation carries is the wiki's too. A shallow clone at
	// each protocol version is the cheap proof that nothing about `.wiki.git`
	// resolution bypasses it.
	for _, version := range gitProtocolVersions {
		into := fmt.Sprintf("shallow-v%d", version)
		git.atProtocol(version).run(temp, "clone", "-q", "--depth=1", remote, into)
		if _, err := os.Stat(filepath.Join(temp, into, "Getting-Started.md")); err != nil {
			t.Fatalf("shallow protocol-v%d wiki clone is missing the page: %v", version, err)
		}
	}
}

// TestWikiOverGitSSH runs the same push-and-clone over the SSH transport, which
// shares the upload-pack and receive-pack implementations with smart HTTP.
func TestWikiOverGitSSH(t *testing.T) {
	// Not parallel: bringing up an SSH listener goes through the process
	// environment.
	srv := newIsolatedServer(t)
	keyPath := startIsolatedGitSSH(t, srv)
	git := wikiGitCLI(t).with("GIT_SSH_COMMAND=ssh -i " + keyPath + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null")
	repo := srv.seedRepo(t, "wiki-ssh", false)
	remote := "ssh://git@" + os.Getenv("BLEEPHUB_SSH_ADDR") + "/" + repo.FullName + store.WikiStorageSuffix
	temp := t.TempDir()

	work := writeWikiWorktree(t, git, filepath.Join(temp, "work"), repo.DefaultBranch, map[string]string{
		"Home.md": "over ssh\n",
	})
	git.run(work, "commit", "-q", "-m", "add the wiki home page")
	git.run(work, "push", "-q", remote, repo.DefaultBranch)

	git.run(temp, "clone", "-q", remote, "back")
	cloned, err := os.ReadFile(filepath.Join(temp, "back", "Home.md"))
	if err != nil {
		t.Fatalf("cloned wiki has no Home.md: %v", err)
	}
	if string(cloned) != "over ssh\n" {
		t.Fatalf("cloned Home.md = %q", cloned)
	}
	if page := srv.store.GetWikiPage(repo.FullName, "home"); page == nil || page.Body != "over ssh\n" {
		t.Fatalf("SSH push is not visible as a page: %+v", page)
	}
}

// TestWikiPrivateRepoIsNotClonableByAStranger holds the read half of the
// authorization matrix: a wiki is as private as the repository it belongs to.
func TestWikiPrivateRepoIsNotClonableByAStranger(t *testing.T) {
	t.Parallel()
	git := wikiGitCLI(t)
	srv := newIsolatedServer(t)
	repo := srv.seedRepo(t, "wiki-private", true)
	if page := srv.store.UpsertWikiPage(repo.FullName, "home", "Home", "classified", "admin", "seed"); page == nil {
		t.Fatal("could not seed the private wiki")
	}
	_, strangerToken := srv.newUser(t, "wiki-stranger")
	temp := t.TempDir()

	for name, credential := range map[string]string{
		"anonymous": "",
		"stranger":  "wiki-stranger:" + strangerToken,
	} {
		remote := wikiRemote(srv.baseURL, credential, repo.FullName)
		output, err := git.tryRun(temp, "clone", "-q", remote, "leak-"+name)
		if err == nil {
			t.Fatalf("%s cloned a private repository's wiki: %s", name, output)
		}
		if strings.Contains(output, "classified") {
			t.Fatalf("%s clone failure leaked wiki content: %s", name, output)
		}
	}

	// The owner still can, so the refusal above is authorization and not breakage.
	git.run(temp, "clone", "-q", wikiRemote(srv.baseURL, "admin:"+defaultToken, repo.FullName), "owner")
	if _, err := os.Stat(filepath.Join(temp, "owner", "Home.md")); err != nil {
		t.Fatalf("owner clone of the private wiki has no Home.md: %v", err)
	}
}

// TestWikiWritePolicy holds the write half of the matrix: a read-only
// collaborator is refused by default and admitted once the repository stops
// restricting wiki edits to collaborators.
func TestWikiWritePolicy(t *testing.T) {
	t.Parallel()
	git := wikiGitCLI(t)
	srv := newIsolatedServer(t)
	repo := srv.seedRepo(t, "wiki-policy", false)
	reader, readerToken := srv.newUser(t, "wiki-reader")
	srv.store.AddRepoCollaborator("admin", repo.Name, reader.Login, "pull")
	temp := t.TempDir()

	remote := wikiRemote(srv.baseURL, reader.Login+":"+readerToken, repo.FullName)
	work := writeWikiWorktree(t, git, filepath.Join(temp, "work"), repo.DefaultBranch, map[string]string{
		"Home.md": "from a reader\n",
	})
	git.run(work, "commit", "-q", "-m", "add the wiki home page")

	if output, err := git.tryRun(work, "push", "-q", remote, repo.DefaultBranch); err == nil {
		t.Fatalf("a pull-only collaborator pushed the wiki: %s", output)
	}
	if resp := srv.put(t, "/ui-data/repos/"+repo.FullName+"/wiki/pages/home", readerToken,
		map[string]string{"title": "Home", "body": "from a reader"}); resp.StatusCode != http.StatusForbidden {
		_ = resp.Body.Close()
		t.Fatalf("a pull-only collaborator edited the wiki in the UI: %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	// Unrestricting wiki edits admits both lanes, and only those two.
	settings := srv.put(t, "/ui-data/repos/"+repo.FullName+"/wiki/settings", defaultToken,
		map[string]bool{"restrict_edits_to_collaborators": false})
	if settings.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(settings.Body)
		_ = settings.Body.Close()
		t.Fatalf("relax the wiki policy = %d %s", settings.StatusCode, body)
	}
	_ = settings.Body.Close()

	git.run(work, "push", "-q", remote, repo.DefaultBranch)
	if page := srv.store.GetWikiPage(repo.FullName, "home"); page == nil || page.Body != "from a reader\n" {
		t.Fatalf("unrestricted wiki push did not land: %+v", page)
	}

	// A repository administrator is the only one who may move the policy.
	refused := srv.put(t, "/ui-data/repos/"+repo.FullName+"/wiki/settings", readerToken,
		map[string]bool{"restrict_edits_to_collaborators": true})
	if refused.StatusCode != http.StatusForbidden {
		_ = refused.Body.Close()
		t.Fatalf("a reader changed the wiki policy: %d", refused.StatusCode)
	}
	_ = refused.Body.Close()
}

// TestWikiDisabledHasNoRemote holds has_wiki as the gate it is meant to be.
func TestWikiDisabledHasNoRemote(t *testing.T) {
	t.Parallel()
	git := wikiGitCLI(t)
	srv := newIsolatedServer(t)
	repo := srv.seedRepo(t, "wiki-disabled", false)
	srv.store.UpdateRepo("admin", repo.Name, func(updated *store.Repo) { updated.HasWiki = false })
	temp := t.TempDir()

	remote := wikiRemote(srv.baseURL, "admin:"+defaultToken, repo.FullName)
	if output, err := git.tryRun(temp, "clone", "-q", remote, "disabled"); err == nil {
		t.Fatalf("a disabled wiki is still clonable: %s", output)
	}
	work := writeWikiWorktree(t, git, filepath.Join(temp, "work"), repo.DefaultBranch, map[string]string{
		"Home.md": "nope\n",
	})
	git.run(work, "commit", "-q", "-m", "add the wiki home page")
	if output, err := git.tryRun(work, "push", "-q", remote, repo.DefaultBranch); err == nil {
		t.Fatalf("a disabled wiki is still pushable: %s", output)
	}

	// Turning it back on gives the repository its wiki remote.
	srv.store.UpdateRepo("admin", repo.Name, func(updated *store.Repo) { updated.HasWiki = true })
	git.run(work, "push", "-q", remote, repo.DefaultBranch)
}

// TestWikiPushEmitsGollum holds the event contract: a wiki push delivers
// gollum, with github's payload, and no push event.
func TestWikiPushEmitsGollum(t *testing.T) {
	t.Parallel()
	git := wikiGitCLI(t)
	srv := newIsolatedServer(t)
	repo := srv.seedRepo(t, "wiki-gollum", false)

	var mu sync.Mutex
	var gollum map[string]interface{}
	pushes := 0
	url, cleanup := startWebhookReceiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch r.Header.Get("X-GitHub-Event") {
		case "gollum":
			if gollum == nil {
				gollum = webhookEventJSON(t, r.Header.Get("Content-Type"), body)
			}
		case "push":
			pushes++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	hook := srv.post(t, "/api/v3/repos/"+repo.FullName+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": url},
		"events": []string{"gollum", "push"},
		"active": true,
	})
	if hook.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(hook.Body)
		_ = hook.Body.Close()
		t.Fatalf("create hook = %d %s", hook.StatusCode, body)
	}
	_ = hook.Body.Close()

	temp := t.TempDir()
	remote := wikiRemote(srv.baseURL, "admin:"+defaultToken, repo.FullName)
	work := writeWikiWorktree(t, git, filepath.Join(temp, "work"), repo.DefaultBranch, map[string]string{
		"Getting-Started.md": "hello\n",
	})
	git.run(work, "commit", "-q", "-m", "add a page")
	git.run(work, "push", "-q", remote, repo.DefaultBranch)

	if !testutil.TestEventually(5*time.Second, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gollum != nil
	}) {
		t.Fatal("a wiki push delivered no gollum event")
	}
	mu.Lock()
	payload := gollum
	pushCount := pushes
	mu.Unlock()

	if pushCount != 0 {
		t.Fatalf("a wiki push delivered %d push events; a wiki push is not a repository push", pushCount)
	}
	pages, _ := payload["pages"].([]interface{})
	if len(pages) != 1 {
		t.Fatalf("gollum pages = %v", payload["pages"])
	}
	page, _ := pages[0].(map[string]interface{})
	if page["page_name"] != "Getting-Started" || page["title"] != "Getting Started" || page["action"] != "created" {
		t.Fatalf("gollum page = %v", page)
	}
	if summary, present := page["summary"]; !present || summary != nil {
		t.Fatalf("gollum summary = %v (present=%v)", summary, present)
	}
	sha, _ := page["sha"].(string)
	if len(sha) != 40 {
		t.Fatalf("gollum sha = %q", sha)
	}
	htmlURL, _ := page["html_url"].(string)
	if !strings.HasSuffix(htmlURL, "/"+repo.FullName+"/wiki/Getting-Started") {
		t.Fatalf("gollum html_url = %q", htmlURL)
	}
	if _, ok := payload["repository"].(map[string]interface{}); !ok {
		t.Fatalf("gollum payload carries no repository: %v", payload)
	}
	if _, ok := payload["sender"].(map[string]interface{}); !ok {
		t.Fatalf("gollum payload carries no sender: %v", payload)
	}
}

// TestWikiTitleFilenameMapping pins the title↔filename mapping both lanes
// depend on: it is what makes a page's identity the same fact in git and in the
// browser, so the two cannot name different pages.
func TestWikiTitleFilenameMapping(t *testing.T) {
	t.Parallel()
	for title, file := range map[string]string{
		"Home":            "Home.md",
		"Getting Started": "Getting-Started.md",
		"API/Reference":   "API-Reference.md",
		"  Spaced  ":      "Spaced.md",
	} {
		if got := store.WikiPageFileName(title); got != file {
			t.Errorf("WikiPageFileName(%q) = %q, want %q", title, got, file)
		}
	}
	for file, title := range map[string]string{
		"Home.md":            "Home",
		"Getting-Started.md": "Getting Started",
		"Notes.textile":      "Notes",
		"Design.asciidoc":    "Design",
	} {
		got, ok := store.WikiTitleFromPath(file)
		if !ok || got != title {
			t.Errorf("WikiTitleFromPath(%q) = %q,%v; want %q,true", file, got, ok, title)
		}
	}
	for _, file := range []string{"logo.png", "styles.css", "docs/Nested.md", ""} {
		if got, ok := store.WikiTitleFromPath(file); ok {
			t.Errorf("WikiTitleFromPath(%q) = %q,true; want a non-page", file, got)
		}
	}
	// Round trip: the slug the browser addresses a page by is a function of the
	// file the wiki repository holds, so a push and an edit agree on it.
	for _, title := range []string{"Home", "Getting Started", "Release Notes"} {
		file := store.WikiPageFileName(title)
		back, ok := store.WikiTitleFromPath(file)
		if !ok || store.WikiSlug(back) != store.WikiSlug(title) {
			t.Errorf("%q → %q → %q does not round trip", title, file, back)
		}
	}
	if fmt.Sprint(store.WikiStorageName("admin/docs")) != "admin/docs.wiki.git" {
		t.Errorf("WikiStorageName = %q", store.WikiStorageName("admin/docs"))
	}
}
