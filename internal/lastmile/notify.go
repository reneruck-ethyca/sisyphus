package lastmile

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"text/template"
)

// quote makes s one inert shell word: single-quoted, with each ' as '\”.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// notifyFields are the values a --notify template may use, and TLM_<NAME> in
// the environment.
func notifyFields(h Handoff, event string) map[string]string {
	pr := ""
	if h.PRNumber != 0 {
		pr = strconv.Itoa(h.PRNumber)
	}
	return map[string]string{"ID": h.ID, "Event": event, "Repo": h.Repo, "Branch": h.Branch, "PRURL": h.PRURL,
		"PRNumber": pr, "State": h.State, "Reason": h.Reason, "Requester": h.Requester}
}

// parseNotify checks a --notify template at handoff time, so a typo fails there.
func parseNotify(tmpl string) (*template.Template, error) {
	return template.New("notify").Option("missingkey=error").Parse(tmpl)
}

// renderNotify fills tmpl with the handoff's values, each shell-quoted: they
// include text from outside (branch names, reasons), and the result runs in sh.
func renderNotify(tmpl string, h Handoff, event string) (string, error) {
	t, err := parseNotify(tmpl)
	if err != nil {
		return "", err
	}
	quoted := map[string]string{}
	for k, v := range notifyFields(h, event) {
		quoted[k] = quote(v)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, quoted); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Notify runs the handoff's --notify command for event, if it asked for it, in
// its worktree. A failure is logged: a callback never stops the handoff.
func (m *Manager) Notify(ctx context.Context, h Handoff, event string) {
	if !h.Notifies(event) {
		return
	}
	cmd, err := renderNotify(h.Notify, h, event)
	if err == nil {
		env := map[string]string{}
		for k, v := range notifyFields(h, event) {
			env["TLM_"+strings.ToUpper(k)] = v
		}
		err = m.runHook(ctx, cmd, m.cfg.HookTimeout.Duration, h.Worktree, env)
	}
	if err != nil {
		slog.Warn("lastmile notify failed", "id", h.ID, "event", event, "err", fmt.Sprint(err))
	}
}
