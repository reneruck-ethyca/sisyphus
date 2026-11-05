package github

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestParsePRView(t *testing.T) {
	b, err := os.ReadFile("testdata/pr_view.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := parsePRView(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.PRRef != (PRRef{Repo: "ethyca/fidesplus", Number: 42}) || s.State != "OPEN" || s.Draft || s.HeadRef != "ENG-1234-thing" ||
		s.BaseRef != "dev" || s.MergeState != "BEHIND" || s.Mergeable != "MERGEABLE" || s.ReviewDecision != "CHANGES_REQUESTED" ||
		s.Author != "reneruck" || s.Additions != 120 || s.Deletions != 8 || !s.MergedAt.IsZero() || !s.ClosedAt.IsZero() {
		t.Errorf("snapshot = %+v", s)
	}
	if !reflect.DeepEqual(s.Files, []string{"src/a.py", "tests/test_a.py"}) {
		t.Errorf("files = %v", s.Files)
	}
	if len(s.Reviews) != 2 || s.Reviews[0].User != "alice" || s.Reviews[0].Bot || s.Reviews[0].CommitSHA[0] != 'a' ||
		!s.Reviews[1].Bot || !s.Reviews[0].SubmittedAt.Equal(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("reviews = %+v", s.Reviews)
	}
	if !reflect.DeepEqual(s.RequestedUsers, []string{"bob"}) || !reflect.DeepEqual(s.RequestedTeams, []string{"backend"}) {
		t.Errorf("requested = %v %v", s.RequestedUsers, s.RequestedTeams)
	}
	want := []Check{
		{Name: "pytest", Workflow: "CI", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://github.com/ethyca/fidesplus/actions/runs/99/job/1"},
		{Name: "lint", Workflow: "CI", Status: "IN_PROGRESS", URL: "https://github.com/ethyca/fidesplus/actions/runs/99/job/2"},
		{Name: "ci/legacy", Status: "IN_PROGRESS", URL: "https://example.com/legacy"},
		{Name: "ci/other", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://example.com/other"},
	}
	if !reflect.DeepEqual(s.Checks, want) {
		t.Errorf("checks = %+v", s.Checks)
	}
	if _, err := parsePRView([]byte("nope")); err == nil {
		t.Error("garbage should fail")
	}
}

func TestParsePRURL(t *testing.T) {
	ref, err := parsePRURL("Creating pull request for x into dev in ethyca/fidesplus\n\nhttps://github.com/ethyca/fidesplus/pull/7\n")
	if err != nil || ref != (PRRef{Repo: "ethyca/fidesplus", Number: 7}) {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, bad := range []string{"", "https://github.com/ethyca/fidesplus/issues/7", "pull request failed", "https://github.com/a/b/pull/x"} {
		if _, err := parsePRURL(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestWriteArgs(t *testing.T) {
	ref := PRRef{Repo: "o/r", Number: 3}
	body := "new"
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"create", createPRArgs("o/r", NewPR{Base: "dev", Head: "b", Title: "t", Body: "x", Draft: true, Labels: []string{"l1", "l2"}}),
			[]string{"pr", "create", "--repo", "o/r", "--base", "dev", "--head", "b", "--title", "t", "--body", "x", "--draft", "--label", "l1", "--label", "l2"}},
		{"edit", editArgs(ref, PREdit{AddReviewers: []string{"a", "b"}, RemoveReviewers: []string{"c"}, AddLabels: []string{"l"}, Base: "main", Body: &body}),
			[]string{"pr", "edit", "3", "--repo", "o/r", "--base", "main", "--body", "new", "--add-reviewer", "a,b", "--remove-reviewer", "c", "--add-label", "l"}},
		{"edit remove label", editArgs(ref, PREdit{RemoveLabels: []string{"x", "y"}}),
			[]string{"pr", "edit", "3", "--repo", "o/r", "--remove-label", "x,y"}},
		{"edit nothing", editArgs(ref, PREdit{}), nil},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParsePRList(t *testing.T) {
	if ref, ok, err := parsePRList("o/r", `[{"number":7}]`); err != nil || !ok || ref != (PRRef{Repo: "o/r", Number: 7}) {
		t.Errorf("one = %+v %v %v", ref, ok, err)
	}
	if ref, ok, err := parsePRList("o/r", `[{"number":3,"isCrossRepository":true},{"number":7}]`); err != nil || !ok || ref.Number != 7 {
		t.Errorf("fork PR adopted: %v %v %v", ref, ok, err)
	}
	if _, ok, err := parsePRList("o/r", `[{"number":3,"isCrossRepository":true}]`); err != nil || ok {
		t.Errorf("only a fork's PR: found = %v, %v", ok, err)
	}
	if _, ok, err := parsePRList("o/r", `[]`); err != nil || ok {
		t.Errorf("none = %v %v", ok, err)
	}
	if _, _, err := parsePRList("o/r", `x`); err == nil {
		t.Error("bad json should fail")
	}
}
