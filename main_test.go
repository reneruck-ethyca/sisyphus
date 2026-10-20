package main

import (
	"strings"
	"testing"
)

func TestCheckTools(t *testing.T) {
	if err := checkTools("sh"); err != nil {
		t.Fatalf("sh should resolve: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := checkTools("gh"); err == nil || !strings.Contains(err.Error(), "gh") {
		t.Fatalf("got %v", err)
	}
}
