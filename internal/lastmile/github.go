package lastmile

import (
	"context"

	"github.com/reneruck/sisyphus/internal/github"
)

// GitHub is what TLM needs from GitHub; github.CLI implements it.
type GitHub interface {
	PRView(ctx context.Context, ref github.PRRef) (github.PRSnapshot, error)
	CreatePR(ctx context.Context, repo string, pr github.NewPR) (github.PRRef, error)
	MarkReady(ctx context.Context, ref github.PRRef) error
	EditPR(ctx context.Context, ref github.PRRef, e github.PREdit) error
	Comment(ctx context.Context, ref github.PRRef, body string) error
	CreateLabel(ctx context.Context, repo, name, color, description string) error
	FindPR(ctx context.Context, repo, branch string) (github.PRRef, bool, error)
}

var _ GitHub = github.CLI{}

// FakeGitHub is an in-memory GitHub for tests: it records the calls it gets.
type FakeGitHub struct {
	Snapshots map[string]github.PRSnapshot // by PRRef.Key()
	Created   []github.NewPR
	NextPR    github.PRRef // what CreatePR returns
	Edits     []github.PREdit
	Ready     []github.PRRef
	Comments  []string
	Labels    []string
	Existing  github.PRRef // the open PR FindPR reports; zero = none
	Err       error        // returned by every call
}

func (f *FakeGitHub) PRView(_ context.Context, ref github.PRRef) (github.PRSnapshot, error) {
	return f.Snapshots[ref.Key()], f.Err
}

func (f *FakeGitHub) CreatePR(_ context.Context, _ string, pr github.NewPR) (github.PRRef, error) {
	f.Created = append(f.Created, pr)
	return f.NextPR, f.Err
}

func (f *FakeGitHub) MarkReady(_ context.Context, ref github.PRRef) error {
	f.Ready = append(f.Ready, ref)
	return f.Err
}

func (f *FakeGitHub) EditPR(_ context.Context, _ github.PRRef, e github.PREdit) error {
	f.Edits = append(f.Edits, e)
	return f.Err
}

func (f *FakeGitHub) Comment(_ context.Context, _ github.PRRef, body string) error {
	f.Comments = append(f.Comments, body)
	return f.Err
}

func (f *FakeGitHub) CreateLabel(_ context.Context, _, name, _, _ string) error {
	f.Labels = append(f.Labels, name)
	return f.Err
}

func (f *FakeGitHub) FindPR(context.Context, string, string) (github.PRRef, bool, error) {
	return f.Existing, f.Existing.Number != 0, f.Err
}
