// Package linear is a read-only client for the Linear GraphQL API.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/httpx"
)

const (
	// APIKeyEnv is the only place the API key is read from.
	APIKeyEnv      = "LINEAR_API_KEY"
	DefaultBaseURL = "https://api.linear.app/graphql"

	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 4 << 20
	maxErrorBody     = 200
	maxIdentifiers   = 10

	aliasAttached = "attached"
	aliasBranch   = "branch"

	issueFragment = `fragment I on Issue {
  identifier title url dueDate canceledAt completedAt
  state { name }
  project { name url targetDate completedAt canceledAt }
  projectMilestone { name targetDate }
}`
)

type Config struct {
	APIKey     string
	BaseURL    string       // DefaultBaseURL when empty
	HTTPClient *http.Client // a client with a 30s timeout when nil
	Logger     *slog.Logger
}

type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
	url  string
}

func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("linear: API key is required")
	}
	c := &Client{cfg: cfg, http: cfg.HTTPClient, log: cfg.Logger, url: cfg.BaseURL}
	if c.url == "" {
		c.url = DefaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout, Transport: httpx.Transport()}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// PRRef is what identifies a GitHub PR to Linear.
type PRRef struct {
	URL    string // https://github.com/<owner>/<repo>/pull/<number>
	Title  string
	Branch string // head ref
}

// Dates are Linear TimelessDates, "YYYY-MM-DD", or empty when unset.

type Issue struct {
	Identifier string // e.g. DEVOPS-123
	Title      string
	URL        string
	DueDate    string
	// State is the name of the workflow state, e.g. In Progress.
	State string
	// Done is true for completed and canceled issues.
	Done      bool
	Project   *Project   // nil without a project
	Milestone *Milestone // nil without a project milestone
}

type Project struct {
	Name       string
	URL        string
	TargetDate string
	// Done is true for completed and canceled projects.
	Done bool
}

type Milestone struct {
	Name       string
	TargetDate string
}

var identifierRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]{1,9}-[0-9]{1,7}\b`)

// IssuesForPR returns the issues linked to pr, deduplicated and sorted by identifier.
//
// Linear's GitHub integration links PRs as attachments, which also covers bodies saying "Closes ABC-123".
// The branch search and the identifiers in the title and branch cover PRs the integration has not linked.
func (c *Client) IssuesForPR(ctx context.Context, pr PRRef) ([]Issue, error) {
	ids := extractIdentifiers(pr.Title, pr.Branch)
	// Issue is non-null in the schema, so a server that follows the GraphQL spec nulls the whole data
	// object when an identifier such as "UTF-8" is unknown. Retrying once without the rejected identifiers
	// recovers the other sources.
	for attempt := 0; ; attempt++ {
		issues, rejected, err := c.query(ctx, pr, ids)
		if err != nil {
			return nil, err
		}
		if rejected == nil {
			return issues, nil
		}
		if attempt > 0 {
			return nil, errors.New("linear: response has no data")
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(rejected, id) })
	}
}

// extractIdentifiers returns the distinct upper-cased Linear identifiers in the texts, in order of
// appearance and capped at maxIdentifiers.
func extractIdentifiers(texts ...string) []string {
	var ids []string
	for _, t := range texts {
		for _, m := range identifierRe.FindAllString(t, -1) {
			m = strings.ToUpper(m)
			if !slices.Contains(ids, m) {
				ids = append(ids, m)
			}
		}
	}
	if len(ids) > maxIdentifiers {
		ids = ids[:maxIdentifiers]
	}
	return ids
}

func identifierAlias(i int) string { return "i" + strconv.Itoa(i) }

func buildQuery(withBranch bool, n int) string {
	decls := []string{"$url: String!"}
	fields := []string{aliasAttached + ": attachmentsForURL(url: $url) { nodes { issue { ...I } } }"}
	if withBranch {
		decls = append(decls, "$branch: String!")
		fields = append(fields, aliasBranch+": issueVcsBranchSearch(branchName: $branch) { ...I }")
	}
	for i := range n {
		a := identifierAlias(i)
		decls = append(decls, "$"+a+": String!")
		fields = append(fields, a+": issue(id: $"+a+") { ...I }")
	}
	return "query IssuesForPR(" + strings.Join(decls, ", ") + ") {\n  " + strings.Join(fields, "\n  ") +
		"\n}\n" + issueFragment
}

type graphQLError struct {
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

type graphQLResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []graphQLError             `json:"errors"`
}

type issueNode struct {
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	DueDate     string  `json:"dueDate"`
	CanceledAt  *string `json:"canceledAt"`
	CompletedAt *string `json:"completedAt"`
	State       *struct {
		Name string `json:"name"`
	} `json:"state"`
	Project *struct {
		Name        string  `json:"name"`
		URL         string  `json:"url"`
		TargetDate  string  `json:"targetDate"`
		CompletedAt *string `json:"completedAt"`
		CanceledAt  *string `json:"canceledAt"`
	} `json:"project"`
	ProjectMilestone *struct {
		Name       string `json:"name"`
		TargetDate string `json:"targetDate"`
	} `json:"projectMilestone"`
}

func (n *issueNode) issue() Issue {
	is := Issue{
		Identifier: n.Identifier,
		Title:      n.Title,
		URL:        n.URL,
		DueDate:    n.DueDate,
		Done:       n.CompletedAt != nil || n.CanceledAt != nil,
	}
	if n.State != nil {
		is.State = n.State.Name
	}
	if p := n.Project; p != nil {
		is.Project = &Project{
			Name:       p.Name,
			URL:        p.URL,
			TargetDate: p.TargetDate,
			Done:       p.CompletedAt != nil || p.CanceledAt != nil,
		}
	}
	if m := n.ProjectMilestone; m != nil {
		is.Milestone = &Milestone{Name: m.Name, TargetDate: m.TargetDate}
	}
	return is
}

// query sends one request. rejected is non-nil only when the server returned no data because it
// rejected identifiers, and then lists them.
func (c *Client) query(ctx context.Context, pr PRRef, ids []string) (issues []Issue, rejected []string, err error) {
	q := buildQuery(pr.Branch != "", len(ids))
	vars := map[string]any{"url": pr.URL}
	if pr.Branch != "" {
		vars["branch"] = pr.Branch
	}
	aliases := make(map[string]string, len(ids))
	for i, id := range ids {
		a := identifierAlias(i)
		vars[a] = id
		aliases[a] = id
	}

	resp, err := c.post(ctx, q, vars)
	if err != nil {
		return nil, nil, err
	}

	var ignored []string
	for _, e := range resp.Errors {
		if id, ok := aliases[firstPathString(e.Path)]; ok {
			c.log.Debug("linear: identifier rejected", "identifier", id, "message", e.Message)
			ignored = append(ignored, id)
			continue
		}
		return nil, nil, fmt.Errorf("linear: graphql error: %s", e.Message)
	}

	if len(resp.Data) == 0 {
		if len(ignored) == 0 {
			return nil, nil, errors.New("linear: response has no data")
		}
		return nil, ignored, nil
	}

	seen := map[string]Issue{}
	add := func(n *issueNode) {
		if n == nil || n.Identifier == "" {
			return
		}
		if _, ok := seen[n.Identifier]; !ok {
			seen[n.Identifier] = n.issue()
		}
	}

	var attached struct {
		Nodes []struct {
			Issue *issueNode `json:"issue"`
		} `json:"nodes"`
	}
	if err := decodeField(resp.Data, aliasAttached, &attached); err != nil {
		return nil, nil, err
	}
	for _, n := range attached.Nodes {
		add(n.Issue)
	}
	keys := []string{aliasBranch}
	for i := range ids {
		keys = append(keys, identifierAlias(i))
	}
	for _, key := range keys {
		var n *issueNode
		if err := decodeField(resp.Data, key, &n); err != nil {
			return nil, nil, err
		}
		add(n)
	}

	issues = make([]Issue, 0, len(seen))
	for _, is := range seen {
		issues = append(issues, is)
	}
	slices.SortFunc(issues, func(a, b Issue) int { return strings.Compare(a.Identifier, b.Identifier) })
	return issues, nil, nil
}

func firstPathString(path []any) string {
	if len(path) == 0 {
		return ""
	}
	s, _ := path[0].(string)
	return s
}

// decodeField decodes data[key] into v; an absent field decodes like null.
func decodeField(data map[string]json.RawMessage, key string, v any) error {
	raw, ok := data[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("linear: decode %s: %w", key, err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, query string, vars map[string]any) (*graphQLResponse, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("linear: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("linear: %w", err)
	}
	// Personal API keys are sent as is; only OAuth tokens take the Bearer prefix.
	req.Header.Set("Authorization", c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	c.log.Debug("linear request", "url", c.url, "variables", vars)
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("linear request error", "latency_ms", time.Since(start).Milliseconds(), "err", err)
		return nil, fmt.Errorf("linear: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	latency := time.Since(start).Milliseconds()
	if err != nil {
		c.log.Warn("linear request error", "status", resp.StatusCode, "latency_ms", latency, "err", err)
		return nil, fmt.Errorf("linear: read response: %w", err)
	}
	c.log.Debug("linear response", "status", resp.StatusCode, "latency_ms", latency)

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("linear: status %d: %s", resp.StatusCode, truncate(data))
		// Linear reports rate limiting as a 400 with a RATELIMITED error code.
		if resp.StatusCode == http.StatusTooManyRequests || bytes.Contains(data, []byte("RATELIMITED")) {
			msg = fmt.Sprintf("linear: rate limited (status %d): %s", resp.StatusCode, truncate(data))
		}
		return nil, errors.New(msg)
	}
	var out graphQLResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("linear: decode response: %w", err)
	}
	return &out, nil
}

func truncate(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorBody {
		s = strings.ToValidUTF8(s[:maxErrorBody], "") + "..."
	}
	return s
}
