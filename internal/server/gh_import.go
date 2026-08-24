package bleephub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	git "github.com/go-git/go-git/v5"
	gitConfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitClient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	importHTTPScheme  = "bleephub-import-http"
	importHTTPSScheme = "bleephub-import-https"
)

// importHTTPTransport gives go-git a private protocol name while handing its
// HTTP implementation an ordinary http/https endpoint. Adding new protocol
// names is process-global, but unlike replacing go-git's "http" client it
// cannot change clones, pushes, Actions checkouts, or any other consumer.
type importHTTPTransport struct {
	scheme string
	base   transport.Transport
}

func (t importHTTPTransport) endpoint(ep *transport.Endpoint) *transport.Endpoint {
	copy := *ep
	copy.Protocol = t.scheme
	return &copy
}

func (t importHTTPTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	return t.base.NewUploadPackSession(t.endpoint(ep), auth)
}

func (t importHTTPTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return t.base.NewReceivePackSession(t.endpoint(ep), auth)
}

var installImportProtocolsOnce sync.Once

func init() {
	// Register only new, import-specific schemes before the server can accept
	// concurrent work. Calling InstallProtocol lazily would write go-git's
	// process-global protocol map while ordinary clones might be reading it.
	installImportProtocols()
}

func installImportProtocols() {
	installImportProtocolsOnce.Do(func() {
		install := func(protocol, scheme string) {
			httpClient := &http.Client{
				Timeout:   importFetchTimeout,
				Transport: otelhttp.NewTransport(newAddressCheckedHTTPTransport(false)),
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					if len(via) >= 10 {
						return errors.New("stopped after 10 redirects")
					}
					_, err := parseWebhookTargetURL(req.URL.String())
					return err
				},
			}
			gitClient.InstallProtocol(protocol, importHTTPTransport{
				scheme: scheme,
				base:   gitHTTP.NewClient(httpClient),
			})
		}
		install(importHTTPScheme, "http")
		install(importHTTPSScheme, "https")
	})
}

// importFetchURL selects an import-only go-git protocol. The URL still carries
// the original authority and path; the adapter above changes only the protocol
// seen by go-git's HTTP session after go-git has selected the isolated client.
func importFetchURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = importHTTPScheme
	case "https":
		u.Scheme = importHTTPSScheme
	default:
		return "", fmt.Errorf("unsupported import protocol %q", u.Scheme)
	}
	return u.String(), nil
}

// PorterLargeFile is a blob over the 100 MB large-file threshold found in
// the imported repository.
type PorterLargeFile struct {
	RefName string
	Path    string
	OID     string
	Size    int
}

const porterLargeFileThreshold = 100 * 1024 * 1024

const (
	// importFetchTimeout caps a single import fetch. Without it a remote that
	// accepts the connection and then stalls holds the worker forever.
	importFetchTimeout = 5 * time.Minute
	// importSyncWait is how long the request waits for the fetch before
	// answering "importing" and leaving it running. A local source finishes
	// well inside this; an unresponsive one no longer pins the request
	// goroutine to the remote's timeline.
	importSyncWait = 20 * time.Second
)

func (s *Server) registerGHImportRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/import", s.handleGetImport)
	s.route("PUT /api/v3/repos/{owner}/{repo}/import",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleStartImport))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/import",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateImport))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/import",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCancelImport))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/import/lfs",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleSetImportLFS))
	s.route("GET /api/v3/repos/{owner}/{repo}/import/authors", s.handleListImportAuthors)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/import/authors/{author_id}",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateImportAuthor))
	s.route("GET /api/v3/repos/{owner}/{repo}/import/large_files", s.handleListImportLargeFiles)
}

// --- Store ---

// --- Handlers ---

func (s *Server) importToJSON(imp *store.RepoImport, repo *store.Repo, baseURL string) map[string]interface{} {
	apiBase := baseURL + "/api/v3/repos/" + repo.FullName
	var vcs interface{}
	if imp.VCS != "" {
		vcs = imp.VCS
	}
	out := map[string]interface{}{
		"vcs":               vcs,
		"use_lfs":           imp.UseLFS,
		"vcs_url":           imp.VCSURL,
		"status":            imp.Status,
		"url":               apiBase + "/import",
		"html_url":          baseURL + "/" + repo.FullName + "/import",
		"authors_url":       apiBase + "/import/authors",
		"repository_url":    apiBase,
		"status_text":       nullableString(imp.StatusText),
		"failed_step":       nullableString(imp.FailedStep),
		"error_message":     nullableString(imp.ErrorMessage),
		"has_large_files":   imp.HasLargeFiles,
		"large_files_size":  imp.LargeFilesSize,
		"large_files_count": imp.LargeFilesCount,
	}
	if imp.TFVCProject != "" {
		out["tfvc_project"] = imp.TFVCProject
	}
	if imp.ImportPercent != nil {
		out["import_percent"] = *imp.ImportPercent
	} else {
		out["import_percent"] = nil
	}
	if imp.CommitCount != nil {
		out["commit_count"] = *imp.CommitCount
	} else {
		out["commit_count"] = nil
	}
	if imp.AuthorsCount != nil {
		out["authors_count"] = *imp.AuthorsCount
	} else {
		out["authors_count"] = nil
	}
	return out
}

func porterAuthorToJSON(a *store.PorterAuthor, repo *store.Repo, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"id":          a.ID,
		"remote_id":   a.RemoteID,
		"remote_name": a.RemoteName,
		"email":       a.Email,
		"name":        a.Name,
		"url":         baseURL + "/api/v3/repos/" + repo.FullName + "/import/authors/" + strconv.Itoa(a.ID),
		"import_url":  baseURL + "/api/v3/repos/" + repo.FullName + "/import",
	}
}

func (s *Server) handleGetImport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.importToJSON(imp, repo, s.baseURL(r)))
}

func (s *Server) handleStartImport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		VCSURL      string `json:"vcs_url"`
		VCS         string `json:"vcs"`
		VCSUsername string `json:"vcs_username"`
		VCSPassword string `json:"vcs_password"`
		TFVCProject string `json:"tfvc_project"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.VCSURL == "" {
		store.WriteGHValidationError(w, "Import", "vcs_url", "missing_field")
		return
	}
	if !s.acceptImportSource(w, req.VCSURL) {
		return
	}
	imp := &store.RepoImport{
		RepoID:       repo.ID,
		VCS:          req.VCS,
		VCSURL:       req.VCSURL,
		VCSUsername:  req.VCSUsername,
		VCSPassword:  req.VCSPassword,
		TFVCProject:  req.TFVCProject,
		NextAuthorID: 1,
		CreatedAt:    time.Now().UTC(),
	}
	writeJSON(w, http.StatusCreated, s.importToJSON(s.startRepoImport(imp, repo, ghUserFromContext(r.Context())), repo, s.baseURL(r)))
}

// acceptImportSource refuses a source URL the server must not fetch.
//
// The import dials whatever the caller names, so it gets the same address gate
// as webhook delivery rather than a second one of its own: two SSRF filters
// drift, and the one that drifts is the one nobody is looking at.
//
// The import-only go-git protocols repeat the policy at dial time, after DNS
// resolution. This early check remains useful because it rejects an obviously
// private target before a durable import record and worker are created.
func (s *Server) acceptImportSource(w http.ResponseWriter, rawURL string) bool {
	if err := validateWebhookTargetURL(rawURL); err != nil {
		store.WriteGHValidationError(w, "Import", "vcs_url", "invalid")
		return false
	}
	return true
}

func (s *Server) handleUpdateImport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		VCSUsername string `json:"vcs_username"`
		VCSPassword string `json:"vcs_password"`
		VCS         string `json:"vcs"`
		TFVCProject string `json:"tfvc_project"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	if req.VCSUsername != "" {
		imp.VCSUsername = req.VCSUsername
	}
	if req.VCSPassword != "" {
		imp.VCSPassword = req.VCSPassword
	}
	if req.VCS != "" {
		imp.VCS = req.VCS
	}
	if req.TFVCProject != "" {
		imp.TFVCProject = req.TFVCProject
	}
	if !s.acceptImportSource(w, imp.VCSURL) {
		return
	}
	// PATCH restarts a stalled import with the updated parameters.
	writeJSON(w, http.StatusOK, s.importToJSON(s.startRepoImport(imp, repo, ghUserFromContext(r.Context())), repo, s.baseURL(r)))
}

func (s *Server) handleCancelImport(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteRepoImport(repo.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetImportLFS(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		UseLFS string `json:"use_lfs"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.UseLFS {
	case "opt_in":
		imp.UseLFS = true
	case "opt_out":
		imp.UseLFS = false
	default:
		store.WriteGHValidationError(w, "Import", "use_lfs", "invalid")
		return
	}
	s.store.PutRepoImport(imp)
	writeJSON(w, http.StatusOK, s.importToJSON(imp, repo, s.baseURL(r)))
}

func (s *Server) handleListImportAuthors(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(imp.Authors))
	for _, a := range imp.Authors {
		out = append(out, porterAuthorToJSON(a, repo, base))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateImportAuthor(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("author_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	for _, a := range imp.Authors {
		if a.ID == id {
			if req.Email != "" {
				a.Email = req.Email
			}
			if req.Name != "" {
				a.Name = req.Name
			}
			s.store.PutRepoImport(imp)
			writeJSON(w, http.StatusOK, porterAuthorToJSON(a, repo, s.baseURL(r)))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleListImportLargeFiles(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	imp := s.store.GetRepoImport(repo.ID)
	if imp == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	files := importLargeFiles(s.store.GetGitStorage(owner, name), repo.DefaultBranch)
	out := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]interface{}{
			"ref_name": f.RefName,
			"path":     f.Path,
			"oid":      f.OID,
			"size":     f.Size,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Import execution ---

// errUnsupportedImportProtocol is what fetchGitSourceInto answers for a source
// URL whose scheme go-git's import-only transports do not speak. It is a
// distinct error because it is a fault in the request rather than in the
// remote: the caller failed detection, not the fetch.
var errUnsupportedImportProtocol = errors.New("unsupported import protocol")

// fetchGitSourceInto fetches every branch and tag of a remote repository into
// stor over the import-only go-git transports.
//
// It is the one place a source repository is really pulled in. The source
// import API calls it, and so does the GitHub Enterprise Importer repository
// migration — a migration and an import differ in what they record about the
// work, not in the work, and two copies of this would drift in exactly the
// dimension that matters: which transports and which address policy a source
// is dialled under.
func fetchGitSourceInto(ctx context.Context, stor gitStorage.Storer, rawURL string, auth transport.AuthMethod) error {
	installImportProtocols()
	fetchURL, err := importFetchURL(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %s", errUnsupportedImportProtocol, err)
	}
	remote := git.NewRemote(stor, &gitConfig.RemoteConfig{
		Name: "bleephub-import",
		URLs: []string{fetchURL},
		Fetch: []gitConfig.RefSpec{
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		},
	})
	err = remote.FetchContext(ctx, &git.FetchOptions{
		Auth:  auth,
		Force: true,
		Tags:  git.AllTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}

// startRepoImport runs the fetch on its own goroutine and returns the record to
// serve. The request waits importSyncWait for the outcome — long enough that a
// reachable source still answers "complete" on the same call — and otherwise
// answers "importing", leaving the fetch to finish and publish on its own.
//
// The goroutine works on a copy: the record the handler renders and the record
// the fetch mutates must not be the same struct.
func (s *Server) startRepoImport(imp *store.RepoImport, repo *store.Repo, sender *store.User) *store.RepoImport {
	pending := *imp
	pending.Status = "importing"
	s.store.PutRepoImport(&pending)

	running := *imp
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runRepoImport(&running, repo)
		if s.store.ReplaceRepoImportIfCurrent(&pending, &running) {
			s.emitRepositoryImportEvent(repo, &running, sender)
		}
	}()

	select {
	case <-done:
		return &running
	case <-time.After(importSyncWait):
		return &pending
	}
}

// emitRepositoryImportEvent delivers GitHub's `repository_import` webhook.
//
// The event has no activity types; its discriminator is the `status` field,
// which carries the outcome the import actually reached rather than a
// hard-coded success. A cancelled import never reaches here — DELETE removes
// the record, and ReplaceRepoImportIfCurrent then refuses to publish the
// fetch, which is the same decision that stops a cancelled import from
// resurrecting itself.
func (s *Server) emitRepositoryImportEvent(repo *store.Repo, imp *store.RepoImport, sender *store.User) {
	status := "failure"
	if imp.Status == "complete" {
		status = "success"
	}
	payload := map[string]interface{}{
		"status":     status,
		"repository": repoPayload(repo, s.publicOrigin()),
	}
	if sender != nil {
		payload["sender"] = store.UserToJSON(sender, s.publicOrigin())
	}
	s.emitWebhookEvent(repo.FullName, "repository_import", "", payload)
}

// runRepoImport performs the import and records the honest outcome on imp.
// Only git sources can really be imported; other VCS types end in status
// "error" saying so.
func (s *Server) runRepoImport(imp *store.RepoImport, repo *store.Repo) {
	imp.StatusText = ""
	imp.FailedStep = ""
	imp.ErrorMessage = ""

	if imp.VCS != "" && imp.VCS != "git" {
		imp.Status = "error"
		imp.FailedStep = "importing"
		imp.ErrorMessage = fmt.Sprintf("bleephub can only import git repositories; %q imports are not supported", imp.VCS)
		return
	}

	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		imp.Status = "error"
		imp.ErrorMessage = "invalid repository name"
		return
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		imp.Status = "error"
		imp.ErrorMessage = "repository git storage unavailable"
		return
	}

	var auth transport.AuthMethod
	if imp.VCSUsername != "" || imp.VCSPassword != "" {
		auth = &gitHTTP.BasicAuth{Username: imp.VCSUsername, Password: imp.VCSPassword}
	}

	ctx, cancel := context.WithTimeout(context.Background(), importFetchTimeout)
	defer cancel()
	err := fetchGitSourceInto(ctx, stor, imp.VCSURL, auth)
	if err != nil {
		if errors.Is(err, errUnsupportedImportProtocol) {
			imp.Status = "error"
			imp.FailedStep = "detecting"
			imp.ErrorMessage = err.Error()
			return
		}
		switch {
		case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
			imp.Status = "auth_failed"
			imp.FailedStep = "importing"
			imp.ErrorMessage = err.Error()
		case errors.Is(err, transport.ErrRepositoryNotFound), errors.Is(err, transport.ErrEmptyRemoteRepository):
			imp.Status = "detection_found_nothing"
			imp.FailedStep = "detecting"
			imp.ErrorMessage = err.Error()
		default:
			imp.Status = "error"
			imp.FailedStep = "importing"
			imp.ErrorMessage = err.Error()
		}
		return
	}

	imp.VCS = "git"
	pointHEADAtImportedBranch(stor, repo.DefaultBranch)

	commitCount, authors := importedCommitStats(stor, imp)
	imp.CommitCount = &commitCount
	authorsCount := len(authors)
	imp.AuthorsCount = &authorsCount
	imp.Authors = authors

	largeFiles := importLargeFiles(stor, repo.DefaultBranch)
	imp.HasLargeFiles = len(largeFiles) > 0
	imp.LargeFilesCount = len(largeFiles)
	total := 0
	for _, f := range largeFiles {
		total += f.Size
	}
	imp.LargeFilesSize = total

	hundred := 100
	imp.ImportPercent = &hundred
	imp.Status = "complete"

	s.store.UpdateRepo(owner, name, func(rp *store.Repo) {
		rp.PushedAt = time.Now().UTC()
	})
}

// pointHEADAtImportedBranch makes HEAD resolve to a fetched branch,
// preferring the repo's configured default branch, then main, then master,
// then the alphabetically first branch.
func pointHEADAtImportedBranch(stor gitStorage.Storer, defaultBranch string) {
	var names []string
	iter, err := stor.IterReferences()
	if err != nil {
		return
	}
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() && ref.Type() == plumbing.HashReference {
			names = append(names, ref.Name().Short())
		}
		return nil
	})
	iter.Close()
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	pick := ""
	for _, candidate := range []string{defaultBranch, "main", "master"} {
		for _, n := range names {
			if n == candidate {
				pick = n
				break
			}
		}
		if pick != "" {
			break
		}
	}
	if pick == "" {
		pick = names[0]
	}
	_ = store.SetGitHeadBranch(stor, pick)
}

// importedCommitStats counts commits and collects the distinct authors in
// the imported storage.
func importedCommitStats(stor gitStorage.Storer, imp *store.RepoImport) (int, []*store.PorterAuthor) {
	count := 0
	seen := map[string]*store.PorterAuthor{}
	// Preserve author IDs (and any name/email remapping) across restarts.
	for _, a := range imp.Authors {
		seen[a.RemoteID] = a
	}
	if imp.NextAuthorID == 0 {
		imp.NextAuthorID = 1
	}
	authors := append([]*store.PorterAuthor(nil), imp.Authors...)

	iter, err := stor.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		return 0, authors
	}
	defer iter.Close()
	_ = iter.ForEach(func(obj plumbing.EncodedObject) error {
		commit, err := object.DecodeCommit(stor, obj)
		if err != nil {
			return nil
		}
		count++
		remoteID := commit.Author.Name + " <" + commit.Author.Email + ">"
		if _, ok := seen[remoteID]; !ok {
			a := &store.PorterAuthor{
				ID:         imp.NextAuthorID,
				RemoteID:   remoteID,
				RemoteName: commit.Author.Name,
				Email:      commit.Author.Email,
				Name:       commit.Author.Name,
			}
			imp.NextAuthorID++
			seen[remoteID] = a
			authors = append(authors, a)
		}
		return nil
	})
	sort.Slice(authors, func(i, j int) bool { return authors[i].ID < authors[j].ID })
	return count, authors
}

// importLargeFiles finds blobs over the 100 MB threshold reachable from the
// default branch's tree, reported against their paths.
func importLargeFiles(stor gitStorage.Storer, defaultBranch string) []*PorterLargeFile {
	if stor == nil {
		return nil
	}
	sha := store.ResolveBranchSha(stor, defaultBranch)
	if sha == "" {
		return nil
	}
	commit, err := object.GetCommit(stor, plumbing.NewHash(sha))
	if err != nil {
		return nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil
	}
	var out []*PorterLargeFile
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		path, entry, err := walker.Next()
		if err != nil {
			break
		}
		if !entry.Mode.IsFile() {
			continue
		}
		blob, err := object.GetBlob(stor, entry.Hash)
		if err != nil {
			continue
		}
		if blob.Size >= porterLargeFileThreshold {
			out = append(out, &PorterLargeFile{
				RefName: "refs/heads/" + defaultBranch,
				Path:    path,
				OID:     entry.Hash.String(),
				Size:    int(blob.Size),
			})
		}
	}
	return out
}
