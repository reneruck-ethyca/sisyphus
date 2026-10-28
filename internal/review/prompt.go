package review

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/reneruck/sisyphus/internal/tracker"
)

//go:embed prompt.md
var defaultPrompt string

// PromptData is what a prompt template can use.
type PromptData struct {
	PR           tracker.Tracked
	Reason       string // new | commits | replies
	PreviousHead string // head of this PR's last successful review ("" if none)
	Workdir      string // where the worktree is inside the container
	Diff         string // the command that shows the change
	Notes        string // the repo's own instructions
	Stack        bool   // local services are running for the tests
}

func diffCommand(j Job) string {
	if j.Base != "" {
		return fmt.Sprintf("git diff %s...HEAD", j.Base)
	}
	return fmt.Sprintf("gh pr diff %d --repo %s", j.PR.Number, j.PR.Repo)
}

// renderPrompt fills the template in file ("" = the built-in prompt).
func renderPrompt(file string, d PromptData) (string, error) {
	text := defaultPrompt
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		text = string(b)
	}
	t, err := template.New("prompt").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, d); err != nil {
		return "", err
	}
	return sb.String(), nil
}
