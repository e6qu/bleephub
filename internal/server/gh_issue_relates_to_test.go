package bleephub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func relatedNumbers(t *testing.T, s *isolatedServer, path, token string) []int {
	t.Helper()
	list := decodeJSONWithStatus2xxArray(t, s.get(t, path+"/relates_to", token), http.StatusOK)
	out := make([]int, 0, len(list))
	for _, item := range list {
		out = append(out, int(item["number"].(float64)))
	}
	return out
}

func subscribeEveryEvent(t *testing.T, s *isolatedServer, repoFullName string) *advisoryEventRecorder {
	t.Helper()
	recorder := &advisoryEventRecorder{}
	receiver := httptest.NewTLSServer(recorder.handler())
	t.Cleanup(receiver.Close)
	s.store.CreateHook(repoFullName, receiver.URL, "", "json", "0", []string{"*"}, true)
	return recorder
}

// TestIssueRelatesTo_SameRepository covers the relationship's life between
// two issues of one repository: it has no direction, it is refused twice and
// to itself, and one delivery names both issues.
func TestIssueRelatesTo_SameRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, false)
	ref := repoRef{owner: "admin", name: repo}
	aID, aNum := s.createIssueForTest(t, ref, "first")
	bID, bNum := s.createIssueForTest(t, ref, "second")
	aPath := fmt.Sprintf("/api/v3/repos/admin/%s/issues/%d", repo, aNum)
	bPath := fmt.Sprintf("/api/v3/repos/admin/%s/issues/%d", repo, bNum)
	recorder := subscribeEveryEvent(t, s, "admin/"+repo)

	if got := relatedNumbers(t, s, aPath, defaultToken); len(got) != 0 {
		t.Fatalf("initial relates_to = %v, want empty", got)
	}
	created := decodeJSONWithStatus(t, s.post(t, aPath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": bID}), http.StatusCreated)
	if int(created["number"].(float64)) != bNum {
		t.Fatalf("POST relates_to returned #%v, want the related issue #%d", created["number"], bNum)
	}
	if got := relatedNumbers(t, s, aPath, defaultToken); len(got) != 1 || got[0] != bNum {
		t.Fatalf("relates_to of #%d = %v, want [%d]", aNum, got, bNum)
	}
	if got := relatedNumbers(t, s, bPath, defaultToken); len(got) != 1 || got[0] != aNum {
		t.Fatalf("relates_to of #%d = %v, want [%d]: the relationship has no direction", bNum, got, aNum)
	}

	requireStatus(t, s.post(t, aPath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": bID}), http.StatusUnprocessableEntity)
	requireStatus(t, s.post(t, bPath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": aID}), http.StatusUnprocessableEntity)
	requireStatus(t, s.post(t, aPath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": aID}), http.StatusUnprocessableEntity)
	requireStatus(t, s.post(t, aPath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": 999999}), http.StatusNotFound)
	requireStatus(t, s.post(t, aPath+"/relates_to", defaultToken, map[string]interface{}{}), http.StatusUnprocessableEntity)

	waitUntil(t, "issue_relates_to relates_to_added", func() bool { return recorder.has("issue_relates_to", "relates_to_added") })
	added, _ := recorder.find("issue_relates_to", "relates_to_added")
	if int(added.payload["issue_id"].(float64)) != aID || int(added.payload["related_issue_id"].(float64)) != bID {
		t.Errorf("same-repository delivery names issue %v and related issue %v, want %d and %d",
			added.payload["issue_id"], added.payload["related_issue_id"], aID, bID)
	}
	if added.payload["related_issue"] == nil || added.payload["repository"] == nil || added.payload["sender"] == nil {
		t.Errorf("same-repository delivery lacks a member: %v", added.payload)
	}

	// Removal from the other side removes the one relationship.
	removed := decodeJSONWithStatus(t, s.delete(t, bPath+"/relates_to/"+strconv.Itoa(aID), defaultToken), http.StatusOK)
	if int(removed["number"].(float64)) != aNum {
		t.Fatalf("DELETE relates_to returned #%v, want #%d", removed["number"], aNum)
	}
	if got := relatedNumbers(t, s, aPath, defaultToken); len(got) != 0 {
		t.Fatalf("relates_to after removal = %v, want empty", got)
	}
	requireStatus(t, s.delete(t, bPath+"/relates_to/"+strconv.Itoa(aID), defaultToken), http.StatusNotFound)
	requireStatus(t, s.delete(t, bPath+"/relates_to/not-a-number", defaultToken), http.StatusBadRequest)
	waitUntil(t, "issue_relates_to relates_to_removed", func() bool { return recorder.has("issue_relates_to", "relates_to_removed") })
}

// TestIssueRelatesTo_AcrossRepositories relates issues of two repositories:
// each repository gets its own delivery naming only its own issue, and an
// issue in a repository the caller cannot read can neither be related nor be
// seen through a relationship.
func TestIssueRelatesTo_AcrossRepositories(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	here := s.seedRepo(t, "relates-here", false)
	there := s.seedRepo(t, "relates-there", false)
	secret := s.seedRepo(t, "relates-secret", true)
	own := s.store.CreateIssue(here.ID, admin.ID, "here", "", nil, nil, 0)
	other := s.store.CreateIssue(there.ID, admin.ID, "there", "", nil, nil, 0)
	hidden := s.store.CreateIssue(secret.ID, admin.ID, "hidden", "", nil, nil, 0)
	herePath := fmt.Sprintf("/api/v3/repos/%s/issues/%d", here.FullName, own.Number)
	hereHook := subscribeEveryEvent(t, s, here.FullName)
	thereHook := subscribeEveryEvent(t, s, there.FullName)

	created := decodeJSONWithStatus(t, s.post(t, herePath+"/relates_to", defaultToken, map[string]interface{}{"issue_id": other.ID}), http.StatusCreated)
	if created["repository_url"] == nil || int(created["number"].(float64)) != other.Number {
		t.Fatalf("cross-repository POST returned %v", created)
	}
	for name, recorder := range map[string]*advisoryEventRecorder{here.FullName: hereHook, there.FullName: thereHook} {
		waitUntil(t, name+" issue_relates_to", func() bool { return recorder.has("issue_relates_to", "relates_to_added") })
		delivery, _ := recorder.find("issue_relates_to", "relates_to_added")
		if _, has := delivery.payload["related_issue"]; has {
			t.Errorf("the delivery to %s names the other repository's issue: %v", name, delivery.payload)
		}
		if _, has := delivery.payload["related_issue_id"]; has {
			t.Errorf("the delivery to %s names the other repository's issue id", name)
		}
		repository, _ := delivery.payload["repository"].(map[string]interface{})
		if repository["full_name"] != name {
			t.Errorf("the delivery to %s carries repository %v", name, repository["full_name"])
		}
	}

	// A stranger reads the public repository but not the private one.
	_, strangerToken := s.newUser(t, "relates-stranger")
	s.store.AddIssueRelatesTo(own.ID, hidden.ID)
	if got := relatedNumbers(t, s, herePath, strangerToken); len(got) != 1 || got[0] != other.Number {
		t.Fatalf("a stranger sees relates_to %v, want only #%d of the public repository", got, other.Number)
	}
	strangerRepo := s.seedRepo(t, "relates-stranger-own", false)
	if !s.store.AddRepoCollaborator("admin", strangerRepo.Name, "relates-stranger", "push") {
		t.Fatal("add the stranger as a collaborator")
	}
	strangerIssue := s.store.CreateIssue(strangerRepo.ID, admin.ID, "stranger's", "", nil, nil, 0)
	strangerPath := fmt.Sprintf("/api/v3/repos/%s/issues/%d", strangerRepo.FullName, strangerIssue.Number)
	requireStatus(t, s.post(t, strangerPath+"/relates_to", strangerToken, map[string]interface{}{"issue_id": hidden.ID}), http.StatusNotFound)
	if related := s.store.ListIssueRelatesTo(strangerIssue.ID); len(related) != 0 {
		t.Fatalf("a relationship to an unreadable issue was stored: %v", related)
	}
}
