package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestParsePatch(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		hunks []Hunk
		added []int
	}{
		{"added file", "@@ -0,0 +1,3 @@\n+a\n+b\n+c", []Hunk{{1, 3}}, []int{1, 2, 3}},
		{"two hunks", "@@ -1,3 +1,4 @@\n a\n-b\n+B\n+B2\n c\n@@ -20,2 +21,3 @@\n x\n+y\n z",
			[]Hunk{{1, 4}, {21, 23}}, []int{2, 3, 22}},
		{"empty right side", "@@ -1,2 +0,0 @@\n-a\n-b", []Hunk{}, []int{}},
		{"missing count", "@@ -5 +5 @@\n-a\n+b", []Hunk{{5, 5}}, []int{5}},
		{"no newline marker", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file",
			[]Hunk{{1, 1}}, []int{1}},
		{"empty patch", "", []Hunk{}, []int{}},
	}
	for _, c := range cases {
		h, a := ParsePatch(c.patch)
		if !reflect.DeepEqual(h, c.hunks) || !reflect.DeepEqual(a, c.added) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, h, a, c.hunks, c.added)
		}
	}
}

func TestThreadNodeDecodesCommentsConnection(t *testing.T) {
	var n threadNode
	in := `{"id":"T1","path":"docs/a.md","comments":{"nodes":[{"id":"C1","body":"hi","author":{"login":"rene"}}]}}`
	if err := json.Unmarshal([]byte(in), &n); err != nil {
		t.Fatal(err)
	}
	if len(n.Comments.Nodes) != 1 || n.Comments.Nodes[0].Author.Login != "rene" || n.Path != "docs/a.md" {
		t.Fatalf("unexpected decode: %+v", n)
	}
}

func TestParseMarker(t *testing.T) {
	got := parseMarker("Why?\n\n<!-- design-review:L3-L7@abc123 -->")
	if got == nil || *got != (DocAnchor{3, 7, "abc123"}) {
		t.Fatalf("got %+v", got)
	}
	if parseMarker("no marker") != nil {
		t.Fatal("expected nil without a marker")
	}
}

const noPending = `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`
const reviewStarted = `{"data":{"addPullRequestReview":{"pullRequestReview":{"id":"R1"}}}}`

func TestCreateThreadReturnsID(t *testing.T) {
	ref, pr := PRRef{Repo: "o/r", Number: 7}, DesignPR{ID: "PR1", HeadSHA: "abc", Viewer: "me"}
	a, _ := newStub(map[string][]reply{"POST /repos/o/r/pulls/7/comments": {{status: 201, body: `{"node_id":"PRRC_rest"}`}}})
	id, err := a.CreateThread(context.Background(), ref, pr, NewThread{Path: "a.md", Body: "x", SubjectType: "LINE", Line: 3, Mode: "now"})
	if err != nil || id != "PRRC_rest" {
		t.Errorf("now: id = %q, %v", id, err)
	}
	a, _ = newStub(map[string][]reply{"POST /graphql": {
		{status: 200, body: noPending}, {status: 200, body: reviewStarted},
		{status: 200, body: `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[{"id":"PRRC_gql"}]}}}}}`},
	}})
	id, err = a.CreateThread(context.Background(), ref, pr, NewThread{Path: "a.md", Body: "x", SubjectType: "LINE", Line: 3, Mode: "review"})
	if err != nil || id != "PRRC_gql" {
		t.Errorf("review: id = %q, %v", id, err)
	}
}

func TestReplyThreadReturnsID(t *testing.T) {
	ref, pr := PRRef{Repo: "o/r", Number: 7}, DesignPR{ID: "PR1", HeadSHA: "abc", Viewer: "me"}
	a, _ := newStub(map[string][]reply{"POST /repos/o/r/pulls/7/comments/42/replies": {{status: 201, body: `{"node_id":"PRRC_rest"}`}}})
	id, err := a.ReplyThread(context.Background(), ref, pr, "T1", "42", "hi", "now")
	if err != nil || id != "PRRC_rest" {
		t.Errorf("now: id = %q, %v", id, err)
	}
	a, _ = newStub(map[string][]reply{"POST /graphql": {
		{status: 200, body: noPending}, {status: 200, body: reviewStarted},
		{status: 200, body: `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"PRRC_gql"}}}}`},
	}})
	id, err = a.ReplyThread(context.Background(), ref, pr, "T1", "42", "hi", "review")
	if err != nil || id != "PRRC_gql" {
		t.Errorf("review: id = %q, %v", id, err)
	}
}

func TestThreadOfComment(t *testing.T) {
	page := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
		{"id":"T1","path":"a.md","comments":{"nodes":[{"id":"C1","fullDatabaseId":"41"}]}},
		{"id":"T2","path":"b.md","comments":{"nodes":[{"id":"C2","fullDatabaseId":"40"},{"id":"C3","fullDatabaseId":"42"}]}}]}}}}}`
	a, _ := newStub(map[string][]reply{"POST /graphql": ok(page)})
	th, err := a.ThreadOfComment(context.Background(), PRRef{Repo: "o/r", Number: 7}, 42)
	if err != nil || th.ID != "T2" {
		t.Errorf("thread = %+v, %v", th, err)
	}
	if _, err := a.ThreadOfComment(context.Background(), PRRef{Repo: "o/r", Number: 7}, 99); !errors.Is(err, ErrNoThread) {
		t.Errorf("unknown id: err = %v", err)
	}
}
