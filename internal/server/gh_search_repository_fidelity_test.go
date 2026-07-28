package bleephub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"
)

func repositorySearchNames(t *testing.T, s *Server, query string) []string {
	t.Helper()
	w := fuzzServe(s, http.MethodGet, "/api/v3/search/repositories?q="+url.QueryEscape(query), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search %q status = %d, want 200; body=%s", query, w.Code, w.Body.String())
	}
	var response struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			FullName string `json:"full_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode search %q: %v", query, err)
	}
	if response.TotalCount != len(response.Items) {
		t.Fatalf("search %q total_count=%d items=%d", query, response.TotalCount, len(response.Items))
	}
	names := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		names = append(names, item.FullName)
	}
	sort.Strings(names)
	return names
}

func TestSearchRepositoriesOfficialQualifierMatrix(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "repository-search-fidelity", "", "")
	if org == nil {
		t.Fatal("create organization")
	}
	golden := s.store.CreateOrgRepo(org, admin, "golden-service", "multi word search needle", false)
	plain := s.store.CreateOrgRepo(org, admin, "plain-service", "ordinary repository", false)
	archived := s.store.CreateOrgRepo(org, admin, "archived-template", "retired web service", false)
	private := s.store.CreateOrgRepo(org, admin, "private-service", "private repository", true)
	if golden == nil || plain == nil || archived == nil || private == nil {
		t.Fatal("create repositories")
	}
	created := time.Date(2025, 2, 3, 12, 0, 0, 0, time.UTC)
	s.store.UpdateRepo(org.Login, golden.Name, func(repo *Repo) {
		repo.Topics = []string{"web", "banking"}
		repo.Language = "Go"
		repo.StargazersCount = 7
		repo.LicenseKey = "mit"
		repo.LicenseSPDX = "MIT"
		repo.CreatedAt = created
		repo.PushedAt = created.Add(24 * time.Hour)
	})
	s.store.UpdateRepo(org.Login, archived.Name, func(repo *Repo) {
		repo.Topics = []string{"web"}
		repo.Archived = true
		repo.IsTemplate = true
	})
	if err := s.initRepoFiles(context.Background(), golden, "main", golden.Description, "", "", true); err != nil {
		t.Fatalf("initialize README: %v", err)
	}
	s.store.UpsertCustomProperty(org.Login, &CustomProperty{
		PropertyName: "environment",
		ValueType:    "string",
	})
	s.store.SetRepoCustomPropertyValues(golden.FullName, []customPropertyValuePayload{{
		PropertyName: "environment",
		Value:        "production",
	}})
	s.store.CreateArtifactStorageRecord(&ArtifactStorageRecord{
		OrgID:            org.ID,
		Name:             "golden",
		Digest:           "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:           "active",
		GitHubRepository: golden.FullName,
	})
	s.store.UpsertArtifactDeploymentRecord(&ArtifactDeploymentRecord{
		OrgID:               org.ID,
		Name:                "golden",
		Digest:              "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:              "deployed",
		LogicalEnvironment:  "production",
		PhysicalEnvironment: "eu",
		Cluster:             "primary",
		DeploymentName:      "golden",
		GitHubRepository:    golden.FullName,
	})

	tests := []struct {
		query string
		want  []string
	}{
		{"org:" + org.Login + " -topic:web", []string{plain.FullName, private.FullName}},
		{"org:" + org.Login + " -topic:web archived:false", []string{plain.FullName, private.FullName}},
		{"org:" + org.Login + " topic:web archived:false", []string{golden.FullName}},
		{"org:" + org.Login + " archived:true template:true", []string{archived.FullName}},
		{"org:" + org.Login + " is:private", []string{private.FullName}},
		{"org:" + org.Login + " visibility:public -archived:true", []string{golden.FullName, plain.FullName}},
		{"org:" + org.Login + " stars:>=7 topics:2", []string{golden.FullName}},
		{"org:" + org.Login + " license:MIT", []string{golden.FullName}},
		{"org:" + org.Login + " created:2025-02-03 pushed:>2025-02-03", []string{golden.FullName}},
		{`org:` + org.Login + ` "multi word" in:description`, []string{golden.FullName}},
		{"org:" + org.Login + " golden in:name", []string{golden.FullName}},
		{"org:" + org.Login + " needle in:readme", []string{golden.FullName}},
		{"org:" + org.Login + " topics:0", []string{plain.FullName, private.FullName}},
		{"org:" + org.Login + " props.environment:production", []string{golden.FullName}},
		{"org:" + org.Login + " deployable:true deployed:true", []string{golden.FullName}},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			got := repositorySearchNames(t, s, test.query)
			sort.Strings(test.want)
			if len(got) != len(test.want) {
				t.Fatalf("results=%v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("results=%v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestSearchRepositoriesForkModesAndExclusions(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	source := s.store.CreateRepo(admin, "fork-search-source", "fork-mode-fidelity", false)
	if source == nil {
		t.Fatal("create source")
	}
	fork := s.store.ForkRepo(admin, source, "fork-search-copy")
	if fork == nil {
		t.Fatal("create fork")
	}

	tests := []struct {
		query string
		want  []string
	}{
		{"user:admin fork-mode-fidelity", []string{source.FullName}},
		{"user:admin fork-mode-fidelity fork:true", []string{source.FullName, fork.FullName}},
		{"user:admin fork-mode-fidelity fork:false", []string{source.FullName}},
		{"user:admin fork-mode-fidelity fork:only", []string{fork.FullName}},
		{"user:admin fork-mode-fidelity -fork:true", []string{source.FullName}},
		{"user:admin fork-mode-fidelity -repo:" + source.FullName + " fork:true", []string{fork.FullName}},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			got := repositorySearchNames(t, s, test.query)
			sort.Strings(test.want)
			if len(got) != len(test.want) {
				t.Fatalf("results=%v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("results=%v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestSearchQueryGrammarAndQualifierValidation(t *testing.T) {
	s := fuzzRoutedServer(t)
	for name, path := range map[string]string{
		"invalid-number":        "/api/v3/search/repositories?q=stars%3Aseven",
		"reversed-range":        "/api/v3/search/repositories?q=topics%3A7..2",
		"invalid-date":          "/api/v3/search/repositories?q=created%3Ayesterday",
		"invalid-boolean":       "/api/v3/search/repositories?q=archived%3Amaybe",
		"wrong-endpoint":        "/api/v3/search/issues?q=archived%3Atrue",
		"ignored-repo-label":    "/api/v3/search/repositories?q=label%3Abug",
		"negated-location":      "/api/v3/search/repositories?q=-in%3Aname",
		"unsupported-has-value": "/api/v3/search/repositories?q=has%3Amagic",
	} {
		t.Run(name, func(t *testing.T) {
			w := fuzzServe(s, http.MethodGet, path, nil)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}
