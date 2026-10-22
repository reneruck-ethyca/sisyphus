package github

import "context"

// Fake is an in-memory Client for tests.
type Fake struct {
	Cands    []PRRef
	CandErr  error
	PRs      map[string]PR
	PRErr    error
	PRCalls  int
	MyTeams  []string // returned by Teams
	TeamsErr error
}

func (f *Fake) CurrentUser(context.Context) (string, error) { return "me", nil }
func (f *Fake) Candidates(context.Context, string, string) ([]PRRef, error) {
	return f.Cands, f.CandErr
}
func (f *Fake) Teams(context.Context, string) ([]string, error) { return f.MyTeams, f.TeamsErr }
func (f *Fake) PR(_ context.Context, ref PRRef) (PR, error) {
	f.PRCalls++
	return f.PRs[ref.Key()], f.PRErr
}
