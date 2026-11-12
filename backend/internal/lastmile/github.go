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
	Comment(ctx context.Context, ref github.PRRef, body string) (int64, error)
	Reply(ctx context.Context, ref github.PRRef, commentID int64, body string) (int64, error)
	Inbox(ctx context.Context, ref github.PRRef) (github.Inbox, error)
	CreateLabel(ctx context.Context, repo, name, color, description string) error
	FindPR(ctx context.Context, repo, branch string) (github.PRRef, bool, error)
	RunLog(ctx context.Context, repo string, runID, jobID int64) (string, error)
	RerunFailed(ctx context.Context, repo string, runID int64) error
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
	Replies   []FakeReply // ReplyToComment calls
	InboxData github.Inbox
	nextID    int64
	Labels    []string
	Existing  github.PRRef     // the open PR FindPR reports; zero = none
	Logs      map[int64]string // RunLog by job id
	Reruns    []int64          // runs RerunFailed was asked for
	Err       error            // returned by every call
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

// FakeReply is an inline reply the fake got.
type FakeReply struct {
	To   int64
	Body string
	ID   int64
}

func (f *FakeGitHub) id() int64 {
	f.nextID++
	return 9000 + f.nextID
}

func (f *FakeGitHub) Comment(_ context.Context, _ github.PRRef, body string) (int64, error) {
	f.Comments = append(f.Comments, body)
	return f.id(), f.Err
}

func (f *FakeGitHub) Reply(_ context.Context, _ github.PRRef, to int64, body string) (int64, error) {
	r := FakeReply{To: to, Body: body, ID: f.id()}
	f.Replies = append(f.Replies, r)
	return r.ID, f.Err
}

func (f *FakeGitHub) Inbox(context.Context, github.PRRef) (github.Inbox, error) {
	return f.InboxData, f.Err
}

func (f *FakeGitHub) CreateLabel(_ context.Context, _, name, _, _ string) error {
	f.Labels = append(f.Labels, name)
	return f.Err
}

func (f *FakeGitHub) FindPR(context.Context, string, string) (github.PRRef, bool, error) {
	return f.Existing, f.Existing.Number != 0, f.Err
}

func (f *FakeGitHub) RunLog(_ context.Context, _ string, _, jobID int64) (string, error) {
	return f.Logs[jobID], f.Err
}

func (f *FakeGitHub) RerunFailed(_ context.Context, _ string, runID int64) error {
	f.Reruns = append(f.Reruns, runID)
	return f.Err
}
