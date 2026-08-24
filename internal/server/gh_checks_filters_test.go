package bleephub

import (
	"net/http"
	"testing"
)

// TestCheckRunListingHonoursCheckNameFilter pins the documented check_name
// filter on both check-run listings, and the naming rule the `latest` filter
// rests on.
//
// check_name is how a client asks about one check among the many a commit
// carries — a required-status gate polling for "build" should not have to page
// through every other app's runs and filter client-side, and a listing that
// ignores the parameter tells it the check it asked about is whatever came back
// first.
func TestCheckRunListingHonoursCheckNameFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	head := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/admin/"+repo+"/commits/main", defaultToken), http.StatusOK)
	sha, _ := head["sha"].(string)
	if sha == "" {
		t.Fatal("no head sha to attach check runs to")
	}

	base := "/api/v3/repos/admin/" + repo + "/check-runs"
	// Two differently-named checks, and a rerun of one of them. This is the
	// shape that a suite-keyed `latest` collapsed wrongly: the rerun of "build"
	// must supersede the first "build" without hiding "lint" beside it.
	for _, spec := range []map[string]interface{}{
		{"name": "build", "head_sha": sha, "status": "completed", "conclusion": "failure"},
		{"name": "lint", "head_sha": sha, "status": "completed", "conclusion": "success"},
		{"name": "build", "head_sha": sha, "status": "completed", "conclusion": "success"},
	} {
		requireStatus(t, s.post(t, base, defaultToken, spec), http.StatusCreated)
	}

	listing := "/api/v3/repos/admin/" + repo + "/commits/" + sha + "/check-runs"
	countRuns := func(t *testing.T, path string) (int, []map[string]interface{}) {
		t.Helper()
		data := decodeJSONWithStatus(t, s.get(t, path, defaultToken), http.StatusOK)
		raw, _ := data["check_runs"].([]interface{})
		runs := make([]map[string]interface{}, 0, len(raw))
		for _, item := range raw {
			if run, _ := item.(map[string]interface{}); run != nil {
				runs = append(runs, run)
			}
		}
		total, _ := data["total_count"].(float64)
		if int(total) != len(runs) {
			t.Errorf("total_count = %v but %d runs returned for %s", data["total_count"], len(runs), path)
		}
		return len(runs), runs
	}

	// filter=all is every run, reruns included.
	if got, _ := countRuns(t, listing+"?filter=all"); got != 3 {
		t.Errorf("filter=all = %d runs, want 3", got)
	}
	// The default keeps the newest run of each named check — both names, not
	// one run for the whole suite.
	got, runs := countRuns(t, listing)
	if got != 2 {
		t.Errorf("default filter = %d runs, want 2 (newest build + lint)", got)
	}
	names := map[string]string{}
	for _, run := range runs {
		name, _ := run["name"].(string)
		conclusion, _ := run["conclusion"].(string)
		names[name] = conclusion
	}
	if names["build"] != "success" {
		t.Errorf("latest build conclusion = %q, want success (the rerun supersedes the first run)", names["build"])
	}
	if names["lint"] != "success" {
		t.Errorf("lint is missing from the default listing (%v) — a rerun of one check must not hide another", names)
	}

	// check_name selects one named check.
	if got, _ := countRuns(t, listing+"?check_name=lint&filter=all"); got != 1 {
		t.Errorf("check_name=lint&filter=all = %d runs, want 1", got)
	}
	if got, _ := countRuns(t, listing+"?check_name=build&filter=all"); got != 2 {
		t.Errorf("check_name=build&filter=all = %d runs, want 2 (the run and its rerun)", got)
	}
	if got, _ := countRuns(t, listing+"?check_name=build"); got != 1 {
		t.Errorf("check_name=build = %d runs, want 1", got)
	}
	if got, _ := countRuns(t, listing+"?check_name=nonesuch&filter=all"); got != 0 {
		t.Errorf("check_name=nonesuch = %d runs, want 0", got)
	}

	// The suite listing takes the same filters.
	suites := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/admin/"+repo+"/commits/"+sha+"/check-suites", defaultToken), http.StatusOK)
	rawSuites, _ := suites["check_suites"].([]interface{})
	if len(rawSuites) == 0 {
		t.Fatal("no check suite to list runs from")
	}
	suite, _ := rawSuites[0].(map[string]interface{})
	suiteID, _ := suite["id"].(float64)
	suiteListing := "/api/v3/repos/admin/" + repo + "/check-suites/" + itoa(int(suiteID)) + "/check-runs"

	if got, _ := countRuns(t, suiteListing+"?filter=all"); got != 3 {
		t.Errorf("suite filter=all = %d runs, want 3", got)
	}
	if got, _ := countRuns(t, suiteListing+"?check_name=lint&filter=all"); got != 1 {
		t.Errorf("suite check_name=lint = %d runs, want 1", got)
	}
	// The suite's default listing must not collapse to one run just because
	// every run in it shares the suite.
	if got, _ := countRuns(t, suiteListing); got != 2 {
		t.Errorf("suite default filter = %d runs, want 2", got)
	}
	if got, _ := countRuns(t, suiteListing+"?status=completed&filter=all"); got != 3 {
		t.Errorf("suite status=completed = %d runs, want 3", got)
	}
	if got, _ := countRuns(t, suiteListing+"?status=queued&filter=all"); got != 0 {
		t.Errorf("suite status=queued = %d runs, want 0", got)
	}
	requireStatus(t, s.get(t, suiteListing+"?filter=bogus", defaultToken), http.StatusUnprocessableEntity)
}
