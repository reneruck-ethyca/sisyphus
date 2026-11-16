package main

// The design review page's API: a tracked PR's Markdown docs, rendered in the
// page, and its review threads, read and written as the user on GitHub.

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/reneruck/sisyphus/internal/github"
)

// isDoc says whether the design page renders a file.
func isDoc(p string) bool {
	return slices.Contains([]string{".md", ".markdown", ".mdx"}, strings.ToLower(path.Ext(p)))
}

func docStatus(s string) string {
	if s == "added" || s == "renamed" {
		return s
	}
	return "modified"
}

type designDoc struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// designHandlers serves /api/prs/{owner}/{repo}/{number}/design/…, for tracked PRs only.
func designHandlers(mux *http.ServeMux, srv server) {
	const base = "/api/prs/{owner}/{repo}/{number}/design"
	// pr resolves the request's PR, or answers it with why not.
	pr := func(w http.ResponseWriter, r *http.Request) (github.PRRef, bool) {
		if srv.gh == nil {
			http.Error(w, "GitHub isn't available", http.StatusServiceUnavailable)
			return github.PRRef{}, false
		}
		v, ok := srv.svc.PR(prID(r))
		if !ok {
			http.Error(w, "not a tracked PR", http.StatusNotFound)
			return github.PRRef{}, false
		}
		return v.PRRef, true
	}
	read := func(fn func(ctx context.Context, ref github.PRRef, r *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ref, ok := pr(w, r)
			if !ok {
				return
			}
			v, err := fn(r.Context(), ref, r)
			if err != nil {
				designError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, v)
		}
	}
	// write refuses when the PR moved past the head the page rendered (X-Head):
	// the lines the user commented on may not be those lines any more.
	write := func(fn func(ctx context.Context, ref github.PRRef, d github.DesignPR, r *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ref, ok := pr(w, r)
			if !ok {
				return
			}
			d, err := srv.gh.DesignPR(r.Context(), ref)
			if err != nil {
				designError(w, err)
				return
			}
			if r.Header.Get("X-Head") != d.HeadSHA {
				http.Error(w, "New commits were pushed. Reload to comment on them.", http.StatusConflict)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if err := fn(r.Context(), ref, d, r); err != nil {
				designError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}

	mux.HandleFunc("GET "+base, read(func(ctx context.Context, ref github.PRRef, _ *http.Request) (any, error) {
		d, err := srv.gh.DesignPR(ctx, ref)
		if err != nil {
			return nil, err
		}
		files, err := srv.gh.PRFiles(ctx, ref)
		if err != nil {
			return nil, err
		}
		docs := []designDoc{}
		for _, f := range files {
			if f.Status != "removed" && isDoc(f.Filename) {
				docs = append(docs, designDoc{f.Filename, docStatus(f.Status)})
			}
		}
		pending, err := srv.gh.Pending(ctx, ref, d.Viewer)
		if err != nil {
			return nil, err
		}
		return map[string]any{"title": d.Title, "url": d.URL, "headSha": d.HeadSHA, "baseSha": d.BaseSHA,
			"headRepo": d.HeadRepo, "viewer": d.Viewer, "docs": docs, "pending": pending}, nil
	}))

	// file is one doc at the head, with what the PR changed in it; since=<sha>
	// adds the lines changed after that commit (sinceLines, null when unknown).
	mux.HandleFunc("GET "+base+"/file", read(func(ctx context.Context, ref github.PRRef, r *http.Request) (any, error) {
		p, since := r.URL.Query().Get("path"), r.URL.Query().Get("since")
		d, err := srv.gh.DesignPR(ctx, ref)
		if err != nil {
			return nil, err
		}
		files, err := srv.gh.PRFiles(ctx, ref)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(files, func(f github.ChangedFile) bool { return f.Filename == p && f.Status != "removed" })
		if i < 0 || !isDoc(p) {
			return nil, errNotDoc
		}
		f := files[i]
		content, err := srv.gh.Content(ctx, d.HeadRepo, p, d.HeadSHA)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"path": p, "headSha": d.HeadSHA, "content": string(content), "status": docStatus(f.Status),
			"patchMissing": f.Patch == nil, "hunks": []github.Hunk{}, "addedLines": []int{}, "sinceLines": nil}
		if f.Patch != nil {
			out["hunks"], out["addedLines"] = github.ParsePatch(*f.Patch)
		}
		if since != "" && since != d.HeadSHA {
			// ponytail: a since commit GitHub no longer has (force-pushed away) leaves sinceLines null; the page then highlights the whole PR.
			if cf, err := srv.gh.CompareFiles(ctx, d.HeadRepo, since, d.HeadSHA); err == nil {
				lines := []int{}
				if j := slices.IndexFunc(cf, func(c github.ChangedFile) bool { return c.Filename == p }); j >= 0 && cf[j].Patch != nil {
					_, lines = github.ParsePatch(*cf[j].Patch)
				}
				out["sinceLines"] = lines
			}
		} else if since != "" {
			out["sinceLines"] = []int{}
		}
		return out, nil
	}))

	// blob is a file the doc links to (an image), at the head.
	mux.HandleFunc("GET "+base+"/blob", func(w http.ResponseWriter, r *http.Request) {
		ref, ok := pr(w, r)
		if !ok {
			return
		}
		p := r.URL.Query().Get("path")
		d, err := srv.gh.DesignPR(r.Context(), ref)
		if err != nil {
			designError(w, err)
			return
		}
		b, err := srv.gh.Content(r.Context(), d.HeadRepo, p, d.HeadSHA)
		if err != nil {
			designError(w, err)
			return
		}
		if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// Repo content is untrusted: it must never run as a page on our origin (an SVG opened directly).
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Write(b)
	})

	mux.HandleFunc("GET "+base+"/threads", read(func(ctx context.Context, ref github.PRRef, r *http.Request) (any, error) {
		return srv.gh.Threads(ctx, ref, r.URL.Query().Get("path"))
	}))
	mux.HandleFunc("POST "+base+"/threads", write(func(ctx context.Context, ref github.PRRef, d github.DesignPR, r *http.Request) error {
		var in github.NewThread
		if err := decodeDesign(r, &in); err != nil {
			return err
		}
		return srv.gh.CreateThread(ctx, ref, d, in)
	}))
	mux.HandleFunc("POST "+base+"/threads/{id}/replies", write(func(ctx context.Context, ref github.PRRef, d github.DesignPR, r *http.Request) error {
		var in struct{ Body, Mode, CommentID string }
		if err := decodeDesign(r, &in); err != nil {
			return err
		}
		return srv.gh.ReplyThread(ctx, ref, d, r.PathValue("id"), in.CommentID, in.Body, in.Mode)
	}))
	for _, op := range []string{"resolve", "unresolve"} {
		mux.HandleFunc("POST "+base+"/threads/{id}/"+op, write(func(ctx context.Context, _ github.PRRef, _ github.DesignPR, r *http.Request) error {
			return srv.gh.SetResolved(ctx, r.PathValue("id"), op == "resolve")
		}))
	}
	mux.HandleFunc("POST "+base+"/review", write(func(ctx context.Context, ref github.PRRef, d github.DesignPR, r *http.Request) error {
		var in struct{ Event, Body string }
		if err := decodeDesign(r, &in); err != nil {
			return err
		}
		if !slices.Contains([]string{"COMMENT", "APPROVE", "REQUEST_CHANGES"}, in.Event) {
			return badRequest{errors.New("event must be COMMENT, APPROVE or REQUEST_CHANGES")}
		}
		if err := srv.gh.SubmitReview(ctx, ref, d, in.Event, in.Body); err != nil {
			return err
		}
		_, err := srv.tracker.Refresh(ctx, ref.Key()) // so the PR leaves Needs you now, not at the next check
		return err
	}))
	mux.HandleFunc("DELETE "+base+"/review", write(func(ctx context.Context, ref github.PRRef, d github.DesignPR, _ *http.Request) error {
		return srv.gh.DiscardReview(ctx, ref, d.Viewer)
	}))
}

var errNotDoc = errors.New("not a doc this PR changes")

type badRequest struct{ error }

func decodeDesign(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest{err}
	}
	return nil
}

// designError answers with GitHub's own status for a 4xx it gave (a gone
// thread, a refused comment), and 502 for the rest of what went wrong there.
func designError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var he *api.HTTPError
	var ge *api.GraphQLError
	var br badRequest
	switch {
	case errors.As(err, &br):
		code = http.StatusBadRequest
	case errors.Is(err, errNotDoc), errors.Is(err, github.ErrNoPendingReview):
		code = http.StatusNotFound
	case errors.As(err, &he) && he.StatusCode >= 400 && he.StatusCode < 500 && he.StatusCode != http.StatusUnauthorized:
		code = he.StatusCode
	case errors.As(err, &ge):
		code = http.StatusUnprocessableEntity
	}
	http.Error(w, err.Error(), code)
}
