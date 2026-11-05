package lastmile

import (
	"strings"
	"text/template"
)

// fixPrompt is what the CI fix agent is told. Built in: it carries the result contract.
var fixPrompt = template.Must(template.New("fix").Parse(`You are fixing a failing CI run on a pull request that an agent handed off.

## The pull request

Title: {{.Title}}
Branch: {{.Branch}} (base: {{.Base}})

{{.Body}}

## What failed

{{range .Failures}}### {{.Name}}
{{if .Log}}` + "```" + `
{{.Log}}
` + "```" + `{{else}}(no log available: not a GitHub Actions job){{end}}

{{end}}Snapshot of the PR (JSON; the same file is at /sisyphus/context/snapshot.json):

` + "```json" + `
{{.Snapshot}}
` + "```" + `

## The change so far (git diff origin/{{.Base}}...HEAD{{if .DiffTruncated}}, truncated{{end}})

` + "```diff" + `
{{.Diff}}
` + "```" + `

## Rules

- Edit files in the working directory only. Do not run git. Do not run gh or call GitHub. Do not commit, push or stage.
- Fix the cause of the failure with the smallest change that does it. Do not touch CI workflows, migrations or lockfiles.
- If the failure is not something you can fix from here (infrastructure, a missing secret, a test that needs a service you do not have, a design decision), do not guess: set "escalate" to true and say why.
- When you are done, write /out/result.json:

` + "```json" + `
{"summary": "one line, imperative, what you changed",
 "confidence": "low|medium|high",
 "escalate": false,
 "escalate_reason": "",
 "body": "optional: a new PR description, only if the old one is now wrong",
 "body_reason": "optional: why"}
` + "```" + `
`))

// renderFixPrompt fills the prompt; a template error is a bug, so it panics into the run's failure.
func renderFixPrompt(d fixPromptData) (string, error) {
	var sb strings.Builder
	err := fixPrompt.Execute(&sb, d)
	return sb.String(), err
}

type fixPromptData struct {
	Title, Body, Branch, Base string
	Failures                  []Failure
	Snapshot                  string
	Diff                      string
	DiffTruncated             bool
}
