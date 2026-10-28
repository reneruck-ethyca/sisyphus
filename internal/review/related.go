package review

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// ponytail: any KEY-123 shape counts, so a branch like release-2 searches for RELEASE-2 too; it
// just finds nothing. Restrict to configured Jira projects if that ever picks a wrong PR.
var ticketKey = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)

// ticketKeys are the Jira keys in a PR's title and branch, without repeats. The
// body is left out: it names epics ("Part of ENG-1"), which match unrelated PRs.
func ticketKeys(title, branch string) []string {
	var keys []string
	for _, k := range ticketKey.FindAllString(title+" "+strings.ToUpper(branch), -1) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func relatedDir(run Run) string { return filepath.Join(run.Dir, "related") }

// dropRelated deletes a run's related checkout once the container is done with it.
func dropRelated(run Run) { _ = os.RemoveAll(relatedDir(run)) }

// checkoutRelated exports rel's pick for the run into the run dir and says what
// it is, for the prompt. git archive takes tracked files only, so nothing ignored
// in the clone (.env files) reaches the container, and no git metadata either.
func (r *Reviewer) checkoutRelated(ctx context.Context, run Run, rel *config.Related) (string, error) {
	if rel == nil {
		return "", nil
	}
	sha, why, err := r.relatedRef(ctx, run.Job.PR, rel)
	if err != nil {
		return "", fmt.Errorf("related %s: %w", rel.Repo, err)
	}
	dir := relatedDir(run)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := proc.Run(ctx, rel.Clone, "bash", "-c", `set -o pipefail; git archive "$1" | tar -x -C "$2"`, "_", sha, dir); err != nil {
		return "", fmt.Errorf("related %s: %w", rel.Repo, err)
	}
	return fmt.Sprintf("%s is checked out read-only at %s, at %.12s: %s. It has no git metadata.",
		rel.Repo, rel.Target, sha, why), nil
}

// relatedRef picks the commit of rel.Repo to check out: the head of the most
// recently updated open PR there naming one of tr's tickets, else rel.Base.
func (r *Reviewer) relatedRef(ctx context.Context, tr tracker.Tracked, rel *config.Related) (sha, why string, err error) {
	keys := ticketKeys(tr.Title, tr.HeadRef)
	var found []github.OpenPR
	for _, k := range keys {
		if r.GitHub == nil {
			break
		}
		prs, err := r.GitHub.OpenPRs(ctx, rel.Repo, k)
		if err != nil {
			return "", "", err
		}
		for _, p := range prs {
			if !slices.ContainsFunc(found, func(f github.OpenPR) bool { return f.Number == p.Number }) {
				found = append(found, p)
			}
		}
	}
	tickets := strings.Join(keys, ", ")
	if len(found) > 0 {
		slices.SortFunc(found, func(a, b github.OpenPR) int { return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), a.Number-b.Number) })
		p := found[0]
		if _, err := proc.Run(ctx, rel.Clone, "git", "fetch", "--quiet", "--no-tags", "origin", fmt.Sprintf("refs/pull/%d/head", p.Number)); err != nil {
			return "", "", err
		}
		why = fmt.Sprintf("the head of %s#%d (%q), the open PR there for %s", rel.Repo, p.Number, p.Title, tickets)
		if len(found) > 1 {
			var others []string
			for _, o := range found[1:] {
				others = append(others, fmt.Sprintf("#%d (%q)", o.Number, o.Title))
			}
			why += "; other open PRs there for them: " + strings.Join(others, ", ")
		}
		return p.HeadSHA, why, nil
	}
	if _, err := proc.Run(ctx, rel.Clone, "git", "fetch", "--quiet", "--no-tags", "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", rel.Base, rel.Base)); err != nil {
		return "", "", err
	}
	if sha, err = proc.Run(ctx, rel.Clone, "git", "rev-parse", "refs/remotes/origin/"+rel.Base); err != nil {
		return "", "", err
	}
	if len(keys) == 0 {
		return sha, rel.Base + ", since this PR names no ticket", nil
	}
	return sha, fmt.Sprintf("%s, since no open PR there names %s", rel.Base, tickets), nil
}
