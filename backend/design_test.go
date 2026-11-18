package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestAddedLines(t *testing.T) {
	for _, c := range []struct {
		before, now string
		want        []int
	}{
		{"", "a\nb", []int{1, 2}},
		{"a\nb\nc", "a\nb\nc", []int{}},
		{"a\nb\nc", "a\nX\nb\nc\nY", []int{2, 5}},
		{"a\nb\nc", "a\nB\nc", []int{2}},
		{"a\nb", "", []int{1}}, // an emptied file is one empty line
		{strings.Repeat("x\n", 9000) + "a", strings.Repeat("x\n", 9000) + "b\na", []int{9001}}, // long, but only the middle differs
		{strings.Repeat("a\n", 3000), strings.Repeat("b\n", 3000), nil},                        // too big a table: no highlight
	} {
		if got := addedLines(c.before, c.now); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q → %q: got %v, want %v", c.before, c.now, got, c.want)
		}
	}
}
