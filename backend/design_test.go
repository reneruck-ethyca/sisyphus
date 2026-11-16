package main

import (
	"reflect"
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
	} {
		if got := addedLines(c.before, c.now); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q → %q: got %v, want %v", c.before, c.now, got, c.want)
		}
	}
}
