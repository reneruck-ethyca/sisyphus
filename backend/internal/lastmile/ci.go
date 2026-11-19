package lastmile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/reneruck/sisyphus/internal/github"
)

// logTailLines is how much of a failed job's log the snapshot keeps.
const logTailLines = 150

// Failure is one failed check, with what an agent needs to fix it.
type Failure struct {
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Flake       bool   `json:"flake,omitempty"` // the log matches lastmile.flakes
	RunID       int64  `json:"run_id,omitempty"`
	JobID       int64  `json:"job_id,omitempty"`
	Log         string `json:"log,omitempty"` // the last logTailLines lines of the failed steps
}

var checkURL = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

// parseCheckURL reads the Actions run and job ids from a check's details URL;
// ok is false for checks that aren't Actions jobs.
func parseCheckURL(u string) (runID, jobID int64, ok bool) {
	m := checkURL.FindStringSubmatch(u)
	if m == nil {
		return 0, 0, false
	}
	run, err1 := strconv.ParseInt(m[1], 10, 64)
	job, err2 := strconv.ParseInt(m[2], 10, 64)
	return run, job, err1 == nil && err2 == nil
}

// tail keeps the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\d[T ][\d:.]+Z?`)
	reTempPath  = regexp.MustCompile(`(?:/tmp|/var/folders|/private/var|/home/runner/work/_temp)/\S*`)
	reHex       = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)
	reNumber    = regexp.MustCompile(`\d+`)
	reErrorLine = regexp.MustCompile(`(?i)error|fail|exception|panic|assert|traceback|fatal|✗|✘`)
)

// fingerprint names a failure: the job, and a hash of its error lines with what
// varies between runs (timestamps, temp paths, SHAs, numbers) removed. Without
// error-looking lines the last lines stand in.
// ponytail: line-based and keyword-driven; a stack trace with changing frames hashes
// differently per run, so a repeat may go unseen until max_fix_rounds catches it.
func fingerprint(job, log string) string {
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if reErrorLine.MatchString(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		lines = strings.Split(tail(log, 10), "\n")
	}
	seen := map[string]bool{}
	h := sha256.New()
	for _, l := range lines {
		l = reTimestamp.ReplaceAllString(l, "")
		l = reTempPath.ReplaceAllString(l, "PATH")
		l = reHex.ReplaceAllString(l, "SHA")
		l = strings.Join(strings.Fields(reNumber.ReplaceAllString(l, "N")), " ")
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		h.Write([]byte(l + "\n"))
	}
	return job + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// failures reads the failed checks of s with their log tails. A failure seen
// at the last tick (same check, same URL) keeps its log: no second fetch.
func (m *Manager) failures(ctx context.Context, h Handoff, checks []github.Check) []Failure {
	var old []Failure
	if h.Snapshot != nil {
		old = h.Snapshot.Failures
	}
	var flakes []*regexp.Regexp
	for _, f := range m.cfg.LastMile.Flakes {
		if re, err := regexp.Compile(f); err == nil { // validated at load
			flakes = append(flakes, re)
		}
	}
	var out []Failure
	for _, c := range checks {
		if c.Status != "COMPLETED" || passing(c.Conclusion) {
			continue
		}
		f := Failure{Name: c.Name, URL: c.URL}
		if i := indexFailure(old, c); i >= 0 {
			f = old[i]
		} else if run, job, ok := parseCheckURL(c.URL); ok {
			f.RunID, f.JobID = run, job
			log, err := m.GH.RunLog(ctx, h.Repo, run, job)
			if err != nil {
				slog.Warn("lastmile job log failed", "id", h.ID, "check", c.Name, "err", err)
			}
			f.Log = tail(log, logTailLines)
			f.Fingerprint = fingerprint(c.Name, f.Log)
		}
		if f.Fingerprint == "" {
			f.Fingerprint = fingerprint(c.Name, "")
		}
		f.Flake = c.Conclusion == "CANCELLED" // a newer run or the runner stopped it: nothing for an agent to fix
		for _, re := range flakes {
			if f.Log != "" && re.MatchString(f.Log) {
				f.Flake = true
			}
		}
		out = append(out, f)
	}
	return out
}

func indexFailure(old []Failure, c github.Check) int {
	for i, f := range old {
		if f.Name == c.Name && f.URL == c.URL {
			return i
		}
	}
	return -1
}

// rerunFlakes reruns the failed jobs of the flaky failures, once per run, while
// the head's budget lasts. It says whether it did.
func (m *Manager) rerunFlakes(ctx context.Context, h *Handoff, s Snapshot) (bool, error) {
	var runs []int64
	for _, f := range s.Failures {
		if f.Flake && f.RunID != 0 && !slices.Contains(runs, f.RunID) {
			runs = append(runs, f.RunID)
		}
	}
	if len(runs) == 0 {
		return false, nil
	}
	if h.FlakeHead != s.Head {
		h.FlakeHead, h.FlakeReruns = s.Head, 0
	}
	if h.FlakeReruns >= m.cfg.LastMile.FlakeReruns {
		return false, nil
	}
	for _, run := range runs {
		if err := m.GH.RerunFailed(ctx, h.Repo, run); err != nil {
			return false, fmt.Errorf("rerun flaky run %d: %w", run, err)
		}
	}
	h.FlakeReruns++
	h.FlakeTotal++
	slog.Info("lastmile flake rerun", "id", h.ID, "runs", runs, "n", h.FlakeReruns)
	return true, nil
}
