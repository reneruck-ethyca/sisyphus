package github

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParsePatch(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		hunks []Hunk
		added []int
	}{
		{"added file", "@@ -0,0 +1,3 @@\n+a\n+b\n+c", []Hunk{{1, 3}}, []int{1, 2, 3}},
		{"two hunks", "@@ -1,3 +1,4 @@\n a\n-b\n+B\n+B2\n c\n@@ -20,2 +21,3 @@\n x\n+y\n z",
			[]Hunk{{1, 4}, {21, 23}}, []int{2, 3, 22}},
		{"empty right side", "@@ -1,2 +0,0 @@\n-a\n-b", []Hunk{}, []int{}},
		{"missing count", "@@ -5 +5 @@\n-a\n+b", []Hunk{{5, 5}}, []int{5}},
		{"no newline marker", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file",
			[]Hunk{{1, 1}}, []int{1}},
		{"empty patch", "", []Hunk{}, []int{}},
	}
	for _, c := range cases {
		h, a := ParsePatch(c.patch)
		if !reflect.DeepEqual(h, c.hunks) || !reflect.DeepEqual(a, c.added) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, h, a, c.hunks, c.added)
		}
	}
}

func TestThreadNodeDecodesCommentsConnection(t *testing.T) {
	var n threadNode
	in := `{"id":"T1","path":"docs/a.md","comments":{"nodes":[{"id":"C1","body":"hi","author":{"login":"rene"}}]}}`
	if err := json.Unmarshal([]byte(in), &n); err != nil {
		t.Fatal(err)
	}
	if len(n.Comments.Nodes) != 1 || n.Comments.Nodes[0].Author.Login != "rene" || n.Path != "docs/a.md" {
		t.Fatalf("unexpected decode: %+v", n)
	}
}

func TestParseMarker(t *testing.T) {
	got := parseMarker("Why?\n\n<!-- design-review:L3-L7@abc123 -->")
	if got == nil || *got != (DocAnchor{3, 7, "abc123"}) {
		t.Fatalf("got %+v", got)
	}
	if parseMarker("no marker") != nil {
		t.Fatal("expected nil without a marker")
	}
}
