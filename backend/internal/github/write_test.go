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
	s, after, err := parsePRView(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.PRRef != (PRRef{Number: 42}) || s.State != "OPEN" || s.Draft || s.HeadRef != "ENG-1234-thing" ||
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
	if after != "" {
		t.Errorf("after = %q on the last page of checks", after)
	}
	if s.NodeID != "PR_kwDOabc" || !s.AutoMergeAllowed || !s.MergeQueue || s.InQueue || !s.AutoMerge {
		t.Errorf("merge state = %+v", s)
	}
	wantRemovals := []QueueRemoval{
		{At: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), Reason: "CI failed", Actor: "alice"},
		{At: time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC), Reason: "merge conflict", Actor: "github-merge-queue", ActorBot: true},
	}
	if !reflect.DeepEqual(s.QueueRemovals, wantRemovals) {
		t.Errorf("removals = %+v", s.QueueRemovals)
	}
	if _, _, err := parsePRView([]byte("nope")); err == nil {
		t.Error("garbage should fail")
	}
}

func TestParsePRViewStack(t *testing.T) {
	b := []byte(`{"repository":{"pullRequest":{"number":11,"baseRefName":"main",
"stack":{"number":3,"baseRefName":"main","entries":{"nodes":[
 {"position":2,"pullRequest":{"number":11,"state":"OPEN"}},
 {"position":1,"pullRequest":{"number":10,"state":"MERGED"}},
 {"position":3,"pullRequest":{"number":12,"state":"OPEN"}}]}},
"stackEntry":{"position":2}}}}`)
	s, _, err := parsePRView(b)
	if err != nil {
		t.Fatal(err)
	}
	want := PRStack{Number: 3, Base: "main", Bottom: 11, Position: 2, Size: 3}
	if s.Stack == nil || *s.Stack != want {
		t.Errorf("stack = %+v", s.Stack)
	}
	s, _, err = parsePRView([]byte(`{"repository":{"pullRequest":{"number":11,"stack":null,"stackEntry":null}}}`))
	if err != nil || s.Stack != nil {
		t.Errorf("plain PR: %+v, %v", s, err)
	}
}
