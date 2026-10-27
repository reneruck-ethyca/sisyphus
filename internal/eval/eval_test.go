package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilterDiscussion(t *testing.T) {
	cutoff := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	comments := `[{"id":1,"created_at":"2026-06-01T11:59:59Z"},{"id":2,"created_at":"2026-06-01T12:00:00Z"},
		{"id":3,"submitted_at":"2026-05-01T00:00:00Z"},{"id":4,"submitted_at":"2026-07-01T00:00:00Z"}]`
	b, err := filterDiscussion("reviews.json", []byte(comments), cutoff, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var kept []struct{ ID int }
	json.Unmarshal(b, &kept)
	if len(kept) != 2 || kept[0].ID != 1 || kept[1].ID != 3 {
		t.Errorf("kept %s; want ids 1 and 3 (the gold review itself, at the cutoff, is hidden)", b)
	}

	pr := `{"number":7,"title":"T","body":"B","state":"closed","merged":true,"merge_commit_sha":"m3rge",
		"head":{"ref":"feat","sha":"final"},"base":{"ref":"dev","sha":"newbase"}}`
	b, err = filterDiscussion("pr.json", []byte(pr), cutoff, "old", "fork")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"m3rge", "final", "newbase", "merged", "closed"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("pr.json keeps %q:\n%s", leak, b)
		}
	}
	for _, want := range []string{`"sha": "old"`, `"sha": "fork"`, `"ref": "feat"`, `"body": "B"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("pr.json lacks %s:\n%s", want, b)
		}
	}
}

func TestScore(t *testing.T) {
	c := Case{Findings: []Finding{{ID: "a", Weight: "must"}, {ID: "b", Weight: "nice"}, {ID: "c"}}}
	var v Verdict
	json.Unmarshal([]byte(`{"gold":[{"id":"a","match":"partial"},{"id":"b","match":"found"},{"id":"c","match":"missed"}]}`), &v)
	recall, must := score(c, v)
	if want := (3*0.5 + 1) / 6; recall != want || must != 0.5 {
		t.Errorf("score = %v, %v; want %v, 0.5", recall, must, want)
	}
	if _, must := score(Case{Findings: []Finding{{ID: "x", Weight: "nice"}}}, v); must != -1 {
		t.Errorf("no must findings: must = %v, want -1", must)
	}
}

func TestReadLog(t *testing.T) {
	f := filepath.Join(t.TempDir(), "log")
	os.WriteFile(f, []byte(`── setup ──
{"type":"user","message":{"content":[{"type":"tool_result","content":"abcdef0123456789 fix the thing"}]}}
{"subtype":"success","total_cost_usd":1.5,"num_turns":40,"type":"result","duration_ms":120000}
`), 0o644)
	ended, cost, turns, d, leaked := readLog(f, []string{"abcdef0123456789aaaa", "ffffffffffffffff"})
	if ended != "success" || cost != 1.5 || turns != 40 || d != 2*time.Minute {
		t.Errorf("usage = %s %v %d %v", ended, cost, turns, d)
	}
	if len(leaked) != 1 || leaked[0] != "abcdef0123" {
		t.Errorf("leaked = %v", leaked)
	}
}
