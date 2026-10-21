package review

import (
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	type ev struct{ kind, tool, text string }
	cases := []struct {
		name string
		line string
		want []ev
	}{
		{"setup output", "Resolved 412 packages in 2.1s", []ev{{"output", "", "Resolved 412 packages in 2.1s"}}},
		{"init", `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5","cwd":"/fidesplus","tools":["Bash","Read"]}`,
			[]ev{{"init", "", "claude-opus-5-5 in /fidesplus, 2 tools"}}},
		{"assistant text and tool", `{"type":"assistant","message":{"content":[
			{"type":"text","text":"Looking at the diff."},
			{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr diff 7","description":"diff"}},
			{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/fidesplus/a.py"}},
			{"type":"tool_use","id":"t3","name":"Grep","input":{"pattern":"TODO","path":"src"}}]}}`,
			[]ev{{"text", "", "Looking at the diff."}, {"tool", "Bash", "gh pr diff 7"}, {"tool", "Read", "/fidesplus/a.py"}, {"tool", "Grep", "TODO in src"}}},
		{"tool result", `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"diff --git a b"}]}}`,
			[]ev{{"tool_result", "", "diff --git a b"}}},
		{"tool result blocks", `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}`,
			[]ev{{"tool_result", "", "boom"}}},
		{"success", `{"type":"result","subtype":"success","is_error":false,"num_turns":12,"duration_ms":201000,"total_cost_usd":0.4234,"session_id":"s1","result":"Posted review."}`,
			[]ev{{"result", "", "success · 12 turns · 3m21s · $0.42"}}},
		{"max turns", `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":100,"duration_ms":5000,"total_cost_usd":1,"session_id":"s1","errors":["max turns reached"]}`,
			[]ev{{"result", "", "error_max_turns · 100 turns · 5s · $1.00: max turns reached"}}},
		{"partial deltas are skipped", `{"type":"stream_event","event":{"type":"content_block_delta"}}`, nil},
		{"unknown json", `{"type":"system","subtype":"api_retry","attempt":2}`, []ev{{"system", "", "api_retry"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := parseLine(7, c.line)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i, w := range c.want {
				g := got[i]
				if g.Kind != w.kind || g.Tool != w.tool || g.Text != w.text || g.Line != 7 {
					t.Errorf("event %d = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestParseLineResult(t *testing.T) {
	_, res := parseLine(0, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":3,"duration_ms":10,"total_cost_usd":0.5,"session_id":"s1","errors":["max turns"]}`)
	if res == nil || !res.IsError || res.Subtype != "error_max_turns" || res.SessionID != "s1" || res.NumTurns != 3 || res.CostUSD != 0.5 || res.Error != "max turns" {
		t.Fatalf("result = %+v", res)
	}
}

func TestToolResultTruncated(t *testing.T) {
	evs, _ := parseLine(0, `{"type":"user","message":{"content":[{"type":"tool_result","content":"`+strings.Repeat("x", 5000)+`"}]}}`)
	if len(evs) != 1 || len(evs[0].Text) > maxResultText+len("…") {
		t.Fatalf("not truncated: %d", len(evs[0].Text))
	}
}
