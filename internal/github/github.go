// Package github reads notifications, search results and PR snapshots from github.com using the
// credentials of the gh CLI. It never mutates anything on GitHub.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"

	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	host            = "github.com"
	apiBaseURL      = "https://api.github.com/"
	searchResultCap = 1000
	// statusBatchSize keeps a batch well below GitHub's ~10s GraphQL execution limit; the status
	// check rollup costs roughly 0.3s per PR.
	statusBatchSize = 10
)

type Client struct {
	http *http.Client
	// baseURL is the REST API root with a trailing slash; GraphQL is served at baseURL+"graphql".
	baseURL string
	now     func() time.Time
	log     *slog.Logger
}

// NewClient authenticates with the github.com token of the gh CLI.
func NewClient() (*Client, error) {
	token, _ := auth.TokenForHost(host)
	if token == "" {
		return nil, errors.New("no github.com token found, run gh auth login")
	}
	return newClient(api.ClientOptions{Host: host, AuthToken: token, Timeout: time.Minute}, apiBaseURL)
}

func newClient(opts api.ClientOptions, baseURL string) (*Client, error) {
	hc, err := api.NewHTTPClient(opts)
	if err != nil {
		return nil, fmt.Errorf("create github http client: %w", err)
	}
	return &Client{http: hc, baseURL: baseURL, now: time.Now, log: slog.Default()}, nil
}

// SetLogger sets where warnings about truncated results go; the default is slog.Default().
func (c *Client) SetLogger(l *slog.Logger) {
	if l != nil {
		c.log = l
	}
}

// Viewer returns the authenticated user and the teams ("org/slug") they belong to.
func (c *Client) Viewer(ctx context.Context) (model.Viewer, error) {
	var me struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if _, err := c.graphQL(ctx, `query ViewerLogin { viewer { login } }`, nil, &me); err != nil {
		return model.Viewer{}, fmt.Errorf("fetch viewer: %w", err)
	}
	if me.Viewer.Login == "" {
		return model.Viewer{}, errors.New("fetch viewer: empty login")
	}

	var orgs struct {
		Viewer struct {
			Organizations struct {
				Nodes []*struct {
					Teams struct {
						Nodes []struct {
							CombinedSlug string `json:"combinedSlug"`
						} `json:"nodes"`
					} `json:"teams"`
				} `json:"nodes"`
			} `json:"organizations"`
		} `json:"viewer"`
	}
	// Partial errors are tolerated: organizations enforcing SAML SSO for which the token is not
	// authorized resolve to null and simply contribute no teams.
	if _, err := c.graphQL(ctx, viewerTeamsQuery, map[string]any{"login": me.Viewer.Login}, &orgs); err != nil {
		return model.Viewer{}, fmt.Errorf("fetch viewer teams: %w", err)
	}

	v := model.Viewer{Login: me.Viewer.Login}
	for _, org := range orgs.Viewer.Organizations.Nodes {
		if org == nil {
			continue
		}
		for _, t := range org.Teams.Nodes {
			if t.CombinedSlug != "" && !slices.Contains(v.Teams, t.CombinedSlug) {
				v.Teams = append(v.Teams, t.CombinedSlug)
			}
		}
	}
	return v, nil
}

const viewerTeamsQuery = `query ViewerTeams($login: String!) {
  viewer {
    organizations(first: 100) {
      nodes {
        teams(first: 100, userLogins: [$login]) { nodes { combinedSlug } }
      }
    }
  }
}`

type Notification struct {
	ThreadID  string
	Reason    string // GitHub notification reason, e.g. review_requested, author, comment, mention, subscribed
	PR        model.PRKey
	UpdatedAt time.Time
}

type NotificationsResult struct {
	Notifications []Notification // PullRequest subjects only
	LastModified  string         // pass to the next call
	PollInterval  time.Duration  // minimum interval requested by GitHub via X-Poll-Interval
	NotModified   bool           // GitHub answered 304
}

type restNotification struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"subject"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// Notifications lists read and unread notifications updated since `since` (all pages). lastModified
// from the previous result enables a cheap 304 when nothing changed.
func (c *Client) Notifications(ctx context.Context, since time.Time, lastModified string) (NotificationsResult, error) {
	q := url.Values{"all": {"true"}, "per_page": {"50"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	res := NotificationsResult{LastModified: lastModified}
	next := c.baseURL + "notifications?" + q.Encode()
	for first := true; next != ""; first = false {
		var header http.Header
		if first && lastModified != "" {
			header = http.Header{"If-Modified-Since": {lastModified}}
		}
		resp, err := c.get(ctx, next, header)
		if err != nil {
			return NotificationsResult{}, fmt.Errorf("list notifications: %w", err)
		}
		if first {
			res.PollInterval = pollInterval(resp.Header)
			if lm := resp.Header.Get("Last-Modified"); lm != "" {
				res.LastModified = lm
			}
			if resp.StatusCode == http.StatusNotModified {
				_ = resp.Body.Close()
				res.NotModified = true
				return res, nil
			}
		}
		var page []restNotification
		if err := decodeJSON(resp, &page); err != nil {
			return NotificationsResult{}, fmt.Errorf("decode notifications: %w", err)
		}
		for _, n := range page {
			if n.Subject.Type != "PullRequest" {
				continue
			}
			number, ok := prNumber(n.Subject.URL)
			if !ok {
				continue
			}
			res.Notifications = append(res.Notifications, Notification{
				ThreadID:  n.ID,
				Reason:    n.Reason,
				PR:        model.PRKey{Owner: n.Repository.Owner.Login, Repo: n.Repository.Name, Number: number},
				UpdatedAt: n.UpdatedAt,
			})
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return res, nil
}

func pollInterval(h http.Header) time.Duration {
	secs, err := strconv.Atoi(h.Get("X-Poll-Interval"))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// prNumber extracts the number from a pull request API URL such as
// https://api.github.com/repos/o/r/pulls/42.
func prNumber(apiURL string) (int, bool) {
	i := strings.LastIndex(apiURL, "/pulls/")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(apiURL[i+len("/pulls/"):])
	return n, err == nil && n > 0
}

// SearchOpenPRs returns the open PRs matching query (e.g. "involves:@me"); "is:pr is:open archived:false"
// is added. All result pages are read.
func (c *Client) SearchOpenPRs(ctx context.Context, query string) ([]model.PRKey, error) {
	q := url.Values{"q": {"is:pr is:open archived:false " + query}, "per_page": {"100"}}
	next := c.baseURL + "search/issues?" + q.Encode()
	var keys []model.PRKey
	seen := map[model.PRKey]bool{}
	for read := 0; next != "" && read < searchResultCap; {
		resp, err := c.get(ctx, next, nil)
		if err != nil {
			return nil, fmt.Errorf("search %q: %w", query, err)
		}
		var page struct {
			Items []struct {
				Number        int    `json:"number"`
				RepositoryURL string `json:"repository_url"`
			} `json:"items"`
		}
		if err := decodeJSON(resp, &page); err != nil {
			return nil, fmt.Errorf("decode search results: %w", err)
		}
		if len(page.Items) == 0 {
			break
		}
		for _, it := range page.Items {
			read++
			owner, repo, ok := ownerRepo(it.RepositoryURL)
			if !ok {
				continue
			}
			k := model.PRKey{Owner: owner, Repo: repo, Number: it.Number}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	return keys, nil
}

// ownerRepo splits a repository API URL such as https://api.github.com/repos/o/r.
func ownerRepo(repoURL string) (owner, repo string, ok bool) {
	_, path, ok := strings.Cut(repoURL, "/repos/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok = strings.Cut(path, "/")
	return owner, repo, ok && owner != "" && repo != "" && !strings.Contains(repo, "/")
}

// FetchPR fetches a full snapshot including reviews, review requests, check runs, all review threads
// and the most recent comments.
func (c *Client) FetchPR(ctx context.Context, key model.PRKey) (model.PR, error) {
	var data struct {
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			PullRequest *gqlPR `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": key.Owner, "repo": key.Repo, "number": key.Number}
	errs, err := c.graphQL(ctx, prQuery, vars, &data)
	if err := queryError(errs, err); err != nil {
		return model.PR{}, fmt.Errorf("fetch %s: %w", key, err)
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		return model.PR{}, fmt.Errorf("fetch %s: %w", key, ErrNotFound)
	}
	pr := data.Repository.PullRequest
	if err := c.fetchRemainingThreads(ctx, key, pr); err != nil {
		return model.PR{}, fmt.Errorf("fetch %s: %w", key, err)
	}
	canonical := model.PRKey{Owner: data.Repository.Owner.Login, Repo: data.Repository.Name, Number: key.Number}
	c.warnTruncated(canonical, pr)
	return pr.toModel(canonical, c.now().UTC()), nil
}

// fetchRemainingThreads appends the review thread pages the PR query did not return.
func (c *Client) fetchRemainingThreads(ctx context.Context, key model.PRKey, pr *gqlPR) error {
	for pr.ReviewThreads.PageInfo.HasNextPage {
		after := pr.ReviewThreads.PageInfo.EndCursor
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads gqlThreads `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": key.Owner, "repo": key.Repo, "number": key.Number, "after": after}
		errs, err := c.graphQL(ctx, threadsQuery, vars, &data)
		if err := queryError(errs, err); err != nil {
			return fmt.Errorf("fetch review threads: %w", err)
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return fmt.Errorf("fetch review threads: %w", ErrNotFound)
		}
		page := data.Repository.PullRequest.ReviewThreads
		if page.PageInfo.HasNextPage && (page.PageInfo.EndCursor == "" || page.PageInfo.EndCursor == after) {
			return fmt.Errorf("fetch review threads: cursor %q does not advance", after)
		}
		pr.ReviewThreads.Nodes = append(pr.ReviewThreads.Nodes, page.Nodes...)
		pr.ReviewThreads.PageInfo = page.PageInfo
	}
	return nil
}

// warnTruncated logs the lists the snapshot holds only a part of: the most recent 100 PR comments
// and the first 50 comments of a review thread.
func (c *Client) warnTruncated(key model.PRKey, pr *gqlPR) {
	if n := pr.Comments.TotalCount; n > len(pr.Comments.Nodes) {
		c.log.Warn("pull request comments truncated", "pr", key.String(), "fetched", len(pr.Comments.Nodes), "total", n)
	}
	for _, t := range pr.ReviewThreads.Nodes {
		if n := t.Comments.TotalCount; n > len(t.Comments.Nodes) {
			c.log.Warn("review thread comments truncated", "pr", key.String(), "thread", t.ID, "fetched", len(t.Comments.Nodes), "total", n)
		}
	}
}

// queryError turns the outcome of graphQL into an error for callers that reject partial results. It
// returns nil when the query succeeded without errors; persistent failures wrap ErrNotFound.
func queryError(errs []api.GraphQLErrorItem, err error) error {
	if err == nil && len(errs) > 0 {
		err = &api.GraphQLError{Errors: errs}
	}
	if err != nil && isInaccessible(err) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// isInaccessible reports persistent GraphQL errors: retrying a missing PR or one hidden by
// permissions or SAML enforcement does not help.
func isInaccessible(err error) bool {
	var gerr *api.GraphQLError
	if !errors.As(err, &gerr) {
		return false
	}
	return slices.ContainsFunc(gerr.Errors, func(e api.GraphQLErrorItem) bool {
		return e.Type == "NOT_FOUND" || e.Type == "FORBIDDEN"
	})
}

// FetchStatuses fetches the status part of many PRs in batched GraphQL queries. PRs that no longer
// exist or are inaccessible are omitted from the result.
func (c *Client) FetchStatuses(ctx context.Context, keys []model.PRKey) (map[model.PRKey]model.PRStatus, error) {
	out := make(map[model.PRKey]model.PRStatus, len(keys))
	for batch := range slices.Chunk(keys, statusBatchSize) {
		if err := c.fetchStatusesSplitting(ctx, batch, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fetchStatusesSplitting halves a batch GitHub fails to answer in time, down to single PRs.
func (c *Client) fetchStatusesSplitting(ctx context.Context, keys []model.PRKey, out map[model.PRKey]model.PRStatus) error {
	err := c.fetchStatusBatch(ctx, keys, out)
	if err == nil || len(keys) == 1 || !isTimeout(err) {
		return err
	}
	mid := len(keys) / 2
	if err := c.fetchStatusesSplitting(ctx, keys[:mid], out); err != nil {
		return err
	}
	return c.fetchStatusesSplitting(ctx, keys[mid:], out)
}

// isTimeout reports GitHub giving up on an expensive query: a gateway error or a GraphQL error
// mentioning a timeout. Smaller queries avoid it.
func isTimeout(err error) bool {
	var herr *api.HTTPError
	if errors.As(err, &herr) {
		switch herr.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	var gerr *api.GraphQLError
	return errors.As(err, &gerr) && slices.ContainsFunc(gerr.Errors, func(e api.GraphQLErrorItem) bool {
		return strings.Contains(strings.ToLower(e.Message), "timeout")
	})
}

func (c *Client) fetchStatusBatch(ctx context.Context, keys []model.PRKey, out map[model.PRKey]model.PRStatus) error {
	var params, fields strings.Builder
	vars := make(map[string]any, 3*len(keys))
	for i, k := range keys {
		if i > 0 {
			params.WriteString(", ")
		}
		fmt.Fprintf(&params, "$o%d: String!, $r%d: String!, $n%d: Int!", i, i, i)
		fmt.Fprintf(&fields, "  pr%d: repository(owner: $o%d, name: $r%d) { pullRequest(number: $n%d) { ...Status } }\n", i, i, i, i)
		vars[fmt.Sprintf("o%d", i)] = k.Owner
		vars[fmt.Sprintf("r%d", i)] = k.Repo
		vars[fmt.Sprintf("n%d", i)] = k.Number
	}
	query := "query Statuses(" + params.String() + ") {\n" + fields.String() + "}\n" + statusFragment

	var data map[string]*struct {
		PullRequest *gqlStatus `json:"pullRequest"`
	}
	errs, err := c.graphQL(ctx, query, vars, &data)
	if err != nil {
		return fmt.Errorf("fetch statuses: %w", err)
	}
	failed := map[string]bool{}
	for _, e := range errs {
		alias, ok := pathRoot(e)
		if !ok {
			return fmt.Errorf("fetch statuses: %w", &api.GraphQLError{Errors: errs})
		}
		failed[alias] = true
	}
	for i, k := range keys {
		alias := fmt.Sprintf("pr%d", i)
		r := data[alias]
		if failed[alias] || r == nil || r.PullRequest == nil {
			continue
		}
		out[k] = r.PullRequest.toModel()
	}
	return nil
}

func pathRoot(e api.GraphQLErrorItem) (string, bool) {
	if len(e.Path) == 0 {
		return "", false
	}
	s, ok := e.Path[0].(string)
	return s, ok
}

func (c *Client) get(ctx context.Context, rawURL string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if err := c.checkResponse(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// graphQL runs a query and decodes its data into out. Errors that come with data (partial results)
// are returned for the caller to judge; a response without data fails.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) ([]api.GraphQLErrorItem, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.checkResponse(resp); err != nil {
		return nil, err
	}
	var res struct {
		Data   json.RawMessage        `json:"data"`
		Errors []api.GraphQLErrorItem `json:"errors"`
	}
	if err := decodeJSON(resp, &res); err != nil {
		return nil, fmt.Errorf("decode graphql response: %w", err)
	}
	if slices.ContainsFunc(res.Errors, func(e api.GraphQLErrorItem) bool { return e.Type == "RATE_LIMITED" }) {
		return nil, &RateLimitError{Reset: retryAt(resp.Header, c.now())}
	}
	if len(res.Data) == 0 || string(res.Data) == "null" {
		if len(res.Errors) > 0 {
			return nil, &api.GraphQLError{Errors: res.Errors}
		}
		return nil, errors.New("graphql response without data")
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return nil, fmt.Errorf("decode graphql data: %w", err)
	}
	return res.Errors, nil
}

// checkResponse returns nil for 2xx responses. Otherwise it closes the body and returns a
// *RateLimitError or an *api.HTTPError.
func (c *Client) checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rateLimited(resp, body) {
		return &RateLimitError{Reset: retryAt(resp.Header, c.now())}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return api.HandleHTTPError(resp)
}

func rateLimited(resp *http.Response, body []byte) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("Retry-After") != "" ||
			resp.Header.Get("X-RateLimit-Remaining") == "0" ||
			bytes.Contains(bytes.ToLower(body), []byte("rate limit"))
	}
	return false
}

// retryAt follows GitHub's guidance: honor Retry-After, else the reset time of an exhausted primary
// limit, else wait one minute.
func retryAt(h http.Header, now time.Time) time.Time {
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return now.Add(time.Duration(secs) * time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(reset, 0)
		}
	}
	return now.Add(time.Minute)
}

func decodeJSON(resp *http.Response, v any) error {
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}

// nextLink returns the rel="next" URL of a Link header, or "" on the last page.
func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}
