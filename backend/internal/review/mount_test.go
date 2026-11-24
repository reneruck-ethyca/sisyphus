package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/gittest"
)

func TestSetupRestoresTrackedFilesUnderACacheMount(t *testing.T) {
	_, clone := gittest.Setup(t)
	os.Mkdir(filepath.Join(clone, "c"), 0o755)
	gittest.Commit(t, clone, "c/__init__.py", "x")
	os.Remove(filepath.Join(clone, "c", "__init__.py")) // an empty volume mounted over c/

	cmd := exec.Command("bash", "-c", setupScript(config.Review{}, config.RepoReview{Cache: []string{"c", "/abs"}}, nil))
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(clone, "c", "__init__.py")); err != nil || string(b) != "x" {
		t.Fatalf("not restored: %q, %v", b, err)
	}
}
