package jsonfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "real.json"), filepath.Join(dir, "link.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Save(link, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi, err)
	}
	var got map[string]int
	if err := Load(target, &got); err != nil || got["a"] != 1 {
		t.Errorf("target = %v, %v", got, err)
	}
}
