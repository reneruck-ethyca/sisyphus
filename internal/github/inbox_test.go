package github

import "testing"

const inboxFixture = `{"data":{"repository":{"pullRequest":{
 "reviewThreads":{"nodes":[
  {"id":"T1","isResolved":false,"isOutdated":true,"path":"a.go","line":12,"comments":{"nodes":[
    {"databaseId":101,"body":"rename this","createdAt":"2026-11-05T10:00:00Z","url":"u1","author":{"login":"alice","__typename":"User"}},
    {"databaseId":102,"body":"ok","createdAt":"2026-11-05T10:05:00Z","url":"u2","author":{"login":"copilot-pull-request-reviewer[bot]","__typename":"Bot"}}]}},
  {"id":"T2","isResolved":true,"isOutdated":false,"path":"b.go","line":null,"comments":{"nodes":[
    {"databaseId":103,"body":"gone","createdAt":"2026-11-05T10:00:00Z","url":"u3","author":null}]}}]},
 "reviews":{"nodes":[
  {"databaseId":201,"state":"COMMENTED","body":"overall","createdAt":"2026-11-05T11:00:00Z","url":"r1","author":{"login":"bob","__typename":"User"}},
  {"databaseId":202,"state":"APPROVED","body":"lgtm","createdAt":"2026-11-05T11:00:00Z","url":"r2","author":{"login":"bob","__typename":"User"}},
  {"databaseId":203,"state":"CHANGES_REQUESTED","body":"  ","createdAt":"2026-11-05T11:00:00Z","url":"r3","author":{"login":"bob","__typename":"User"}},
  {"databaseId":204,"state":"CHANGES_REQUESTED","body":"fix it","createdAt":"2026-11-05T11:00:00Z","url":"r4","author":{"login":"EthycaBot","__typename":"User"}}]},
 "comments":{"nodes":[
  {"databaseId":301,"body":"hello","createdAt":"2026-11-05T12:00:00Z","url":"c1","author":{"login":"carol","__typename":"User"}}]}
}}}}`

func TestParseInbox(t *testing.T) {
	in, err := parseInbox([]byte(inboxFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Threads) != 2 || in.Threads[0].ID != "T1" || !in.Threads[0].Outdated || in.Threads[0].Path != "a.go" || in.Threads[0].Line != 12 || in.Threads[1].Line != 0 || !in.Threads[1].Resolved {
		t.Fatalf("threads = %+v", in.Threads)
	}
	c := in.Threads[0].Comments
	if len(c) != 2 || c[0].ID != 101 || c[0].Bot || c[0].Author != "alice" || !c[1].Bot || c[1].URL != "u2" || c[1].Created.IsZero() {
		t.Errorf("comments = %+v", c)
	}
	if g := in.Threads[1].Comments[0]; g.Author != "ghost" || g.Bot {
		t.Errorf("deleted author = %+v", g)
	}
	if len(in.Reviews) != 2 || in.Reviews[0].ID != 201 || in.Reviews[1].ID != 204 || in.Reviews[1].State != "CHANGES_REQUESTED" {
		t.Errorf("reviews = %+v", in.Reviews) // approvals and empty bodies are not feedback
	}
	if len(in.Issue) != 1 || in.Issue[0].ID != 301 {
		t.Errorf("issue = %+v", in.Issue)
	}
}

func TestParseInboxErrors(t *testing.T) {
	if _, err := parseInbox([]byte(`{"errors":[{"message":"nope"}]}`)); err == nil {
		t.Error("graphql error not reported")
	}
	if _, err := parseInbox([]byte(`x`)); err == nil {
		t.Error("bad json not reported")
	}
}

func TestParseCommentID(t *testing.T) {
	if got := parseCommentID("https://github.com/o/r/pull/7#issuecomment-12345\n"); got != 12345 {
		t.Errorf("id = %d", got)
	}
	if got := parseCommentID("nothing"); got != 0 {
		t.Errorf("id = %d", got)
	}
}
