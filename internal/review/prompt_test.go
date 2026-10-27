package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reneruck/sisyphus/internal/tracker"
)

func TestRenderPrompt(t *testing.T) {
	d := PromptData{PR: tracker.Tracked{PRRef: refA, Title: "Fix it", HeadSHA: "abc123", URL: "https://x/1"}, Reason: "new", Workdir: "/fidesplus", Diff: "git diff base1...HEAD"}
	got, err := renderPrompt("", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"git diff base1...HEAD", "#1 in ethyca/a", `"Fix it"`, "/fidesplus", "at abc123", "/out/review.json", "/sisyphus/context/", "first review"} {
		if !strings.Contains(got, want) {
			t.Errorf("default prompt missing %q:\n%s", want, got)
		}
	}
	d.Reason, d.PreviousHead = "commits", "old999"
	if got, _ := renderPrompt("", d); !strings.Contains(got, "git diff old999..HEAD") || strings.Contains(got, "first review") {
		t.Errorf("commits prompt:\n%s", got)
	}
	d.Reason = "replies"
	if got, _ := renderPrompt("", d); !strings.Contains(got, "new replies") {
		t.Errorf("replies prompt:\n%s", got)
	}

	f := filepath.Join(t.TempDir(), "p.md")
	os.WriteFile(f, []byte("Review {{.PR.URL}} ({{.Reason}})"), 0o644)
	if got, err := renderPrompt(f, d); err != nil || got != "Review https://x/1 (replies)" {
		t.Errorf("custom prompt = %q, %v", got, err)
	}
	os.WriteFile(f, []byte("{{.Nope}}"), 0o644)
	if _, err := renderPrompt(f, d); err == nil {
		t.Error("unknown field should fail")
	}
}
