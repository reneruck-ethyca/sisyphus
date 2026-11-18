package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Check is one CI check on a PR's head: a check run or a commit status.
type Check struct {
	Name       string
	Workflow   string // "" for a commit status
	Status     string // QUEUED | IN_PROGRESS | COMPLETED
	Conclusion string // SUCCESS | FAILURE | CANCELLED | SKIPPED | NEUTRAL | TIMED_OUT | …; "" while running
	URL        string
}

// SnapReview is a review as PRView reports it, with the commit it covers.
type SnapReview struct {
	User, State, Body, CommitSHA string
	Bot                          bool
	SubmittedAt                  time.Time
}

// QueueRemoval is one time GitHub took the PR out of the merge queue. Reason is
// GitHub's free-form String, not an enum, so match it loosely.
type QueueRemoval struct {
	At            time.Time
	Reason, Actor string // Actor is a login; "" if none
	ActorBot      bool
}

// PRSnapshot is the PR state one PRView reads. Review threads and failed
// job logs come from elsewhere.
type PRSnapshot struct {
	PRRef
	URL, Body, Author              string
	State                          string // OPEN | CLOSED | MERGED
	Draft                          bool
	HeadSHA, HeadRef, BaseRef      string
	Mergeable, MergeState          string // GitHub's MERGEABLE|CONFLICTING|UNKNOWN; BEHIND|BLOCKED|CLEAN|DIRTY|DRAFT|UNSTABLE|…
	ReviewDecision                 string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
	Reviews                        []SnapReview
	RequestedUsers, RequestedTeams []string
	Checks                         []Check
	MergedAt, ClosedAt             time.Time // zero while unset
	MergedBy                       string    // login; "" while unmerged
	Additions, Deletions           int
	Files                          []string
	NodeID                         string // GraphQL id, for Enqueue and EnableAutoMerge
	AutoMergeAllowed               bool   // the repo allows auto-merge
	MergeQueue, InQueue, AutoMerge bool   // base has a merge queue; the PR is in it; auto-merge is on
	QueueRemovals                  []QueueRemoval
}

// checkNodes is one page of a head commit's checks: check runs and commit statuses.
const checkNodes = `pageInfo{hasNextPage endCursor} nodes{__typename
... on CheckRun{name status conclusion detailsUrl checkSuite{workflowRun{workflow{name}}}}
... on StatusContext{context state targetUrl}}`

const prViewQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){autoMergeAllowed pullRequest(number:$number){
id isMergeQueueEnabled isInMergeQueue autoMergeRequest{enabledAt}
timelineItems(last:20,itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{... on RemovedFromMergeQueueEvent{createdAt reason actor{login __typename}}}}
number url state isDraft headRefOid headRefName baseRefName mergeable mergeStateStatus reviewDecision body
author{login __typename} mergedAt closedAt mergedBy{login} additions deletions
reviews(last:100){nodes{author{login __typename} state body submittedAt commit{oid}}}
reviewRequests(first:100){nodes{requestedReviewer{__typename ... on User{login} ... on Bot{login} ... on Mannequin{login} ... on Team{slug}}}}
commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100){` + checkNodes + `}}}}}
files(first:100){nodes{path}}
}}}`

// checksQuery reads the checks after cursor, for a PR with more than one page of them.
const checksQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String!){repository(owner:$owner,name:$name){pullRequest(number:$number){
commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100,after:$after){` + checkNodes + `}}}}}
}}}`

// PRView reads the PR's snapshot with one GraphQL query, and one more per
// further 100 checks: a failing check must never be missed.
// ponytail: first 100 files and review requests, last 100 reviews; page when a PR outgrows them.
func (a *API) PRView(ctx context.Context, ref PRRef) (PRSnapshot, error) {
	owner, name, err := splitRepo(ref.Repo)
	if err != nil {
		return PRSnapshot{}, err
	}
	vars := map[string]any{"owner": owner, "name": name, "number": ref.Number}
	var data json.RawMessage
	if err := a.graphql(ctx, prViewQuery, vars, &data); err != nil {
		return PRSnapshot{}, err
	}
	s, after, err := parsePRView(data)
	for err == nil && after != "" {
		vars["after"] = after
		if err = a.graphql(ctx, checksQuery, vars, &data); err != nil {
			break
		}
		var more PRSnapshot
		more, after, err = parsePRView(data)
		s.Checks = append(s.Checks, more.Checks...)
	}
	s.PRRef = ref
	return s, err
}

type gqlActor struct {
	Login string `json:"login"`
	Type  string `json:"__typename"`
}

// parsePRView reads prViewQuery's or checksQuery's data. after is the cursor
// of the next page of checks, "" when there is none.
func parsePRView(b []byte) (s PRSnapshot, after string, err error) {
	type nodes[T any] struct {
		Nodes []T `json:"nodes"`
	}
	var v struct {
		Repository struct {
			AutoMergeAllowed bool `json:"autoMergeAllowed"`
			PullRequest      struct {
				ID                  string `json:"id"`
				IsMergeQueueEnabled bool   `json:"isMergeQueueEnabled"`
				IsInMergeQueue      bool   `json:"isInMergeQueue"`
				AutoMergeRequest    *struct {
					EnabledAt time.Time `json:"enabledAt"`
				} `json:"autoMergeRequest"`
				TimelineItems nodes[struct {
					CreatedAt time.Time `json:"createdAt"`
					Reason    string    `json:"reason"`
					Actor     *gqlActor `json:"actor"`
				}] `json:"timelineItems"`
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
				Author           *gqlActor `json:"author"`
				MergedAt         time.Time `json:"mergedAt"`
				ClosedAt         time.Time `json:"closedAt"`
				MergedBy         *gqlActor `json:"mergedBy"`
				Additions        int       `json:"additions"`
				Deletions        int       `json:"deletions"`
				Reviews          nodes[struct {
					Author      *gqlActor `json:"author"`
					State       string    `json:"state"`
					Body        string    `json:"body"`
					SubmittedAt time.Time `json:"submittedAt"`
					Commit      *struct {
						OID string `json:"oid"`
					} `json:"commit"`
				}] `json:"reviews"`
				ReviewRequests nodes[struct {
					Reviewer *struct {
						Type  string `json:"__typename"`
						Login string `json:"login"`
						Slug  string `json:"slug"`
					} `json:"requestedReviewer"`
				}] `json:"reviewRequests"`
				Commits nodes[struct {
					Commit struct {
						Rollup *struct {
							Contexts struct {
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
								Nodes []struct {
									Type       string `json:"__typename"`
									Name       string `json:"name"`
									Status     string `json:"status"`
									Conclusion string `json:"conclusion"`
									DetailsURL string `json:"detailsUrl"`
									CheckSuite *struct {
										WorkflowRun *struct {
											Workflow struct {
												Name string `json:"name"`
											} `json:"workflow"`
										} `json:"workflowRun"`
									} `json:"checkSuite"`
									Context   string `json:"context"` // StatusContext
									State     string `json:"state"`
									TargetURL string `json:"targetUrl"`
								} `json:"nodes"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				}] `json:"commits"`
				Files nodes[struct {
					Path string `json:"path"`
				}] `json:"files"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return PRSnapshot{}, "", fmt.Errorf("parse pr view: %w", err)
	}
	p := v.Repository.PullRequest
	login := func(a *gqlActor) string {
		if a == nil {
			return ""
		}
		return a.Login
	}
	s = PRSnapshot{
		PRRef: PRRef{Number: p.Number}, URL: p.URL, Body: p.Body, Author: login(p.Author), State: p.State, Draft: p.IsDraft,
		HeadSHA: p.HeadRefOid, HeadRef: p.HeadRefName, BaseRef: p.BaseRefName,
		Mergeable: p.Mergeable, MergeState: p.MergeStateStatus, ReviewDecision: p.ReviewDecision,
		MergedAt: p.MergedAt, ClosedAt: p.ClosedAt, MergedBy: login(p.MergedBy), Additions: p.Additions, Deletions: p.Deletions,
		NodeID: p.ID, AutoMergeAllowed: v.Repository.AutoMergeAllowed, MergeQueue: p.IsMergeQueueEnabled,
		InQueue: p.IsInMergeQueue, AutoMerge: p.AutoMergeRequest != nil,
	}
	for _, e := range p.TimelineItems.Nodes {
		s.QueueRemovals = append(s.QueueRemovals, QueueRemoval{At: e.CreatedAt, Reason: e.Reason, Actor: login(e.Actor),
			ActorBot: e.Actor != nil && (e.Actor.Type == "Bot" || strings.HasSuffix(e.Actor.Login, "[bot]"))})
	}
	for _, r := range p.Reviews.Nodes {
		sr := SnapReview{User: login(r.Author), Bot: r.Author != nil && r.Author.Type == "Bot", State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt}
		if r.Commit != nil {
			sr.CommitSHA = r.Commit.OID
		}
		s.Reviews = append(s.Reviews, sr)
	}
	for _, r := range p.ReviewRequests.Nodes {
		switch {
		case r.Reviewer == nil:
		case r.Reviewer.Type == "Team":
			s.RequestedTeams = append(s.RequestedTeams, r.Reviewer.Slug)
		default:
			s.RequestedUsers = append(s.RequestedUsers, r.Reviewer.Login)
		}
	}
	for _, cm := range p.Commits.Nodes {
		if cm.Commit.Rollup == nil {
			continue
		}
		if pi := cm.Commit.Rollup.Contexts.PageInfo; pi.HasNextPage {
			after = pi.EndCursor
		}
		for _, c := range cm.Commit.Rollup.Contexts.Nodes {
			if c.Type == "StatusContext" { // a commit status: PENDING | SUCCESS | FAILURE | ERROR | EXPECTED
				ck := Check{Name: c.Context, Status: "COMPLETED", Conclusion: c.State, URL: c.TargetURL}
				if c.State == "PENDING" || c.State == "EXPECTED" {
					ck.Status, ck.Conclusion = "IN_PROGRESS", ""
				}
				s.Checks = append(s.Checks, ck)
				continue
			}
			ck := Check{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion, URL: c.DetailsURL}
			if c.CheckSuite != nil && c.CheckSuite.WorkflowRun != nil {
				ck.Workflow = c.CheckSuite.WorkflowRun.Workflow.Name
			}
			s.Checks = append(s.Checks, ck)
		}
	}
	for _, f := range p.Files.Nodes {
		s.Files = append(s.Files, f.Path)
	}
	return s, after, nil
}
