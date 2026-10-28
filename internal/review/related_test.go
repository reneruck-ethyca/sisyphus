package review

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
)

func TestTicketKeys(t *testing.T) {
	for _, c := range []struct {
		title, branch string
		want          []string
	}{
		{"ENG-6804: port the specs", "eng-6804-port", []string{"ENG-6804"}},
		{"Fix ENG-1 and HJ-22", "feat/astralis-multiorg", []string{"ENG-1", "HJ-22"}},
		{"Run the Delphi specs", "fix/astralis-helios-lane", nil},
	} {
		if got := ticketKeys(c.title, c.branch); !slices.Equal(got, c.want) {
			t.Errorf("ticketKeys(%q, %q) = %v, want %v", c.title, c.branch, got, c.want)
		}
	}
}

func TestRelatedCheckout(t *testing.T) {
	upstream, clone := gittest.Setup(t) // main has a.txt "one", feature/x "two"
	prHead := gittest.Git(t, upstream, "rev-parse", "feature/x")
	gittest.Git(t, upstream, "update-ref", "refs/pull/7/head", prHead)
	os.WriteFile(filepath.Join(clone, ".env"), []byte("SECRET=1"), 0o600)

	for name, c := range map[string]struct {
		open       map[string][]github.OpenPR
		want, says string
	}{
		"no PR: base": {nil, "one", "main, since no open PR there names ENG-1"},
		"open PR":     {map[string][]github.OpenPR{"ENG-1": {{Number: 7, Title: "Feature", HeadSHA: prHead}}}, "two", `the head of ethyca/b#7 ("Feature")`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *config.Review) {
				c.Repos["ethyca/a"] = config.RepoReview{Image: "img:1",
					Related: &config.Related{Repo: "ethyca/b", Base: "main", Target: "/b", Clone: clone}}
			})
			gh := &fakeGH{open: c.open}
			f.r.GitHub, f.tr.Title = gh, "ENG-1: test it"
			f.start(t)
			f.r.Tracked(f.tr)
			waitFor(t, "container start", f.started(1))
			run := f.runs()[0]

			dir := relatedDir(run)
			if !slices.Contains(f.rt.Started()[0].Mounts, container.Mount{Source: dir, Target: "/b", ReadOnly: true}) {
				t.Fatalf("related not mounted: %+v", f.rt.Started()[0].Mounts)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != c.want {
				t.Errorf("a.txt = %q, want %q", b, c.want)
			}
			for _, absent := range []string{".env", ".git"} {
				if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
					t.Errorf("%s in the checkout: %v", absent, err)
				}
			}
			if p, _ := os.ReadFile(filepath.Join(run.Dir, "prompt.md")); !strings.Contains(string(p), "ethyca/b is checked out read-only at /b") ||
				!strings.Contains(string(p), c.says) {
				t.Errorf("prompt doesn't say what's checked out:\n%s", p)
			}
			if !slices.Equal(gh.searches, []string{"ethyca/b ENG-1"}) {
				t.Errorf("searches = %v", gh.searches)
			}

			f.succeed(f.containers()[0].ID, "")
			waitFor(t, "run to finish", func() bool { return f.runs()[0].Status != Running })
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("related checkout left behind: %v", err)
			}
		})
	}
}
