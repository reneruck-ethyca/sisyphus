package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/waterboy/internal/review"
)

// JudgeModel scores runs; it only reads text, so it needs no tools.
var JudgeModel = "opus"

// Verdict is judge.json in a run dir.
type Verdict struct {
	Gold []struct {
		ID    string `json:"id"`
		Match string `json:"match"` // found | partial | missed
		Quote string `json:"quote"`
	} `json:"gold"`
	Produced []struct {
		Summary string `json:"summary"`
		Label   string `json:"label"` // gold | plausible_new | false_positive | nit
		GoldID  string `json:"gold_id"`
	} `json:"produced"`
}

const verdictSchema = `{"type":"object","required":["gold","produced"],"properties":{
"gold":{"type":"array","items":{"type":"object","required":["id","match","quote"],"properties":{
  "id":{"type":"string"},"match":{"enum":["found","partial","missed"]},"quote":{"type":"string"}}}},
"produced":{"type":"array","items":{"type":"object","required":["summary","label","gold_id"],"properties":{
  "summary":{"type":"string"},"label":{"enum":["gold","plausible_new","false_positive","nit"]},"gold_id":{"type":"string"}}}}}}`

const judgePrompt = `You are scoring an automated code review against a gold standard: the findings a trusted
review of the same PR, at the same commit, raised. Judge substance, not wording or location
alone: a finding is "found" when the review identifies the same defect and why it matters,
"partial" when it points at the right spot but misses or misstates the problem, "missed"
otherwise. Quote the review's matching text (empty when missed).

Then list every distinct issue the review itself raises (summary body and inline comments)
and label it: "gold" (matches a gold finding; give its id), "false_positive" (wrong, or one
of the listed non-issues), "nit" (true but too minor to matter), or "plausible_new" (a real
issue the gold review didn't raise, as far as you can tell from the text). gold_id is empty
unless the label is "gold".

## Gold findings
%s
## Non-issues (reporting these is a false positive)
%s
## Notes
%s

## The review to score (review.json)
%s
`

// Judge scores every run under out that has no judge.json yet (or all, with
// redo), then writes out/report.md.
func Judge(ctx context.Context, out string, redo bool) error {
	dirs, err := runDirs(out)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "judge.json")); err == nil && !redo {
			continue
		}
		if err := judgeRun(ctx, d); err != nil {
			slog.Error("eval: judging", "run", d, "err", err)
		}
	}
	report, err := Report(out)
	if err != nil {
		return err
	}
	fmt.Print(report)
	return os.WriteFile(filepath.Join(out, "report.md"), []byte(report), 0o644)
}

func runDirs(out string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "eval.json" {
			dirs = append(dirs, filepath.Dir(p))
		}
		return err
	})
	return dirs, err
}

func readMeta(dir string) (runMeta, Case, error) {
	var m runMeta
	b, err := os.ReadFile(filepath.Join(dir, "eval.json"))
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		return m, Case{}, err
	}
	c, err := loadCase(m.Case)
	if err != nil { // moved or deleted: the case as it was run
		c = m.Gold
	}
	return m, c, nil
}

func judgeRun(ctx context.Context, dir string) error {
	_, c, err := readMeta(dir)
	if err != nil {
		return err
	}
	rf, err := review.ReadReviewFile(dir)
	if err != nil {
		return err
	}
	if rf == nil {
		rf = &review.ReviewFile{}
	}
	reviewJSON, _ := json.MarshalIndent(rf, "", "  ")
	gold, _ := json.MarshalIndent(c.Findings, "", "  ")
	non, _ := json.MarshalIndent(c.NonIssues, "", "  ")
	prompt := fmt.Sprintf(judgePrompt, gold, non, c.Notes, reviewJSON)

	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", JudgeModel, "--output-format", "json",
		"--json-schema", verdictSchema, "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands")
	cmd.Dir, cmd.Stdin = os.TempDir(), strings.NewReader(prompt) // no project CLAUDE.md
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("claude: %w: %s", err, stderr.String())
	}
	var res struct {
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	if res.IsError || len(res.Structured) == 0 {
		return fmt.Errorf("judge gave no verdict: %s", res.Result)
	}
	return os.WriteFile(filepath.Join(dir, "judge.json"), res.Structured, 0o644)
}

// Score is one judged run.
type Score struct {
	Case, Run     string
	Recall        float64 // found = 1, partial = ½, weighted must 3, should 2, nice 1
	MustRecall    float64 // the same over must findings; -1 = the case has none
	FalsePos, New int
	Verdict       string // the review's event
	VerdictOK     bool
	Reproduced    int // findings the review says it reproduced
	Cost          float64
	Turns         int
	Duration      time.Duration
	Leaked        []string // later commits the run's log mentions
	Ended         string   // the result line's subtype; "" = Claude never finished
	NewFindings   []string
}

var weights = map[string]float64{"must": 3, "should": 2, "nice": 1}

func score(c Case, v Verdict) (recall, must float64) {
	match := map[string]float64{}
	for _, g := range v.Gold {
		match[g.ID] = map[string]float64{"found": 1, "partial": 0.5}[g.Match]
	}
	var got, total, mGot, mTotal float64
	for _, f := range c.Findings {
		w := weights[f.Weight]
		if w == 0 {
			w = weights["should"]
		}
		got, total = got+w*match[f.ID], total+w
		if f.Weight == "must" {
			mGot, mTotal = mGot+match[f.ID], mTotal+1
		}
	}
	recall, must = 1, -1
	if total > 0 {
		recall = got / total
	}
	if mTotal > 0 {
		must = mGot / mTotal
	}
	return recall, must
}

func scoreRun(dir string) (Score, error) {
	m, c, err := readMeta(dir)
	if err != nil {
		return Score{}, err
	}
	s := Score{Case: filepath.Base(c.File), Run: filepath.Base(dir)}
	var v Verdict
	b, err := os.ReadFile(filepath.Join(dir, "judge.json"))
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	if err != nil {
		return s, fmt.Errorf("not judged: %w", err)
	}
	s.Recall, s.MustRecall = score(c, v)
	for _, p := range v.Produced {
		switch p.Label {
		case "false_positive":
			s.FalsePos++
		case "plausible_new":
			s.New++
			s.NewFindings = append(s.NewFindings, p.Summary)
		}
	}
	if rf, _ := review.ReadReviewFile(dir); rf != nil {
		s.Verdict = rf.Event
		text := rf.Body
		for _, cm := range rf.Comments {
			text += "\n" + cm.Body
		}
		text = strings.ToLower(text)
		s.Reproduced = strings.Count(text, "reproduced") - strings.Count(text, "not reproduced")
	}
	s.VerdictOK = s.Verdict == c.Verdict
	s.Ended, s.Cost, s.Turns, s.Duration, s.Leaked = readLog(filepath.Join(dir, "log"), m.Leaks)
	return s, nil
}

// readLog takes the result line's usage from a run's log and the leaked
// commits it mentions anywhere (tool output included).
func readLog(file string, leaks []string) (ended string, cost float64, turns int, d time.Duration, leaked []string) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	found := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, l := range leaks {
			if len(l) >= 10 && strings.Contains(line, l[:10]) {
				found[l[:10]] = true
			}
		}
		var r struct {
			Type     string  `json:"type"`
			Subtype  string  `json:"subtype"`
			Cost     float64 `json:"total_cost_usd"`
			Turns    int     `json:"num_turns"`
			Duration int64   `json:"duration_ms"`
		}
		if strings.Contains(line, `"type":"result"`) && json.Unmarshal([]byte(line), &r) == nil && r.Type == "result" {
			ended, cost, turns, d = r.Subtype, r.Cost, r.Turns, time.Duration(r.Duration)*time.Millisecond
		}
	}
	for l := range found {
		leaked = append(leaked, l)
	}
	slices.Sort(leaked)
	return
}

// Report is a markdown table of every judged run under out, by case.
func Report(out string) (string, error) {
	dirs, err := runDirs(out)
	if err != nil {
		return "", err
	}
	var scores []Score
	var sb strings.Builder
	for _, d := range dirs {
		s, err := scoreRun(d)
		if err != nil {
			fmt.Fprintf(&sb, "- %s: %v\n", d, err)
			continue
		}
		scores = append(scores, s)
	}
	slices.SortFunc(scores, func(a, b Score) int { return strings.Compare(a.Case+a.Run, b.Case+b.Run) })
	var table strings.Builder
	fmt.Fprintf(&table, "# Eval %s\n\n", out)
	table.WriteString("| case | run | recall | must | false pos | new | verdict | reproduced | cost | turns | min | ended | leaked |\n")
	table.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range scores {
		must := "–"
		if s.MustRecall >= 0 {
			must = fmt.Sprintf("%.2f", s.MustRecall)
		}
		ok := "✗"
		if s.VerdictOK {
			ok = "✓"
		}
		fmt.Fprintf(&table, "| %s | %s | %.2f | %s | %d | %d | %s %s | %d | $%.2f | %d | %.0f | %s | %s |\n",
			s.Case, s.Run, s.Recall, must, s.FalsePos, s.New, ok, s.Verdict, s.Reproduced, s.Cost, s.Turns,
			s.Duration.Minutes(), s.Ended, strings.Join(s.Leaked, " "))
	}
	byCase := map[string][]Score{}
	for _, s := range scores {
		byCase[s.Case] = append(byCase[s.Case], s)
	}
	table.WriteString("\n## Mean per case\n\n| case | runs | recall | false pos | verdict ok | cost | min |\n|---|---|---|---|---|---|---|\n")
	for _, name := range slices.Sorted(maps.Keys(byCase)) {
		ss := byCase[name]
		var r, fp, ok, cost, min float64
		for _, s := range ss {
			r, fp, cost, min = r+s.Recall, fp+float64(s.FalsePos), cost+s.Cost, min+s.Duration.Minutes()
			if s.VerdictOK {
				ok++
			}
		}
		n := float64(len(ss))
		fmt.Fprintf(&table, "| %s | %d | %.2f | %.1f | %.0f/%d | $%.2f | %.0f |\n", name, len(ss), r/n, fp/n, ok, len(ss), cost/n, min/n)
	}
	table.WriteString("\n## Plausible new findings (label them in the case: findings or non_issues)\n\n")
	for _, s := range scores {
		for _, f := range s.NewFindings {
			fmt.Fprintf(&table, "- %s %s: %s\n", s.Case, s.Run, f)
		}
	}
	if sb.Len() > 0 {
		table.WriteString("\n## Not scored\n\n" + sb.String())
	}
	return table.String(), nil
}
