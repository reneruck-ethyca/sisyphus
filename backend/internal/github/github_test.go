package github

import (
	"reflect"
	"testing"
)

func TestParsePR(t *testing.T) {
	pull := []byte(`{"state":"open","merged":false,"draft":true,"html_url":"https://github.com/ethyca/fidesplus/pull/7",
		"title":"T","number":7,"user":{"login":"carol"},
		"requested_reviewers":[{"login":"me"}],"requested_teams":[{"slug":"eng","name":"Eng"}],
		"head":{"ref":"feature/x","sha":"abc","repo":{"full_name":"ethyca/fidesplus"}},
		"base":{"repo":{"full_name":"ethyca/fidesplus"}}}`)
	reviews := []byte(`[{"user":{"login":"ci","type":"Bot"},"state":"APPROVED","body":"","submitted_at":"2026-10-20T12:01:00Z"}]`)
	rc := []byte(`[{"user":{"login":"bob","type":"User"},"created_at":"2026-10-20T12:02:00Z"}]`)
	ic := []byte(`[{"user":{"login":"alice","type":"User"},"created_at":"2026-10-20T12:03:00Z"}]`)
	pr, err := parsePR(pull, reviews, rc, ic)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || pr.HeadRef != "feature/x" || pr.HeadSHA != "abc" || pr.HeadRepo != "ethyca/fidesplus" ||
		pr.Repo != "ethyca/fidesplus" || pr.Number != 7 || pr.Author != "carol" || pr.State != "open" ||
		len(pr.RequestedReviewers) != 1 || len(pr.RequestedTeams) != 1 || pr.RequestedTeams[0] != "eng" {
		t.Errorf("bad pr: %+v", pr)
	}
	if len(pr.Reviews) != 1 || !pr.Reviews[0].Bot || len(pr.Comments) != 2 || pr.Comments[1].User != "alice" {
		t.Errorf("bad reviews/comments: %+v %+v", pr.Reviews, pr.Comments)
	}

	pull = []byte(`{"state":"open","number":8,"head":{"ref":"x","sha":"d","repo":null},"base":{"repo":{"full_name":"ethyca/a"}}}`)
	pr, err = parsePR(pull, []byte(`[]`), []byte(`[]`), []byte(`[]`))
	if err != nil || pr.HeadRepo != "" {
		t.Errorf("deleted fork: %+v %v", pr, err)
	}
}

func TestParseSearch(t *testing.T) {
	got, err := parseSearch([]byte(`[{"number":1,"repository":{"nameWithOwner":"ethyca/a"}}]`))
	if err != nil || !reflect.DeepEqual(got, []PRRef{{"ethyca/a", 1}}) {
		t.Errorf("got %v %v", got, err)
	}
}
