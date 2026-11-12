package container

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunArgs(t *testing.T) {
	args, env, err := runArgs(Spec{
		Name: "wb-1", Network: "net", Image: "img:1", Command: []string{"sh", "-c", "echo hi"},
		Env:     map[string]string{"SECRET": "s3cr3t", "A": "1"},
		Mounts:  []Mount{{Source: "/w", Target: "/w"}, {Source: "/g", Target: "/g", ReadOnly: true}},
		Workdir: "/w", Labels: map[string]string{"sisyphus": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--detach", "--init", "--name", "wb-1", "--network", "net", "--label", "sisyphus=1",
		"--env", "A", "--env", "SECRET", "--volume", "/w:/w", "--volume", "/g:/g:ro",
		"--workdir", "/w", "--entrypoint", "sh", "img:1", "-c", "echo hi"}
	if !slices.Equal(args, want) {
		t.Fatalf("args\n got %q\nwant %q", args, want)
	}
	if strings.Contains(strings.Join(args, " "), "s3cr3t") || !slices.Equal(env, []string{"A=1", "SECRET=s3cr3t"}) {
		t.Fatalf("secret leaked into argv or env wrong: %q", env)
	}
	if _, _, err := runArgs(Spec{Image: "i", Command: []string{"x"}, Mounts: []Mount{{Source: "/a:b", Target: "/a"}}}); err == nil {
		t.Fatal("colon in mount path should be rejected")
	}
}

func TestParseInspect(t *testing.T) {
	got, err := parseInspect([]byte(`[{"Id":"abc","Config":{"Labels":{"sisyphus.run":"r1"}},"State":{"Running":false,"ExitCode":3}}]`))
	if err != nil || len(got) != 1 || got[0].ID != "abc" || got[0].Labels["sisyphus.run"] != "r1" || got[0].Running || got[0].ExitCode != 3 {
		t.Fatalf("%+v %v", got, err)
	}
}

// TestDockerRoundTrip runs a real container; skipped without a Docker daemon.
func TestDockerRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := Docker{}
	if testing.Short() || d.Available(ctx) != nil {
		t.Skip("no docker")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir()) // /var → /private/var, which Docker Desktop shares
	labels := map[string]string{"sisyphus.test": t.Name()}
	id, err := d.Start(ctx, Spec{
		Image: "alpine/git:v2.49.1", Command: []string{"sh", "-c", `echo "hi $WHO"; echo out > /out/f; exit 3`},
		Env: map[string]string{"WHO": "there"}, Mounts: []Mount{{Source: dir, Target: "/out"}}, Labels: labels,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove(ctx, id)
	code, err := d.Wait(ctx, id)
	if err != nil || code != 3 {
		t.Fatalf("wait: %d %v", code, err)
	}
	var logs bytes.Buffer
	if err := d.Logs(ctx, id, &logs); err != nil || logs.String() != "hi there\n" {
		t.Fatalf("logs %q %v", logs.String(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "out\n" {
		t.Fatalf("mount: %q", b)
	}
	infos, err := d.List(ctx, labels)
	if err != nil || len(infos) != 1 || infos[0].ID != id || infos[0].Running || infos[0].ExitCode != 3 {
		t.Fatalf("list: %+v %v", infos, err)
	}
}
