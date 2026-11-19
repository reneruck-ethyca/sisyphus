package lastmile

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// InvalidError is a handoff TLM refuses: the reason is for whoever sent it.
type InvalidError string

func (e InvalidError) Error() string { return string(e) }

func invalid(format string, args ...any) error { return InvalidError(fmt.Sprintf(format, args...)) }

var (
	jiraKey   = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)
	unsafeID  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	reviewer  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*(\[bot\])?$`)
	labelSafe = regexp.MustCompile(`^[^,\n]+$`) // gh takes labels and reviewers as comma lists
)

// Handoff validates req, records the handoff and returns it. The worktree
// must be clean, its branch ahead of origin/<base> and in a configured repo,
// and not already owned by an open handoff. From here on TLM owns the worktree.
func (m *Manager) Handoff(ctx context.Context, req Request) (Handoff, error) {
	h, err := m.parse(req)
	if err != nil {
		return Handoff{}, err
	}
	if err := m.inspect(ctx, &h, req.StackParent); err != nil {
		return Handoff{}, err
	}
	if err := m.add(&h); err != nil {
		return Handoff{}, err
	}
	m.changed()
	m.Kick()
	return h, nil
}

// add gives h an id and stores it, unless its branch is already owned.
func (m *Manager) add(h *Handoff) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := slices.IndexFunc(m.handoffs, func(o Handoff) bool {
		return o.Repo == h.Repo && o.Branch == h.Branch && !Terminal(o.State)
	}); i >= 0 {
		return invalid("branch %s of %s is already owned by handoff %s (%s)", h.Branch, h.Repo, m.handoffs[i].ID, m.handoffs[i].State)
	}
	for { // 4 hex digits: a clash is possible, so draw again
		h.ID = newID(h.Repo, h.Branch)
		if !slices.ContainsFunc(m.handoffs, func(o Handoff) bool { return o.ID == h.ID }) {
			break
		}
	}
	m.handoffs = append(m.handoffs, *h)
	if err := m.saveLocked(); err != nil {
		m.handoffs = m.handoffs[:len(m.handoffs)-1]
		return err
	}
	return nil
}

// parse checks what req says by itself, without looking at the worktree.
func (m *Manager) parse(req Request) (Handoff, error) {
	h := Handoff{Worktree: filepath.Clean(req.Worktree), Base: req.Base, Title: strings.TrimSpace(req.Title),
		Body: req.Body, Jira: req.Jira, Notify: req.Notify, Requester: req.Requester,
		State: StateHandedOff, MaxFixRounds: req.MaxFixRounds}
	if !filepath.IsAbs(req.Worktree) {
		return h, invalid("worktree must be an absolute path")
	}
	if req.Base == "" || !worktree.ValidBranch(context.Background(), req.Base) {
		return h, invalid("base %q isn't a branch name", req.Base)
	}
	if sp := req.StackParent; sp != "" && !worktree.ValidBranch(context.Background(), sp) {
		return h, invalid("stack_parent %q isn't a handoff id or a branch name", sp)
	}
	if req.Jira != "" && !jiraKey.MatchString(req.Jira) {
		return h, invalid("jira %q isn't a ticket key like ENG-1234", req.Jira)
	}
	var err error
	if h.Autonomy, err = parseAutonomy(req.Autonomy); err != nil {
		return h, err
	}
	if h.Agent, err = m.parseAgent(req.Agent); err != nil {
		return h, err
	}
	if h.MaxFixRounds < 0 {
		return h, invalid("max_fix_rounds must be >= 0")
	}
	h.MaxFixRounds = cmp.Or(h.MaxFixRounds, m.cfg.LastMile.MaxFixRounds)
	for _, l := range req.Labels {
		if l = strings.TrimSpace(l); !labelSafe.MatchString(l) {
			return h, invalid("label %q is empty or has a comma", l)
		}
		h.Labels = append(h.Labels, l)
	}
	for _, r := range req.Reviewers {
		if r = strings.TrimSpace(r); !reviewer.MatchString(r) {
			return h, invalid("reviewer %q isn't a GitHub login", r)
		}
		h.Reviewers = append(h.Reviewers, r)
	}
	if h.Notify != "" {
		if _, err := parseNotify(h.Notify); err != nil {
			return h, invalid("notify template: %v", err)
		}
	}
	for _, e := range req.NotifyOn {
		if !slices.Contains(events, e) {
			return h, invalid("notify_on %q isn't one of %s", e, strings.Join(events, ", "))
		}
	}
	if len(req.NotifyOn) > 0 && h.Notify == "" {
		return h, invalid("notify_on needs a notify command")
	}
	h.NotifyOn = req.NotifyOn
	if h.Notify != "" && len(h.NotifyOn) == 0 {
		h.NotifyOn = slices.Clone(defaultNotifyOn)
	}
	return h, nil
}

// parseAutonomy reads "fix-ci,address-reviews,merge=off|shadow|auto|approved"; "" allows nothing.
func parseAutonomy(s string) (Autonomy, error) {
	a := Autonomy{Merge: MergeOff}
	for _, tok := range strings.Split(s, ",") {
		switch tok = strings.TrimSpace(tok); tok {
		case "":
		case "fix-ci":
			a.FixCI = true
		case "address-reviews":
			a.AddressReviews = true
		default:
			mode, ok := strings.CutPrefix(tok, "merge=")
			if !ok || !slices.Contains(mergeModes, mode) {
				return a, invalid("autonomy %q isn't fix-ci, address-reviews or merge=off|shadow|auto|approved", tok)
			}
			a.Merge = mode
		}
	}
	return a, nil
}

// parseAgent reads "vendor:model[:effort]"; "" is the review agent.
func (m *Manager) parseAgent(s string) (config.Agent, error) {
	if s == "" {
		return m.cfg.Review.Agent, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return config.Agent{}, invalid("agent %q isn't vendor:model:effort", s)
	}
	a := config.Agent{Vendor: parts[0], Model: parts[1]}
	if len(parts) == 3 {
		a.Effort = parts[2]
	}
	if err := a.Check(); err != nil {
		return a, invalid("agent %q: %v", s, err)
	}
	return a, nil
}

// inspect fills h from the worktree and checks it can be handed over.
func (m *Manager) inspect(ctx context.Context, h *Handoff, stackParent string) error {
	git := func(args ...string) (string, error) { return proc.Run(ctx, h.Worktree, "git", args...) }
	branch, err := git("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return invalid("%s has no branch checked out: %v", h.Worktree, err)
	}
	h.Branch = branch
	// ponytail: the raw remote URL, not `remote get-url`, which expands insteadOf aliases into something we can't read.
	remote, err := git("config", "--get", "remote.origin.url")
	if err != nil {
		return invalid("%s has no origin remote", h.Worktree)
	}
	if h.Repo, err = parseRepo(remote); err != nil {
		return err
	}
	if _, ok := m.cfg.Review.Repos[h.Repo]; !ok {
		return invalid("%s isn't in review.repos, so it has no image or setup", h.Repo)
	}
	parent, err := m.resolveParent(h, stackParent)
	if err != nil {
		return err
	}
	baseRef := "origin/" + h.Base
	if _, err := git("fetch", "--quiet", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", h.Base, h.Base)); err != nil {
		// A parent handed off a moment ago may not be pushed yet; its local branch does for the check.
		if _, lerr := git("rev-parse", "--verify", "--quiet", "refs/heads/"+h.Base); parent == nil || lerr != nil {
			return invalid("fetching base %s: %v", h.Base, err)
		}
		baseRef = "refs/heads/" + h.Base
	}
	// Untracked files don't count: wtp hooks copy untracked config into worktrees.
	status, err := git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if status != "" {
		return invalid("worktree isn't clean: commit or discard its changes first")
	}
	others, err := git("ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	h.Untracked = slices.DeleteFunc(strings.Split(others, "\x00"), func(f string) bool { return f == "" })
	ahead, err := git("rev-list", "--count", baseRef+"..HEAD")
	if err != nil {
		return err
	}
	if n, _ := strconv.Atoi(ahead); n == 0 {
		return invalid("branch %s has no commits ahead of %s", h.Branch, baseRef)
	}
	if h.Title == "" {
		if h.Title, err = git("log", "-1", "--format=%s"); err != nil {
			return err
		}
	}
	now := time.Now()
	h.Created, h.Updated = now, now
	return nil
}

// parseRepo reads "owner/name" from a GitHub remote URL (https, ssh or scp-style).
func parseRepo(remote string) (string, error) {
	// Only github.com: the repo name picks the image, setup and secrets, so a local path
	// or another host must not pass for a configured repo.
	var path string
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		if u.Hostname() != "github.com" {
			return "", invalid("remote %q isn't on github.com", remote)
		}
		path = u.Path
	} else if host, after, ok := strings.Cut(remote, ":"); ok && (host == "github.com" || strings.HasSuffix(host, "@github.com")) {
		path = after // git@github.com:owner/name.git
	} else {
		return "", invalid("remote %q isn't on github.com", remote)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if owner, name, ok := strings.Cut(path, "/"); ok && owner != "" && name != "" && !strings.Contains(name, "/") {
		return path, nil
	}
	return "", invalid("can't read owner/name from remote %q", remote)
}

// newID is "<repo>-<branch>-<4 hex>", safe in paths, labels and shell words.
func newID(repo, branch string) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	slug := strings.Trim(unsafeID.ReplaceAllString(branch, "-"), "-.")
	return fmt.Sprintf("%s-%s-%s", filepath.Base(repo), slug, hex.EncodeToString(b[:]))
}

// resolveParent turns --stack-parent into h's base (design §3): the parent handoff's
// branch, found by id or by branch among the open handoffs of h's repo, else the
// name is a plain base branch and nothing is tracked. It returns the parent handoff, if any.
func (m *Manager) resolveParent(h *Handoff, name string) (*Handoff, error) {
	if name == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var parent *Handoff
	if i := slices.IndexFunc(m.handoffs, func(o Handoff) bool { return o.ID == name }); i >= 0 {
		parent = &m.handoffs[i]
		switch {
		case parent.Repo != h.Repo:
			return nil, invalid("stack parent %s belongs to %s, not %s", name, parent.Repo, h.Repo)
		case Terminal(parent.State):
			return nil, invalid("stack parent %s is %s", name, parent.State)
		}
	} else if i := slices.IndexFunc(m.handoffs, func(o Handoff) bool {
		return o.Repo == h.Repo && o.Branch == name && !Terminal(o.State)
	}); i >= 0 {
		parent = &m.handoffs[i]
	}
	if parent == nil {
		h.Base = name // a branch nobody hands off: just the base
		return nil, nil
	}
	// A cycle: the chain above the parent reaches this branch (the new handoff has no id yet,
	// so only a branch can close it).
	for p, n := parent, 0; p != nil && n <= len(m.handoffs); n++ {
		if p.Branch == h.Branch {
			return nil, invalid("stack parent %s would make a cycle: %s is %s's own branch", name, p.Branch, h.Branch)
		}
		i := slices.IndexFunc(m.handoffs, func(o Handoff) bool { return p.StackParent != "" && o.ID == p.StackParent })
		if i < 0 {
			break
		}
		p = &m.handoffs[i]
	}
	h.StackParent, h.Base = parent.ID, parent.Branch
	cp := *parent
	return &cp, nil
}
