package lastmile

import (
	"cmp"
	"context"

	"github.com/reneruck/sisyphus/internal/github"
)

// GitHub is what TLM needs from GitHub; github.API implements it.
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
	MergeSquash(ctx context.Context, ref github.PRRef, head string) error
	MergeAsync(ctx context.Context, ref github.PRRef, head string, queue bool) error
	BranchProtected(ctx context.Context, repo, branch string) (bool, error)
	Enqueue(ctx context.Context, nodeID string) error
	Dequeue(ctx context.Context, nodeID string) error
	EnableAutoMerge(ctx context.Context, nodeID string) error
	DisableAutoMerge(ctx context.Context, nodeID string) error
	QueueFailures(ctx context.Context, repo, base string, number int) ([]github.Check, error)
}

var _ GitHub = (*github.API)(nil)

// FakeGitHub is an in-memory GitHub for tests: it records the calls it gets.
type FakeGitHub struct {
	Snapshots    map[string]github.PRSnapshot // by PRRef.Key()
	Created      []github.NewPR
	NextPR       github.PRRef // what CreatePR returns
	Edits        []github.PREdit
	Ready        []github.PRRef
	Comments     []string
	Replies      []FakeReply // Reply calls
	InboxData    github.Inbox
	nextID       int64
	Labels       []string
	Existing     github.PRRef     // the open PR FindPR reports; zero = none
	Logs         map[int64]string // RunLog by job id
	Reruns       []int64          // runs RerunFailed was asked for
	Merged       []github.PRRef   // MergeSquash calls
	AsyncMerged  []github.PRRef   // MergeAsync calls
	Found        map[string]int   // FindPR by branch, over Existing
	Protected    map[string]bool  // BranchProtected by branch
	Enqueued     []string         // node ids Enqueue was asked for
	Dequeued     []string         // node ids Dequeue was asked for
	AutoMerged   []string         // node ids EnableAutoMerge was asked for
	AutoMergeOff []string         // node ids DisableAutoMerge was asked for
	AutoMergeErr error            // returned by EnableAutoMerge
	MergeErr     error            // returned by MergeSquash and Enqueue
	QueueChecks  []github.Check   // what QueueFailures returns
	Err          error            // returned by every call
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

func (f *FakeGitHub) FindPR(_ context.Context, repo, branch string) (github.PRRef, bool, error) {
	if n, ok := f.Found[branch]; ok {
		return github.PRRef{Repo: repo, Number: n}, true, f.Err
	}
	return f.Existing, f.Existing.Number != 0, f.Err
}

func (f *FakeGitHub) RunLog(_ context.Context, _ string, _, jobID int64) (string, error) {
	return f.Logs[jobID], f.Err
}

func (f *FakeGitHub) RerunFailed(_ context.Context, _ string, runID int64) error {
	f.Reruns = append(f.Reruns, runID)
	return f.Err
}

func (f *FakeGitHub) MergeSquash(_ context.Context, ref github.PRRef, _ string) error {
	f.Merged = append(f.Merged, ref)
	return cmp.Or(f.MergeErr, f.Err)
}

func (f *FakeGitHub) MergeAsync(_ context.Context, ref github.PRRef, _ string, _ bool) error {
	f.AsyncMerged = append(f.AsyncMerged, ref)
	return cmp.Or(f.MergeErr, f.Err)
}

func (f *FakeGitHub) BranchProtected(_ context.Context, _, branch string) (bool, error) {
	return f.Protected[branch], f.Err
}

func (f *FakeGitHub) Enqueue(_ context.Context, nodeID string) error {
	f.Enqueued = append(f.Enqueued, nodeID)
	return cmp.Or(f.MergeErr, f.Err)
}

func (f *FakeGitHub) Dequeue(_ context.Context, nodeID string) error {
	f.Dequeued = append(f.Dequeued, nodeID)
	return f.Err
}

func (f *FakeGitHub) EnableAutoMerge(_ context.Context, nodeID string) error {
	f.AutoMerged = append(f.AutoMerged, nodeID)
	return cmp.Or(f.AutoMergeErr, f.Err)
}

func (f *FakeGitHub) DisableAutoMerge(_ context.Context, nodeID string) error {
	f.AutoMergeOff = append(f.AutoMergeOff, nodeID)
	return f.Err
}

func (f *FakeGitHub) QueueFailures(context.Context, string, string, int) ([]github.Check, error) {
	return f.QueueChecks, f.Err
}
