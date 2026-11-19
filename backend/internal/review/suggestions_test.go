package review

import (
	"errors"
	"testing"
	"time"
)

const sugPR = "ethyca/a#1"

func sugFixture(t *testing.T) *Reviewer {
	t.Helper()
	r := newFixture(t).r
	f := &ReviewFile{Event: "COMMENT", Body: "overall",
		Comments: []InlineComment{{Path: "a.go", Line: 3, Body: "x"}, {Path: "b.go", Line: 5, StartLine: 4, Side: "LEFT", Body: "y"}},
		Replies:  []Reply{{CommentID: 77, Body: "z"}}}
	if n, err := r.addSuggestions(sugPR, "R1", "h1", time.Now(), f); err != nil || n != 3 {
		t.Fatalf("add: %d, %v", n, err)
	}
	return r
}

func sugStatus(t *testing.T, r *Reviewer, id string) Suggestion {
	t.Helper()
	s, err := r.Suggestions(sugPR)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range s.Items {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("no %s", id)
	return Suggestion{}
}

func TestAddSuggestions(t *testing.T) {
	r := sugFixture(t)
	s, _ := r.Suggestions(sugPR)
	want := []struct{ id, kind string }{{"R1-c0", "comment"}, {"R1-c1", "comment"}, {"R1-r0", "reply"}}
	if len(s.Items) != 3 {
		t.Fatalf("items = %d", len(s.Items))
	}
	for i, w := range want {
		it := s.Items[i]
		if it.ID != w.id || it.Kind != w.kind || it.Status != "open" || it.Head != "h1" {
			t.Errorf("item %d = %+v", i, it)
		}
	}
	if s.Items[2].CommentID != 77 {
		t.Errorf("reply comment id = %d", s.Items[2].CommentID)
	}
	if len(s.Runs) != 1 || s.Runs[0].Event != "COMMENT" || s.Runs[0].Body != "overall" {
		t.Errorf("runs = %+v", s.Runs)
	}
	if _, err := r.addSuggestions(sugPR, "R2", "h2", time.Now(), &ReviewFile{Comments: []InlineComment{{Path: "c", Line: 1, Body: "q"}}}); err != nil {
		t.Fatal(err)
	}
	s, _ = r.Suggestions(sugPR)
	if len(s.Items) != 4 || len(s.Runs) != 2 || s.Items[0].ID != "R1-c0" || s.Items[3].ID != "R2-c0" {
		t.Errorf("after R2: %+v", s)
	}
}

func TestBeginAcceptOnlyOnce(t *testing.T) {
	r := sugFixture(t)
	if _, err := r.BeginAccept(sugPR, "R1-c0", "h1"); err != nil {
		t.Fatal(err)
	}
	if got := sugStatus(t, r, "R1-c0").Status; got != "posting" {
		t.Errorf("status = %s", got)
	}
	if _, err := r.BeginAccept(sugPR, "R1-c0", "h1"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("second begin: %v", err)
	}
}

func TestAcceptOutdated(t *testing.T) {
	r := sugFixture(t)
	if _, err := r.BeginAccept(sugPR, "R1-c0", "h2"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("err = %v", err)
	}
	if got := sugStatus(t, r, "R1-c0").Status; got != "open" {
		t.Errorf("status = %s", got)
	}
}

func TestFinishAccept(t *testing.T) {
	r := sugFixture(t)
	if _, err := r.BeginAccept(sugPR, "R1-c0", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishAccept(sugPR, "R1-c0", "review", "edited", "PRRC_1"); err != nil {
		t.Fatal(err)
	}
	s := sugStatus(t, r, "R1-c0")
	if s.Status != "review" || s.EditedBody != "edited" || s.GitHubID != "PRRC_1" || s.DecidedAt.IsZero() {
		t.Errorf("s = %+v", s)
	}
}

func TestPostingIsNotAcceptable(t *testing.T) {
	r := sugFixture(t)
	for _, id := range []string{"R1-c0", "R1-c1"} {
		if _, err := r.BeginAccept(sugPR, id, "h1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.BeginAccept(sugPR, "R1-c0", "h1"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("begin on posting: %v", err)
	}
	if err := r.SetSuggestionStatus(sugPR, "R1-c0", "posted"); err != nil {
		t.Error(err)
	}
	if err := r.SetSuggestionStatus(sugPR, "R1-c1", "open"); err != nil {
		t.Error(err)
	}
}

func TestDismissUndo(t *testing.T) {
	r := sugFixture(t)
	for _, to := range []string{"dismissed", "open"} {
		if err := r.SetSuggestionStatus(sugPR, "R1-c0", to); err != nil {
			t.Fatal(err)
		}
	}
	r.SetSuggestionStatus(sugPR, "R1-c0", "dismissed")
	if err := r.SetSuggestionStatus(sugPR, "R1-c0", "posted"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("dismissed to posted: %v", err)
	}
	if err := r.SetSuggestionStatus(sugPR, "nope", "open"); !errors.Is(err, ErrNoSuggestion) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestSuggestionsMissingFile(t *testing.T) {
	s, err := newFixture(t).r.Suggestions("x/y#9")
	if err != nil || s.Runs == nil || s.Items == nil || len(s.Runs)+len(s.Items) != 0 {
		t.Errorf("%+v, %v", s, err)
	}
}

func TestOpenSuggestions(t *testing.T) {
	r := sugFixture(t)
	r.SetSuggestionStatus(sugPR, "R1-c0", "dismissed")
	if n := r.OpenSuggestions(sugPR); n != 2 {
		t.Errorf("open = %d", n)
	}
}

func TestSetSuggestionStatusFrom(t *testing.T) {
	r := sugFixture(t)
	if err := r.SetSuggestionStatusFrom(sugPR, "R1-c0", "dismissed", "open"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("open, from dismissed: %v", err)
	}
	if err := r.SetSuggestionStatusFrom(sugPR, "R1-c0", "open", "posted"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("a move sugMoves refuses: %v", err)
	}
	if err := r.SetSuggestionStatusFrom(sugPR, "R1-c0", "open", "dismissed"); err != nil || sugStatus(t, r, "R1-c0").Status != "dismissed" {
		t.Errorf("open to dismissed: %v", err)
	}
	if err := r.SetSuggestionStatusFrom(sugPR, "nope", "open", "dismissed"); !errors.Is(err, ErrNoSuggestion) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestReconcileSuggestion(t *testing.T) {
	r := sugFixture(t)
	r.BeginAccept(sugPR, "R1-c0", "h1")
	r.FinishAccept(sugPR, "R1-c0", "review", "x", "C2")
	if err := r.ReconcileSuggestion(sugPR, "R1-c0", "C1", "open"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("stale comment id: %v", err)
	}
	if err := r.ReconcileSuggestion(sugPR, "R1-c0", "C2", "posted"); err != nil || sugStatus(t, r, "R1-c0").Status != "posted" {
		t.Errorf("matching comment id: %v", err)
	}
	if err := r.ReconcileSuggestion(sugPR, "R1-c0", "C2", "open"); !errors.Is(err, ErrSuggestionState) {
		t.Errorf("no longer in review: %v", err)
	}
}
