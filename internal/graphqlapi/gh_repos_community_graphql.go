package graphqlapi

// Repository members backed by files in git: community-health documents
// (CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, FUNDING, issue/PR templates),
// CODEOWNERS, and .gitmodules submodules.
//
// Lookup follows GitHub's search order (.github/, root, docs/) on the default
// branch — the same files the REST community-profile and CODEOWNERS endpoints read.

import (
	"context"
	"encoding/base64"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/graphql-go/graphql"
	"gopkg.in/yaml.v3"

	"github.com/e6qu/bleephub/internal/store"
)

// communityFileSearchLocations are the directories GitHub scans for a
// community-health file, in precedence order.
var communityFileSearchLocations = []string{".github", "", "docs"}

var (
	securityPolicyPaths  = []string{"SECURITY.md", "SECURITY"}
	contributingPaths    = []string{"CONTRIBUTING.md", "CONTRIBUTING"}
	codeOfConductPaths   = []string{"CODE_OF_CONDUCT.md", "CODE_OF_CONDUCT"}
	pullRequestTemplates = []string{"PULL_REQUEST_TEMPLATE.md", "PULL_REQUEST_TEMPLATE", "pull_request_template.md"}
	// codeownersSearchPaths in precedence order; the first found is operative.
	codeownersSearchPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}
	fundingFilePaths      = []string{".github/FUNDING.yml", ".github/FUNDING.yaml", "FUNDING.yml", "FUNDING.yaml"}
	issueTemplateConfigs  = []string{".github/ISSUE_TEMPLATE/config.yml", ".github/ISSUE_TEMPLATE/config.yaml"}
	issueTemplateDirs     = []string{".github/ISSUE_TEMPLATE", "ISSUE_TEMPLATE", "docs/ISSUE_TEMPLATE"}
	prTemplateDirs        = []string{".github/PULL_REQUEST_TEMPLATE", "PULL_REQUEST_TEMPLATE", "docs/PULL_REQUEST_TEMPLATE"}
)

// readableRepoFromSource re-reads the named repository, answering nil when the
// request may not read it. Every git-content field goes through it so a private
// repository's files cannot be read by a stranger who reached the source another way.
func (s *Resolver) readableRepoFromSource(ctx context.Context, source interface{}) (*store.Repo, error) {
	repo, err := s.repoFromSource(source)
	if err != nil {
		return nil, err
	}
	if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
		return nil, nil
	}
	return repo, nil
}

// repositoryDefaultTree opens the repository's default-branch tree.
func (s *Resolver) repositoryDefaultTree(repo *store.Repo) (gitStorage.Storer, *object.Tree, bool) {
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil, nil, false
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil, nil, false
	}
	tree, ok := repoBranchTree(stor, repo.DefaultBranch)
	if !ok {
		return nil, nil, false
	}
	return stor, tree, true
}

// repoBranchTree resolves a branch's commit tree in a repository's git
// storage.
func repoBranchTree(stor gitStorage.Storer, branch string) (*object.Tree, bool) {
	if branch == "" {
		return nil, false
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, false
	}
	commit, err := object.GetCommit(stor, ref.Hash())
	if err != nil {
		return nil, false
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, false
	}
	return tree, true
}

// repositoryHealthFile finds the first of candidates present in one of
// GitHub's community-file locations, returning the path it was found at and
// its content.
func (s *Resolver) repositoryHealthFile(repo *store.Repo, candidates []string) (string, []byte, bool) {
	stor, tree, ok := s.repositoryDefaultTree(repo)
	if !ok {
		return "", nil, false
	}
	for _, dir := range communityFileSearchLocations {
		for _, candidate := range candidates {
			path := candidate
			if dir != "" {
				path = dir + "/" + candidate
			}
			content, ok := readTreeFile(stor, tree, path)
			if ok {
				return path, content, true
			}
		}
	}
	return "", nil, false
}

// repositoryFileAtPaths reads the first of the exact paths that exists.
func (s *Resolver) repositoryFileAtPaths(repo *store.Repo, paths []string) (string, []byte, bool) {
	stor, tree, ok := s.repositoryDefaultTree(repo)
	if !ok {
		return "", nil, false
	}
	for _, path := range paths {
		content, found := readTreeFile(stor, tree, path)
		if found {
			return path, content, true
		}
	}
	return "", nil, false
}

// readTreeFile reads one blob out of a tree by path.
func readTreeFile(stor gitStorage.Storer, tree *object.Tree, path string) ([]byte, bool) {
	entry, err := tree.FindEntry(path)
	if err != nil || !entry.Mode.IsFile() {
		return nil, false
	}
	content, err := store.ReadGitBlob(stor, entry.Hash)
	if err != nil {
		return nil, false
	}
	return content, true
}

// repositoryTemplateFiles lists the markdown template files in the first of
// dirs that exists, sorted by filename.
func (s *Resolver) repositoryTemplateFiles(repo *store.Repo, dirs []string) []templateFile {
	stor, tree, ok := s.repositoryDefaultTree(repo)
	if !ok {
		return nil
	}
	for _, dir := range dirs {
		sub, err := tree.Tree(dir)
		if err != nil {
			continue
		}
		out := make([]templateFile, 0, len(sub.Entries))
		for _, entry := range sub.Entries {
			if !entry.Mode.IsFile() || !isMarkdownTemplateName(entry.Name) {
				continue
			}
			content, err := store.ReadGitBlob(stor, entry.Hash)
			if err != nil {
				continue
			}
			out = append(out, templateFile{name: entry.Name, content: string(content)})
		}
		if len(out) == 0 {
			continue
		}
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out
	}
	return nil
}

type templateFile struct {
	name    string
	content string
}

func isMarkdownTemplateName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown")
}

// --- schema ----------------------------------------------------------------

// addRepositoryCommunityFields installs the git-content-backed members of
// Repository.
func (s *Resolver) addRepositoryCommunityFields(types *accountSurfaceTypes) {
	repoType := types.repository
	uri := s.graphQLStringScalar("URI")

	// --- CONTRIBUTING ------------------------------------------------------
	contributing := graphql.NewObject(graphql.ObjectConfig{
		Name: "ContributingGuidelines",
		Fields: graphql.Fields{
			"body":         &graphql.Field{Type: graphql.String},
			"resourcePath": &graphql.Field{Type: uri},
			"url":          &graphql.Field{Type: uri},
		},
	})
	repoType.AddFieldConfig("contributingGuidelines", &graphql.Field{
		Type: contributing,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			path, content, found := s.repositoryHealthFile(repo, contributingPaths)
			if !found {
				return nil, nil
			}
			blobPath := "/" + repo.FullName + "/blob/" + repo.DefaultBranch + "/" + path
			return map[string]interface{}{
				"body":         string(content),
				"resourcePath": blobPath,
				"url":          externalURL(blobPath),
			}, nil
		},
	})

	// --- CODE_OF_CONDUCT ---------------------------------------------------
	repoType.AddFieldConfig("codeOfConduct", &graphql.Field{
		Type: s.gqlCodeOfConductType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			path, content, found := s.repositoryHealthFile(repo, codeOfConductPaths)
			if !found {
				return nil, nil
			}
			blobPath := "/" + repo.FullName + "/blob/" + repo.DefaultBranch + "/" + path
			entry := detectCodeOfConduct(string(content))
			return map[string]interface{}{
				"body":         string(content),
				"id":           "COC_" + entry.Key,
				"key":          entry.Key,
				"name":         entry.Name,
				"resourcePath": blobPath,
				"url":          externalURL(blobPath),
			}, nil
		},
	})

	// --- FUNDING.yml -------------------------------------------------------
	fundingLink := graphql.NewObject(graphql.ObjectConfig{
		Name: "FundingLink",
		Fields: graphql.Fields{
			"platform": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("FundingPlatform",
				"BUY_ME_A_COFFEE", "COMMUNITY_BRIDGE", "CUSTOM", "GITHUB", "ISSUEHUNT", "KO_FI",
				"LFX_CROWDFUNDING", "LIBERAPAY", "OPEN_COLLECTIVE", "PATREON", "POLAR",
				"THANKS_DEV", "TIDELIFT"))},
			"url": &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	repoType.AddFieldConfig("fundingLinks", &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(fundingLink))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return []interface{}{}, err
			}
			links := s.repositoryFundingLinks(repo)
			out := make([]interface{}, 0, len(links))
			for _, link := range links {
				out = append(out, map[string]interface{}{"platform": link.platform, "url": link.url})
			}
			return out, nil
		},
	})

	// --- issue-template config contact links -------------------------------
	contactLink := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryContactLink",
		Fields: graphql.Fields{
			"about": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"url":   &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	repoType.AddFieldConfig("contactLinks", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(contactLink)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			config, found := s.repositoryIssueTemplateConfig(repo)
			if !found {
				return nil, nil
			}
			out := make([]interface{}, 0, len(config.ContactLinks))
			for _, link := range config.ContactLinks {
				if link.Name == "" || link.URL == "" {
					continue
				}
				out = append(out, map[string]interface{}{
					"about": link.About, "name": link.Name, "url": link.URL,
				})
			}
			return out, nil
		},
	})

	// --- issue templates ---------------------------------------------------
	issueTemplate := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueTemplate",
		Fields: graphql.Fields{
			"about":    &graphql.Field{Type: graphql.String},
			"body":     &graphql.Field{Type: graphql.String},
			"filename": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"title":    &graphql.Field{Type: graphql.String},
			// Resolves null: bleephub's templates record no issue-type binding.
			"type": &graphql.Field{Type: s.graphqlTypes.issueType},
			"assignees": &graphql.Field{
				Type: graphql.NewNonNull(types.userConnection),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					logins, _ := src["_assignees"].([]string)
					users := make([]*store.User, 0, len(logins))
					for _, login := range logins {
						if u := s.store.LookupUserByLogin(login); u != nil {
							users = append(users, u)
						}
					}
					return paginateGQLItems(userConnectionItems(users), p.Args), nil
				},
			},
			"labels": &graphql.Field{
				Type: s.gqlLabelConnectionType(),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					names, _ := src["_labels"].([]string)
					repoID, _ := src["_repoID"].(int)
					items := make([]gqlConnItem, 0, len(names))
					for _, name := range names {
						label := s.store.GetLabelByName(repoID, name)
						if label == nil {
							continue
						}
						row := label
						items = append(items, gqlConnItem{
							identity: row.NodeID,
							render:   func() map[string]interface{} { return labelToGQL(row) },
						})
					}
					return paginateGQLItems(items, p.Args), nil
				},
			},
		},
	})
	repoType.AddFieldConfig("issueTemplates", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(issueTemplate)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			files := s.repositoryTemplateFiles(repo, issueTemplateDirs)
			if len(files) == 0 {
				// A single legacy ISSUE_TEMPLATE.md is one template.
				path, content, found := s.repositoryHealthFile(repo, []string{"ISSUE_TEMPLATE.md", "ISSUE_TEMPLATE"})
				if !found {
					return nil, nil
				}
				files = []templateFile{{name: path[strings.LastIndex(path, "/")+1:], content: string(content)}}
			}
			out := make([]interface{}, 0, len(files))
			for _, file := range files {
				meta := parseTemplateFrontMatter(file.name, file.content)
				out = append(out, map[string]interface{}{
					"about":      nilStr(meta.About),
					"body":       meta.Body,
					"filename":   file.name,
					"name":       meta.Name,
					"title":      nilStr(meta.Title),
					"_assignees": meta.Assignees,
					"_labels":    meta.Labels,
					"_repoID":    repo.ID,
				})
			}
			return out, nil
		},
	})

	// --- pull-request templates -------------------------------------------
	pullRequestTemplate := graphql.NewObject(graphql.ObjectConfig{
		Name: "PullRequestTemplate",
		Fields: graphql.Fields{
			"body":     &graphql.Field{Type: graphql.String},
			"filename": &graphql.Field{Type: graphql.String},
			"repository": &graphql.Field{
				Type: graphql.NewNonNull(repoType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					return src["_repository"], nil
				},
			},
		},
	})
	repoType.AddFieldConfig("pullRequestTemplates", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(pullRequestTemplate)),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			files := s.repositoryTemplateFiles(repo, prTemplateDirs)
			if len(files) == 0 {
				path, content, found := s.repositoryHealthFile(repo, pullRequestTemplates)
				if !found {
					return nil, nil
				}
				files = []templateFile{{name: path[strings.LastIndex(path, "/")+1:], content: string(content)}}
			}
			out := make([]interface{}, 0, len(files))
			for _, file := range files {
				out = append(out, map[string]interface{}{
					"body":        file.content,
					"filename":    file.name,
					"_repository": repoSource,
				})
			}
			return out, nil
		},
	})

	// --- CODEOWNERS --------------------------------------------------------
	codeownersError := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryCodeownersError",
		Fields: graphql.Fields{
			"column":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"kind":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"line":       &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"message":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"path":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"source":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"suggestion": &graphql.Field{Type: graphql.String},
		},
	})
	codeowners := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryCodeowners",
		Fields: graphql.Fields{
			"errors": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(codeownersError)))},
		},
	})
	repoType.AddFieldConfig("codeowners", &graphql.Field{
		Type: codeowners,
		Args: graphql.FieldConfigArgument{
			"refName": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			path, content, found := s.repositoryCodeownersFile(repo, p.Args)
			if !found {
				return nil, nil
			}
			return map[string]interface{}{"errors": s.validateRepositoryCodeowners(string(content), path)}, nil
		},
	})

	// --- .gitmodules -------------------------------------------------------
	base64String := s.graphQLStringScalar("Base64String")
	submodule := graphql.NewObject(graphql.ObjectConfig{
		Name: "Submodule",
		Fields: graphql.Fields{
			"branch":              &graphql.Field{Type: graphql.String},
			"gitUrl":              &graphql.Field{Type: graphql.NewNonNull(uri)},
			"name":                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nameRaw":             &graphql.Field{Type: graphql.NewNonNull(base64String)},
			"path":                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"pathRaw":             &graphql.Field{Type: graphql.NewNonNull(base64String)},
			"subprojectCommitOid": &graphql.Field{Type: s.graphQLStringScalar("GitObjectID")},
		},
	})
	repoType.AddFieldConfig("submodules", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "Submodule", submodule, false, nil)),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.readableRepoFromSource(p.Context, p.Source)
			if err != nil || repo == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			return paginateGQLItems(s.repositorySubmoduleItems(repo), p.Args), nil
		},
	})
}

// gqlCodeOfConductType returns the CodeOfConduct object type (memoized).
func (s *Resolver) gqlCodeOfConductType() *graphql.Object {
	types := s.accountSurfaceRegistry()
	if types.codeOfConduct != nil {
		return types.codeOfConduct
	}
	uri := s.graphQLStringScalar("URI")
	types.codeOfConduct = graphql.NewObject(graphql.ObjectConfig{
		Name: "CodeOfConduct",
		Fields: graphql.Fields{
			"body":         &graphql.Field{Type: graphql.String},
			"id":           &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"key":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"resourcePath": &graphql.Field{Type: uri},
			"url":          &graphql.Field{Type: uri},
		},
	})
	return types.codeOfConduct
}

// detectCodeOfConduct identifies the catalog document by the title it carries;
// one outside the catalog is GitHub's "other".
func detectCodeOfConduct(content string) store.CodeOfConduct {
	for _, entry := range store.CodesOfConductCatalog {
		if strings.Contains(content, entry.Name) {
			return entry
		}
	}
	return store.CodeOfConduct{Key: "other", Name: "Other"}
}

// --- FUNDING.yml -----------------------------------------------------------

type fundingLink struct {
	platform string
	url      string
}

// fundingPlatforms maps FUNDING.yml keys onto the FundingPlatform enum and the
// URL prefix each key's handle expands to.
var fundingPlatforms = []struct {
	key      string
	platform string
	prefix   string
}{
	{"buy_me_a_coffee", "BUY_ME_A_COFFEE", "https://www.buymeacoffee.com/"},
	{"community_bridge", "COMMUNITY_BRIDGE", "https://funding.communitybridge.org/projects/"},
	{"github", "GITHUB", "https://github.com/sponsors/"},
	{"issuehunt", "ISSUEHUNT", "https://issuehunt.io/r/"},
	{"ko_fi", "KO_FI", "https://ko-fi.com/"},
	{"lfx_crowdfunding", "LFX_CROWDFUNDING", "https://crowdfunding.lfx.linuxfoundation.org/projects/"},
	{"liberapay", "LIBERAPAY", "https://liberapay.com/"},
	{"open_collective", "OPEN_COLLECTIVE", "https://opencollective.com/"},
	{"patreon", "PATREON", "https://www.patreon.com/"},
	{"polar", "POLAR", "https://polar.sh/"},
	{"thanks_dev", "THANKS_DEV", "https://thanks.dev/"},
	{"tidelift", "TIDELIFT", "https://tidelift.com/funding/github/"},
}

// repositoryFundingLinks parses the FUNDING file into platform/url pairs.
func (s *Resolver) repositoryFundingLinks(repo *store.Repo) []fundingLink {
	_, content, found := s.repositoryFileAtPaths(repo, fundingFilePaths)
	if !found {
		return nil
	}
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		return nil
	}
	var out []fundingLink
	for _, platform := range fundingPlatforms {
		for _, handle := range fundingYAMLValues(parsed[platform.key]) {
			out = append(out, fundingLink{platform: platform.platform, url: platform.prefix + handle})
		}
	}
	// `custom` carries whole URLs rather than handles.
	for _, url := range fundingYAMLValues(parsed["custom"]) {
		out = append(out, fundingLink{platform: "CUSTOM", url: url})
	}
	return out
}

// fundingYAMLValues normalizes a FUNDING.yml value (a single handle or a list).
func fundingYAMLValues(value interface{}) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, element := range typed {
			if text, ok := element.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	}
	return nil
}

// --- issue-template config -------------------------------------------------

type issueTemplateConfig struct {
	BlankIssuesEnabled *bool `yaml:"blank_issues_enabled"`
	ContactLinks       []struct {
		Name  string `yaml:"name"`
		URL   string `yaml:"url"`
		About string `yaml:"about"`
	} `yaml:"contact_links"`
}

// repositoryIssueTemplateConfig parses .github/ISSUE_TEMPLATE/config.yml.
func (s *Resolver) repositoryIssueTemplateConfig(repo *store.Repo) (issueTemplateConfig, bool) {
	var config issueTemplateConfig
	_, content, found := s.repositoryFileAtPaths(repo, issueTemplateConfigs)
	if !found {
		return config, false
	}
	if err := yaml.Unmarshal(content, &config); err != nil {
		return issueTemplateConfig{}, false
	}
	return config, true
}

// repositoryAllowsBlankIssues reports whether the "open a blank issue" escape
// hatch is offered; on unless the issue-template config turns it off.
func (s *Resolver) repositoryAllowsBlankIssues(repo *store.Repo) bool {
	config, found := s.repositoryIssueTemplateConfig(repo)
	if !found || config.BlankIssuesEnabled == nil {
		return true
	}
	return *config.BlankIssuesEnabled
}

// --- template front matter -------------------------------------------------

type templateMetadata struct {
	Name      string
	About     string
	Title     string
	Labels    []string
	Assignees []string
	Body      string
}

type templateFrontMatter struct {
	Name      string      `yaml:"name"`
	About     string      `yaml:"about"`
	Title     string      `yaml:"title"`
	Labels    interface{} `yaml:"labels"`
	Assignees interface{} `yaml:"assignees"`
}

// parseTemplateFrontMatter splits a template's YAML front matter from its body.
func parseTemplateFrontMatter(filename, content string) templateMetadata {
	meta := templateMetadata{
		Name: strings.TrimSuffix(strings.TrimSuffix(filename, ".markdown"), ".md"),
		Body: content,
	}
	rest, front, ok := splitFrontMatter(content)
	if !ok {
		return meta
	}
	var parsed templateFrontMatter
	if err := yaml.Unmarshal([]byte(front), &parsed); err != nil {
		return meta
	}
	meta.Body = rest
	if parsed.Name != "" {
		meta.Name = parsed.Name
	}
	meta.About = parsed.About
	meta.Title = parsed.Title
	meta.Labels = fundingYAMLValues(parsed.Labels)
	meta.Assignees = fundingYAMLValues(parsed.Assignees)
	if list, ok := parsed.Labels.(string); ok {
		meta.Labels = splitCommaList(list)
	}
	if list, ok := parsed.Assignees.(string); ok {
		meta.Assignees = splitCommaList(list)
	}
	return meta
}

// splitFrontMatter returns the body and the YAML front matter of a document
// that opens with a `---` fence.
func splitFrontMatter(content string) (body, front string, ok bool) {
	trimmed := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return content, "", false
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimPrefix(rest, "\r")
	if !strings.HasPrefix(rest, "\n") {
		return content, "", false
	}
	rest = rest[1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return content, "", false
	}
	front = rest[:end]
	after := rest[end+len("\n---"):]
	after = strings.TrimPrefix(after, "\r")
	after = strings.TrimPrefix(after, "\n")
	return after, front, true
}

// splitCommaList splits GitHub's comma-separated front-matter list form.
func splitCommaList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// --- CODEOWNERS ------------------------------------------------------------

// repositoryCodeownersFile finds the operative CODEOWNERS file, honoring refName.
func (s *Resolver) repositoryCodeownersFile(repo *store.Repo, args map[string]interface{}) (string, []byte, bool) {
	refName, _ := args["refName"].(string)
	if refName == "" {
		return s.repositoryFileAtPaths(repo, codeownersSearchPaths)
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return "", nil, false
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return "", nil, false
	}
	tree, ok := repoBranchTree(stor, refName)
	if !ok {
		return "", nil, false
	}
	for _, path := range codeownersSearchPaths {
		if content, found := readTreeFile(stor, tree, path); found {
			return path, content, true
		}
	}
	return "", nil, false
}

// validateRepositoryCodeowners reports "Invalid owner" (malformed reference) and
// "Unknown owner" (no such account/team), the same classification the REST
// codeowners-errors endpoint serves.
func (s *Resolver) validateRepositoryCodeowners(content, path string) []interface{} {
	out := []interface{}{}
	for index, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		for _, owner := range fields[1:] {
			kind := s.classifyCodeownersToken(owner)
			if kind == "" {
				continue
			}
			column := strings.Index(line, owner) + 1
			pointer := strings.Repeat(" ", column-1) + "^"
			out = append(out, map[string]interface{}{
				"line":       index + 1,
				"column":     column,
				"source":     line,
				"kind":       kind,
				"suggestion": nil,
				"message":    kind + " on line " + strconv.Itoa(index+1) + ":\n\n  " + line + "\n  " + pointer,
				"path":       path,
			})
		}
	}
	return out
}

// classifyCodeownersToken reports the error kind for one owner token, or ""
// when the token names a resolvable owner.
func (s *Resolver) classifyCodeownersToken(owner string) string {
	if strings.HasPrefix(owner, "@") {
		name := strings.TrimPrefix(owner, "@")
		if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Count(name, "/") > 1 {
			return "Invalid owner"
		}
		if orgName, teamSlug, isTeam := strings.Cut(name, "/"); isTeam {
			if s.store.GetOrg(orgName) == nil || s.store.GetTeam(orgName, teamSlug) == nil {
				return "Unknown owner"
			}
			return ""
		}
		if s.store.LookupUserByLogin(name) == nil && s.store.GetOrg(name) == nil {
			return "Unknown owner"
		}
		return ""
	}
	if at := strings.Index(owner, "@"); at > 0 && at < len(owner)-1 {
		return ""
	}
	return "Invalid owner"
}

// --- .gitmodules -----------------------------------------------------------

// repositorySubmoduleItems pairs each .gitmodules submodule with the commit the
// default-branch tree records for its path.
func (s *Resolver) repositorySubmoduleItems(repo *store.Repo) []gqlConnItem {
	stor, tree, ok := s.repositoryDefaultTree(repo)
	if !ok {
		return nil
	}
	content, found := readTreeFile(stor, tree, ".gitmodules")
	if !found {
		return nil
	}
	modules := parseGitmodules(string(content))
	items := make([]gqlConnItem, 0, len(modules))
	for _, module := range modules {
		row := module
		commitOID := interface{}(nil)
		if entry, err := tree.FindEntry(row.path); err == nil {
			commitOID = entry.Hash.String()
		}
		items = append(items, gqlConnItem{
			identity: repo.FullName + ":" + row.name,
			render: func() map[string]interface{} {
				return map[string]interface{}{
					"branch":              nilStr(row.branch),
					"gitUrl":              row.url,
					"name":                row.name,
					"nameRaw":             base64.StdEncoding.EncodeToString([]byte(row.name)),
					"path":                row.path,
					"pathRaw":             base64.StdEncoding.EncodeToString([]byte(row.path)),
					"subprojectCommitOid": commitOID,
				}
			},
		})
	}
	return items
}

type gitSubmodule struct {
	name   string
	path   string
	url    string
	branch string
}

// parseGitmodules reads the git-config subset .gitmodules uses: a
// [submodule "name"] section per entry with path/url/branch keys.
func parseGitmodules(content string) []gitSubmodule {
	var out []gitSubmodule
	var current *gitSubmodule
	flush := func() {
		if current != nil && current.path != "" && current.url != "" {
			out = append(out, *current)
		}
		current = nil
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			flush()
			header := strings.Trim(trimmed, "[]")
			if !strings.HasPrefix(header, "submodule") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(header, "submodule"))
			current = &gitSubmodule{name: strings.Trim(name, `"`)}
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "path":
			current.path = value
		case "url":
			current.url = value
		case "branch":
			current.branch = value
		}
	}
	flush()
	// Stable order: page boundaries must not shift with .gitmodules line order.
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}
