package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// API talks to GitHub with the token gh would use: GH_TOKEN or GITHUB_TOKEN,
// gh's config, or the keyring through `gh auth token`.
type API struct {
	opts api.ClientOptions

	mu   sync.Mutex
	rest *api.RESTClient
	gql  *api.GraphQLClient
}

// NewAPI builds clients lazily from opts; the zero value finds the token as gh does.
func NewAPI(opts api.ClientOptions) *API {
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute // bounds every call, as proc.Timeout bounded gh
	}
	return &API{opts: opts}
}

func (a *API) clients(fresh bool) (*api.RESTClient, *api.GraphQLClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fresh || a.rest == nil {
		rest, err := api.NewRESTClient(a.opts)
		if err != nil {
			return nil, nil, fmt.Errorf("github: %w (run `gh auth login` or set GH_TOKEN)", err)
		}
		gql, err := api.NewGraphQLClient(a.opts)
		if err != nil {
			return nil, nil, fmt.Errorf("github: %w", err)
		}
		a.rest, a.gql = rest, gql
	}
	return a.rest, a.gql, nil
}

// call runs fn, and once more with clients built anew if GitHub answered 401,
// so a `gh auth login` after start is picked up. A 401 means nothing was done,
// so writes are safe to repeat.
func (a *API) call(fn func(*api.RESTClient, *api.GraphQLClient) error) error {
	rest, gql, err := a.clients(false)
	if err != nil {
		return err
	}
	err = fn(rest, gql)
	if IsStatus(err, http.StatusUnauthorized) {
		if rest, gql, ferr := a.clients(true); ferr == nil {
			err = fn(rest, gql)
		}
	}
	return err
}

// do sends a REST request. in is sent as is when it's []byte, else as JSON.
// It returns the body and the next page's URL ("" on the last page).
func (a *API) do(ctx context.Context, method, path string, in any) (body []byte, next string, err error) {
	var payload []byte
	switch v := in.(type) {
	case nil:
	case []byte:
		payload = v
	default:
		if payload, err = json.Marshal(v); err != nil {
			return nil, "", err
		}
	}
	err = a.call(func(rest *api.RESTClient, _ *api.GraphQLClient) error {
		var r io.Reader
		if payload != nil {
			r = bytes.NewReader(payload)
		}
		resp, err := rest.RequestWithContext(ctx, method, path, r)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		next = nextPage(resp.Header.Get("Link"))
		body, err = io.ReadAll(resp.Body)
		return err
	})
	return body, next, err
}

// send is do with the reply decoded into out (nil = ignored).
func (a *API) send(ctx context.Context, method, path string, in, out any) error {
	b, _, err := a.do(ctx, method, path, in)
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

func (a *API) get(ctx context.Context, path string, out any) error {
	return a.send(ctx, http.MethodGet, path, nil, out)
}

// graphql runs query; out receives the response's data.
func (a *API) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	return a.call(func(_ *api.RESTClient, gql *api.GraphQLClient) error {
		return gql.DoWithContext(ctx, query, vars, out)
	})
}

// pages GETs path and every page after it, each a JSON array.
func pages[T any](ctx context.Context, a *API, path string) ([]T, error) {
	var all []T
	for path != "" {
		b, next, err := a.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		all, path = append(all, page...), next
	}
	return all, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextPage(link string) string {
	if m := linkNext.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

// IsStatus says err is GitHub answering with HTTP status code.
func IsStatus(err error, code int) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.StatusCode == code
}
