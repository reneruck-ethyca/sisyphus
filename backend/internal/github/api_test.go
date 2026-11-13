package github

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

type reply struct {
	status int
	body   string
	link   string // Link header
}

// stub answers by "METHOD /path"; a route's replies are used in turn, the last one repeating.
type stub struct {
	routes map[string][]reply
	got    []string // "METHOD /path?query body"
}

func (s *stub) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	key := r.Method + " " + r.URL.Path
	s.got = append(s.got, strings.TrimSpace(key+"?"+r.URL.RawQuery+" "+string(body)))
	rep := reply{status: 404, body: `{"message":"Not Found"}`}
	if rs := s.routes[key]; len(rs) > 0 {
		rep = rs[0]
		if len(rs) > 1 {
			s.routes[key] = rs[1:]
		}
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if rep.link != "" {
		h.Set("Link", rep.link)
	}
	return &http.Response{StatusCode: rep.status, Header: h, Body: io.NopCloser(strings.NewReader(rep.body)), Request: r}, nil
}

func newStub(routes map[string][]reply) (*API, *stub) {
	s := &stub{routes: routes}
	return NewAPI(api.ClientOptions{Host: "github.com", AuthToken: "t", Transport: s}), s
}

func ok(body string) []reply { return []reply{{status: 200, body: body}} }

func TestRetriesOnceAfter401(t *testing.T) {
	a, s := newStub(map[string][]reply{"GET /user": {{status: 401, body: `{"message":"Bad credentials"}`}, {status: 200, body: `{"login":"me"}`}}})
	if u, err := a.CurrentUser(context.Background()); err != nil || u != "me" || len(s.got) != 2 {
		t.Errorf("user = %q, %v after %d calls", u, err, len(s.got))
	}
	a, _ = newStub(map[string][]reply{"GET /user": {{status: 401, body: `{"message":"Bad credentials"}`}}})
	if _, err := a.CurrentUser(context.Background()); !isStatus(err, 401) {
		t.Errorf("a lasting 401 = %v", err)
	}
}

func TestTeamsFollowsPages(t *testing.T) {
	a, s := newStub(map[string][]reply{"GET /user/teams": {
		{status: 200, body: `[{"slug":"eng","organization":{"login":"ethyca"}},{"slug":"x","organization":{"login":"other"}}]`,
			link: `<https://api.github.com/user/teams?per_page=100&page=2>; rel="next", <https://api.github.com/user/teams?page=2>; rel="last"`},
		{status: 200, body: `[{"slug":"backend","organization":{"login":"ethyca"}}]`},
	}})
	got, err := a.Teams(context.Background(), "ethyca")
	if err != nil || !slices.Equal(got, []string{"eng", "backend"}) || len(s.got) != 2 {
		t.Errorf("teams = %v, %v; calls %v", got, err, s.got)
	}
}

func TestEditPR(t *testing.T) {
	a, s := newStub(map[string][]reply{
		"PATCH /repos/o/r/pulls/3":                    ok(`{}`),
		"POST /repos/o/r/pulls/3/requested_reviewers": ok(`{}`),
		"POST /repos/o/r/issues/3/labels":             ok(`[]`),
		// DELETE /repos/o/r/issues/3/labels/gone isn't routed: the 404 of a label already off is fine.
	})
	body := "new"
	err := a.EditPR(context.Background(), PRRef{Repo: "o/r", Number: 3},
		PREdit{Base: "main", Body: &body, AddReviewers: []string{"alice", "ethyca/backend"}, AddLabels: []string{"l"}, RemoveLabels: []string{"gone"}})
	want := []string{
		`PATCH /repos/o/r/pulls/3? {"base":"main","body":"new"}`,
		`POST /repos/o/r/pulls/3/requested_reviewers? {"reviewers":["alice"],"team_reviewers":["backend"]}`,
		`POST /repos/o/r/issues/3/labels? {"labels":["l"]}`,
		`DELETE /repos/o/r/issues/3/labels/gone?`,
	}
	if err != nil || !slices.Equal(s.got, want) {
		t.Errorf("err %v, calls:\n%s", err, strings.Join(s.got, "\n"))
	}

	a, s = newStub(nil)
	if err := a.EditPR(context.Background(), PRRef{Repo: "o/r", Number: 3}, PREdit{}); err != nil || len(s.got) != 0 {
		t.Errorf("empty edit: %v, calls %v", err, s.got)
	}
	a, _ = newStub(nil)
	if err := a.EditPR(context.Background(), PRRef{Repo: "o/r", Number: 3}, PREdit{RemoveReviewers: []string{"bob"}}); !isStatus(err, 404) {
		t.Errorf("a failed reviewer removal = %v", err)
	}
}

func TestCreatePR(t *testing.T) {
	a, s := newStub(map[string][]reply{
		"POST /repos/o/r/pulls":           ok(`{"number":7}`),
		"POST /repos/o/r/issues/7/labels": ok(`[]`),
	})
	ref, err := a.CreatePR(context.Background(), "o/r", NewPR{Base: "dev", Head: "b", Title: "t", Body: "x", Draft: true, Labels: []string{"l"}})
	if err != nil || ref != (PRRef{Repo: "o/r", Number: 7}) || len(s.got) != 2 ||
		s.got[0] != `POST /repos/o/r/pulls? {"base":"dev","body":"x","draft":true,"head":"b","title":"t"}` {
		t.Errorf("ref %+v, %v; calls %v", ref, err, s.got)
	}
	a, _ = newStub(map[string][]reply{"POST /repos/o/r/pulls": ok(`{"number":7}`)})
	if ref, err := a.CreatePR(context.Background(), "o/r", NewPR{Labels: []string{"l"}}); err == nil || ref.Number != 7 {
		t.Errorf("failed labelling: ref %+v, %v; want the PR and an error", ref, err)
	}
}

func TestCreateLabelUpdatesExisting(t *testing.T) {
	a, s := newStub(map[string][]reply{
		"POST /repos/o/r/labels":            {{status: 422, body: `{"message":"Validation Failed","errors":[{"code":"already_exists"}]}`}},
		"PATCH /repos/o/r/labels/tlm: wait": ok(`{}`),
	})
	if err := a.CreateLabel(context.Background(), "o/r", "tlm: wait", "ededed", "d"); err != nil || len(s.got) != 2 ||
		s.got[1] != `PATCH /repos/o/r/labels/tlm: wait? {"color":"ededed","description":"d"}` {
		t.Errorf("%v; calls %v", err, s.got)
	}
}

func TestFindPRSkipsForks(t *testing.T) {
	a, s := newStub(map[string][]reply{"GET /repos/o/r/pulls": ok(
		`[{"number":3,"head":{"repo":{"full_name":"fork/r"}}},{"number":4,"head":{"repo":null}},{"number":7,"head":{"repo":{"full_name":"o/r"}}}]`)})
	ref, found, err := a.FindPR(context.Background(), "o/r", "feat/x")
	if err != nil || !found || ref.Number != 7 || s.got[0] != "GET /repos/o/r/pulls?head=o%3Afeat%2Fx&state=open" {
		t.Errorf("%+v %v %v; calls %v", ref, found, err, s.got)
	}
	a, _ = newStub(map[string][]reply{"GET /repos/o/r/pulls": ok(`[{"number":3,"head":{"repo":{"full_name":"fork/r"}}}]`)})
	if _, found, err := a.FindPR(context.Background(), "o/r", "feat/x"); err != nil || found {
		t.Errorf("only a fork's PR: found = %v, %v", found, err)
	}
}

func TestMarkReady(t *testing.T) {
	a, s := newStub(map[string][]reply{"GET /repos/o/r/pulls/3": ok(`{"node_id":"PR_1","draft":false}`)})
	if err := a.MarkReady(context.Background(), PRRef{Repo: "o/r", Number: 3}); err != nil || len(s.got) != 1 {
		t.Errorf("not a draft: %v; calls %v", err, s.got)
	}
	a, s = newStub(map[string][]reply{
		"GET /repos/o/r/pulls/3": ok(`{"node_id":"PR_1","draft":true}`),
		"POST /graphql":          ok(`{"data":{"markPullRequestReadyForReview":{"clientMutationId":null}}}`),
	})
	if err := a.MarkReady(context.Background(), PRRef{Repo: "o/r", Number: 3}); err != nil || len(s.got) != 2 || !strings.Contains(s.got[1], `"id":"PR_1"`) {
		t.Errorf("draft: %v; calls %v", err, s.got)
	}
}

func TestGraphQLErrorsAreErrors(t *testing.T) {
	a, _ := newStub(map[string][]reply{"POST /graphql": ok(`{"data":null,"errors":[{"message":"nope"}]}`)})
	if _, err := a.Inbox(context.Background(), PRRef{Repo: "o/r", Number: 3}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
}

func TestFailedSteps(t *testing.T) {
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	log := "\ufeff2026-11-05T09:00:00.1Z setup\n" +
		"2026-11-05T09:00:02.5Z run tests\n" +
		"continued line\n" +
		"2026-11-05T09:00:04.9Z FAIL test_x\n" +
		"2026-11-05T09:00:06.0Z post cleanup\n"
	steps := []jobStep{
		{Name: "setup", Conclusion: "success", StartedAt: at("2026-11-05T09:00:00Z"), CompletedAt: at("2026-11-05T09:00:01Z")},
		{Name: "test", Conclusion: "failure", StartedAt: at("2026-11-05T09:00:02Z"), CompletedAt: at("2026-11-05T09:00:04Z")},
		{Name: "cleanup", Conclusion: "success", StartedAt: at("2026-11-05T09:00:06Z"), CompletedAt: at("2026-11-05T09:00:06Z")},
	}
	want := "2026-11-05T09:00:02.5Z run tests\ncontinued line\n2026-11-05T09:00:04.9Z FAIL test_x\n"
	if got := failedSteps(log, steps); got != want {
		t.Errorf("got %q", got)
	}
	if got := failedSteps(log, steps[:1]); got != log {
		t.Errorf("no failed step: want the whole log, got %q", got)
	}
}

func TestPRViewPagesChecks(t *testing.T) {
	page := func(name, next string) string {
		pi := `"pageInfo":{"hasNextPage":false,"endCursor":"c"}`
		if next != "" {
			pi = `"pageInfo":{"hasNextPage":true,"endCursor":"` + next + `"}`
		}
		return `{"data":{"repository":{"pullRequest":{"number":3,"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{` + pi +
			`,"nodes":[{"__typename":"CheckRun","name":"` + name + `","status":"COMPLETED","conclusion":"FAILURE"}]}}}}]}}}}}`
	}
	a, s := newStub(map[string][]reply{"POST /graphql": {{status: 200, body: page("one", "c1")}, {status: 200, body: page("two", "c2")}, {status: 200, body: page("three", "")}}})
	v, err := a.PRView(context.Background(), PRRef{Repo: "o/r", Number: 3})
	if err != nil || len(v.Checks) != 3 || v.Checks[2].Name != "three" || v.Number != 3 || len(s.got) != 3 || !strings.Contains(s.got[2], `"after":"c2"`) {
		t.Errorf("checks %+v, %v; calls %v", v.Checks, err, s.got)
	}
}
