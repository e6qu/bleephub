package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func primaryEmailTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHAccountSettingsRoutes()
	return s
}

func doPrimaryEmailReq(s *Server, token string, body []byte) *httptest.ResponseRecorder {
	return serveTestRequest(s, bearerHeader(token), "PUT", "/ui-data/user/emails/primary", body)
}

func TestPrimaryEmail_PromoteVerifiedAddress(t *testing.T) {
	s := primaryEmailTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	if _, ok := s.store.AddUserEmails(admin.ID, []string{"second@bleephub.local"}); !ok {
		t.Fatal("add secondary email")
	}

	w := doPrimaryEmailReq(s, adminPAT, []byte(`{"email":"second@bleephub.local"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("promote status = %d, body = %s", w.Code, w.Body.String())
	}
	var emails []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &emails)
	if len(emails) < 2 {
		t.Fatalf("emails = %v, want the full list", emails)
	}
	if emails[0]["email"] != "second@bleephub.local" || emails[0]["primary"] != true {
		t.Fatalf("first entry = %v, want the new primary first", emails[0])
	}
	for _, e := range emails[1:] {
		if e["primary"] == true {
			t.Fatalf("demoted entry still primary: %v", e)
		}
	}

	// The user payload's primary follows.
	if u := s.store.GetUserByID(admin.ID); u.Email != "second@bleephub.local" {
		t.Fatalf("user primary email = %q, want the promoted address", u.Email)
	}
}

func TestPrimaryEmail_UnknownAddressIs422(t *testing.T) {
	s := primaryEmailTestServer(t)
	w := doPrimaryEmailReq(s, adminPAT, []byte(`{"email":"nobody@else.example"}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown address status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
}

func TestPrimaryEmail_UnverifiedAddressIs422(t *testing.T) {
	s := primaryEmailTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	if _, ok := s.store.AddUserEmails(admin.ID, []string{"pending@bleephub.local"}); !ok {
		t.Fatal("add pending email")
	}
	s.store.Mu.Lock()
	u := s.store.Users[admin.ID]
	for i := range u.Emails {
		if u.Emails[i].Email == "pending@bleephub.local" {
			u.Emails[i].Verified = false
		}
	}
	s.store.Mu.Unlock()

	w := doPrimaryEmailReq(s, adminPAT, []byte(`{"email":"pending@bleephub.local"}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unverified address status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if u := s.store.GetUserByID(admin.ID); u.Email == "pending@bleephub.local" {
		t.Fatal("unverified address became primary")
	}
}

func TestPrimaryEmail_MissingFieldAndAuth(t *testing.T) {
	s := primaryEmailTestServer(t)
	if w := doPrimaryEmailReq(s, adminPAT, []byte(`{}`)); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing email status = %d, want 422", w.Code)
	}
	if w := doPrimaryEmailReq(s, "", []byte(`{"email":"x@y.z"}`)); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", w.Code)
	}
}
