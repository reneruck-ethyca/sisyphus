package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event is one readable piece of a run's output, for UIs. A log line yields
// zero or more events, all carrying that line's number.
type Event struct {
	Line    int    `json:"line"`
	Kind    string `json:"kind"` // output | init | text | tool | tool_result | result | system
	Tool    string `json:"tool,omitempty"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// ClaudeResult is the final result line of a Claude Code run.
type ClaudeResult struct {
	Subtype   string  `json:"subtype"` // success | error_max_turns | error_during_execution | …
	IsError   bool    `json:"is_error"`
	SessionID string  `json:"session_id"`
	NumTurns  int     `json:"num_turns"`
	CostUSD   float64 `json:"cost_usd"`
	Duration  int64   `json:"duration_ms"`
	Error     string  `json:"error,omitempty"`
}

const (
	maxResultText = 2000 // tool output shown per call; the full text is in the log
	maxToolText   = 300
)

// parseLine turns one line of container output into events, plus the result or
// a 5-hour window reading the line carries. Lines that aren't Claude stream-json
// (the setup script, shell errors) become "output" events.
func parseLine(n int, line string) ([]Event, *ClaudeResult, *Window) {
	var m struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
		Cwd     string `json:"cwd"`
		Tools   []any  `json:"tools"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		IsError   bool     `json:"is_error"`
		NumTurns  int      `json:"num_turns"`
		Duration  int64    `json:"duration_ms"`
		CostUSD   float64  `json:"total_cost_usd"`
		SessionID string   `json:"session_id"`
		Errors    []string `json:"errors"`
		RateLimit struct {
			Windows struct {
				FiveHour *struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"five_hour"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &m) != nil || m.Type == "" {
		return []Event{{Line: n, Kind: "output", Text: line}}, nil, nil
	}
	ev := func(kind, tool, text string, isErr bool) Event {
		return Event{Line: n, Kind: kind, Tool: tool, Text: text, IsError: isErr}
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			return []Event{ev("init", "", fmt.Sprintf("%s in %s, %d tools", m.Model, m.Cwd, len(m.Tools)), false)}, nil, nil
		}
		return []Event{ev("system", "", m.Subtype, false)}, nil, nil
	case "assistant", "user":
		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"is_error"`
		}
		if json.Unmarshal(m.Message.Content, &blocks) != nil {
			return nil, nil, nil // a plain-string user message: the prompt echoed back
		}
		var out []Event
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, ev("text", "", t, false))
				}
			case "tool_use":
				out = append(out, ev("tool", b.Name, toolSummary(b.Name, b.Input), false))
			case "tool_result":
				out = append(out, ev("tool_result", "", truncate(resultText(b.Content), maxResultText), b.IsError))
			}
		}
		return out, nil, nil
	case "result":
		r := &ClaudeResult{Subtype: m.Subtype, IsError: m.IsError, SessionID: m.SessionID, NumTurns: m.NumTurns,
			CostUSD: m.CostUSD, Duration: m.Duration, Error: strings.Join(m.Errors, "; ")}
		text := fmt.Sprintf("%s · %d turns · %s · $%.2f", m.Subtype, m.NumTurns,
			(time.Duration(m.Duration) * time.Millisecond).Round(time.Second), m.CostUSD)
		if r.Error != "" {
			text += ": " + r.Error
		}
		return []Event{ev("result", "", text, m.IsError)}, r, nil
	case "stream_event":
		return nil, nil, nil // partial deltas; the complete assistant message follows
	case "rate_limit_event":
		if fh := m.RateLimit.Windows.FiveHour; fh != nil {
			w := &Window{ResetsAt: time.Unix(fh.ResetsAt, 0), Used: fh.Utilization}
			text := fmt.Sprintf("5-hour limit at %.0f%%, resets %s", w.Used*100, w.ResetsAt.Local().Format("15:04"))
			return []Event{ev("system", "", text, false)}, nil, w
		}
	}
	return []Event{ev("system", "", m.Type, false)}, nil, nil
}

// toolSummary picks the one input field that says what a tool call does.
func toolSummary(name string, input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	var s string
	switch {
	case str("command") != "":
		s = str("command")
	case str("file_path") != "":
		s = str("file_path")
	case str("pattern") != "":
		s = str("pattern")
		if p := str("path"); p != "" {
			s += " in " + p
		}
	case str("url") != "":
		s = str("url")
	case str("description") != "":
		s = str("description")
	default:
		s = string(input)
	}
	return truncate(s, maxToolText)
}

// resultText flattens a tool_result's content: a string or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type, Text string
	}
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// lineWriter splits a container's output into lines, appends each to the run
// log, and hands it to onLine with its number (from 0).
type lineWriter struct {
	file   interface{ Write([]byte) (int, error) }
	buf    bytes.Buffer
	n      int
	onLine func(n int, line string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if _, err := w.file.Write(p); err != nil {
		return 0, err
	}
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(w.buf.Next(i + 1))
		w.onLine(w.n, strings.TrimRight(line, "\r\n"))
		w.n++
	}
}

// Flush hands over a final line that had no newline.
func (w *lineWriter) Flush() {
	if w.buf.Len() > 0 {
		w.onLine(w.n, strings.TrimRight(w.buf.String(), "\r\n"))
		w.buf.Reset()
		w.n++
	}
}
