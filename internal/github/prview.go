package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/proc"
)

// prViewFields is what `gh pr view --json` is asked for: one call builds most of a PR snapshot.
const prViewFields = "number,url,state,isDraft,headRefOid,headRefName,baseRefName,mergeable,mergeStateStatus," +
	"reviewDecision,reviews,reviewRequests,body,author,statusCheckRollup,mergedAt,closedAt,additions,deletions,files"

// Check is one CI check on a PR's head: a check run or a commit status.
type Check struct {
	Name       string
	Workflow   string // "" for a commit status
	Status     string // QUEUED | IN_PROGRESS | COMPLETED
	Conclusion string // SUCCESS | FAILURE | CANCELLED | SKIPPED | NEUTRAL | TIMED_OUT | …; "" while running
	URL        string
}

// SnapReview is a review as `gh pr view` reports it, with the commit it covers.
type SnapReview struct {
	User, State, Body, CommitSHA string
	Bot                          bool
	SubmittedAt                  time.Time
}

// PRSnapshot is the PR state one `gh pr view` shows. Review threads and failed
// job logs come from elsewhere.
type PRSnapshot struct {
	PRRef
	URL, Body, Author              string
	State                          string // OPEN | CLOSED | MERGED
	Draft                          bool
	HeadSHA, HeadRef, BaseRef      string
	Mergeable, MergeState          string // gh's MERGEABLE|CONFLICTING|UNKNOWN; BEHIND|BLOCKED|CLEAN|DIRTY|DRAFT|UNSTABLE|…
	ReviewDecision                 string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
	Reviews                        []SnapReview
	RequestedUsers, RequestedTeams []string
	Checks                         []Check
	MergedAt, ClosedAt             time.Time // zero while unset
	Additions, Deletions           int
	Files                          []string
}

// PRView reads the PR's snapshot with one `gh pr view`.
func (CLI) PRView(ctx context.Context, ref PRRef) (PRSnapshot, error) {
	out, err := proc.Run(ctx, "", "gh", "pr", "view", strconv.Itoa(ref.Number), "--repo", ref.Repo, "--json", prViewFields)
	if err != nil {
		return PRSnapshot{}, err
	}
	s, err := parsePRView([]byte(out))
	s.PRRef = ref
	return s, err
}

type ghAuthor struct {
	Login string `json:"login"`
	IsBot bool   `json:"is_bot"`
}

func parsePRView(b []byte) (PRSnapshot, error) {
	var v struct {
		Number           int       `json:"number"`
		URL              string    `json:"url"`
		State            string    `json:"state"`
		IsDraft          bool      `json:"isDraft"`
		HeadRefOid       string    `json:"headRefOid"`
		HeadRefName      string    `json:"headRefName"`
		BaseRefName      string    `json:"baseRefName"`
		Mergeable        string    `json:"mergeable"`
		MergeStateStatus string    `json:"mergeStateStatus"`
		ReviewDecision   string    `json:"reviewDecision"`
		Body             string    `json:"body"`
		Author           ghAuthor  `json:"author"`
		MergedAt         time.Time `json:"mergedAt"`
		ClosedAt         time.Time `json:"closedAt"`
		Additions        int       `json:"additions"`
		Deletions        int       `json:"deletions"`
		Reviews          []struct {
			Author      ghAuthor  `json:"author"`
			State       string    `json:"state"`
			Body        string    `json:"body"`
			SubmittedAt time.Time `json:"submittedAt"`
			Commit      struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"reviews"`
		ReviewRequests []struct {
			Type  string `json:"__typename"`
			Login string `json:"login"`
			Slug  string `json:"slug"`
		} `json:"reviewRequests"`
		StatusCheckRollup []struct {
			Type         string `json:"__typename"`
			Name         string `json:"name"`
			WorkflowName string `json:"workflowName"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			DetailsURL   string `json:"detailsUrl"`
			Context      string `json:"context"` // StatusContext
			State        string `json:"state"`
			TargetURL    string `json:"targetUrl"`
		} `json:"statusCheckRollup"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return PRSnapshot{}, fmt.Errorf("parse pr view: %w", err)
	}
	s := PRSnapshot{
		PRRef: PRRef{Number: v.Number}, URL: v.URL, Body: v.Body, Author: v.Author.Login, State: v.State, Draft: v.IsDraft,
		HeadSHA: v.HeadRefOid, HeadRef: v.HeadRefName, BaseRef: v.BaseRefName,
		Mergeable: v.Mergeable, MergeState: v.MergeStateStatus, ReviewDecision: v.ReviewDecision,
		MergedAt: v.MergedAt, ClosedAt: v.ClosedAt, Additions: v.Additions, Deletions: v.Deletions,
	}
	if u, err := url.Parse(v.URL); err == nil { // https://github.com/owner/name/pull/n
		if p := strings.Split(strings.Trim(u.Path, "/"), "/"); len(p) >= 2 {
			s.Repo = p[0] + "/" + p[1]
		}
	}
	for _, r := range v.Reviews {
		s.Reviews = append(s.Reviews, SnapReview{User: r.Author.Login, Bot: r.Author.IsBot, State: r.State,
			Body: r.Body, CommitSHA: r.Commit.OID, SubmittedAt: r.SubmittedAt})
	}
	for _, r := range v.ReviewRequests {
		if r.Type == "Team" {
			s.RequestedTeams = append(s.RequestedTeams, r.Slug)
		} else {
			s.RequestedUsers = append(s.RequestedUsers, r.Login)
		}
	}
	for _, c := range v.StatusCheckRollup {
		if c.Type == "StatusContext" { // a commit status: PENDING | SUCCESS | FAILURE | ERROR | EXPECTED
			ck := Check{Name: c.Context, Status: "COMPLETED", Conclusion: c.State, URL: c.TargetURL}
			if c.State == "PENDING" || c.State == "EXPECTED" {
				ck.Status, ck.Conclusion = "IN_PROGRESS", ""
			}
			s.Checks = append(s.Checks, ck)
			continue
		}
		s.Checks = append(s.Checks, Check{Name: c.Name, Workflow: c.WorkflowName, Status: c.Status, Conclusion: c.Conclusion, URL: c.DetailsURL})
	}
	for _, f := range v.Files {
		s.Files = append(s.Files, f.Path)
	}
	return s, nil
}
