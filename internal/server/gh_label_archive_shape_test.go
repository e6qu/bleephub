package bleephub

import (
	"net/http"
	"testing"
)

// TestLabelsCarryTheArchiveFieldsEachResourceDocuments pins the label shape in
// each place GitHub documents one. The label resource and a pull request's
// labels must carry `archived_at` and `archived_by`; an issue's labels are
// documented with `archived_by` alone. Every response here also passes through
// the OpenAPI shape observer, which is what holds the three to the definition.
func TestLabelsCarryTheArchiveFieldsEachResourceDocuments(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "label-archive-shape")
	base := "/api/v3/repos/admin/label-archive-shape"

	label := decodeBody(t, s.post(t, base+"/labels", defaultToken, map[string]interface{}{"name": "triage", "color": "ededed"}), http.StatusCreated)
	for _, field := range []string{"archived_at", "archived_by"} {
		if value, present := label[field]; !present || value != nil {
			t.Errorf("label %s = %v (present %v), want an explicit null: nothing archives a label", field, value, present)
		}
	}

	issue := decodeBody(t, s.post(t, base+"/issues", defaultToken, map[string]interface{}{"title": "labelled", "labels": []string{"triage"}}), http.StatusCreated)
	issueLabel := issue["labels"].([]interface{})[0].(map[string]interface{})
	if _, present := issueLabel["archived_by"]; !present {
		t.Error("an issue's label lacks archived_by")
	}
	if _, present := issueLabel["archived_at"]; present {
		t.Error("an issue's label carries archived_at, which GitHub's issue shape does not define")
	}

	pull := decodeBody(t, s.post(t, base+"/pulls", defaultToken, map[string]interface{}{"title": "labelled pull", "head": "feat", "base": "main"}), http.StatusCreated)
	number := int(pull["number"].(float64))
	decodeJSONArray(t, s.post(t, base+"/issues/"+itoa(number)+"/labels", defaultToken, map[string]interface{}{"labels": []string{"triage"}}))
	fetched := decodeBody(t, s.get(t, base+"/pulls/"+itoa(number), defaultToken), http.StatusOK)
	pullLabel := fetched["labels"].([]interface{})[0].(map[string]interface{})
	for _, field := range []string{"archived_at", "archived_by"} {
		if _, present := pullLabel[field]; !present {
			t.Errorf("a pull request's label lacks %s, which GitHub requires of it", field)
		}
	}
}
