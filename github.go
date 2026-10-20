package main

import (
	"context"
	"fmt"
	"time"
)

// PRRef identifies a PR. Repo is "owner/name".
type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r PRRef) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

type Review struct {
	User        string
	Bot         bool
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	SubmittedAt time.Time
}

type Comment struct {
	User      string
	Bot       bool
	CreatedAt time.Time
}

type PR struct {
	PRRef
	URL, Title, Author string
	State              string // "open" | "closed"
	Merged, Draft      bool
	HeadRef, HeadSHA   string
	HeadRepo           string // "owner/name"; "" if the fork was deleted
	Reviews            []Review
	Comments           []Comment // issue + inline review comments
}

// GitHub is everything waterboy needs from GitHub; swap the gh CLI out here.
type GitHub interface {
	CurrentUser(ctx context.Context) (string, error)
	Candidates(ctx context.Context, org, user string) ([]PRRef, error)
	PR(ctx context.Context, ref PRRef) (PR, error)
}
