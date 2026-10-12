package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testKey = "lin_api_secret"

type recordedRequest struct {
	Header http.Header
	Method string
	Query  string
	Vars   map[string]any
}

type fakeLinear struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

// newFakeLinear serves the given handlers in order, repeating the last one.
func newFakeLinear(t *testing.T, handlers ...http.HandlerFunc) *fakeLinear {
	t.Helper()
	f := &fakeLinear{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode body %q: %v", raw, err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{r.Header.Clone(), r.Method, body.Query, body.Variables})
		n := min(len(f.requests), len(handlers)) - 1
		f.mu.Unlock()
		handlers[n](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinear) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{APIKey: testKey, BaseURL: f.srv.URL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeLinear) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func respondStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func issueJSON(identifier string) string {
	return `{"identifier":"` + identifier + `","title":"t ` + identifier + `","url":"https://linear.app/x/issue/` + identifier +
		`","dueDate":null,"canceledAt":null,"completedAt":null,"state":{"name":"Todo"},"project":null,"projectMilestone":null}`
}

func identifiers(issues []Issue) []string {
	var out []string
	for _, is := range issues {
		out = append(out, is.Identifier)
	}
	return out
}

func TestNew(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected an error for an empty API key")
	}

	c, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if c.url != DefaultBaseURL {
		t.Errorf("url = %q, want %q", c.url, DefaultBaseURL)
	}
	if c.http.Timeout.Seconds() != 30 {
		t.Errorf("default timeout = %v, want 30s", c.http.Timeout)
	}

	custom := &http.Client{}
	c, err = New(Config{APIKey: testKey, HTTPClient: custom, BaseURL: "http://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if c.http != custom || c.url != "http://example.test" {
		t.Errorf("custom config not used: http=%p url=%q", c.http, c.url)
	}
}

func TestRequestShape(t *testing.T) {
	f := newFakeLinear(t, respondJSON(`{"data":{"attached":{"nodes":[]},"branch":null,"i0":null,"i1":null}}`))
	var logs bytes.Buffer
	c, err := New(Config{
		APIKey:  testKey,
		BaseURL: f.srv.URL,
		Logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}

	pr := PRRef{
		URL:    "https://github.com/acme-co/x/pull/7",
		Title:  "fix: handle devops-123 \" } injection",
		Branch: "janroehrich/doc-45-fix",
	}
	issues, err := c.IssuesForPR(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %v, want none", issues)
	}

	reqs := f.recorded()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost {
		t.Errorf("method = %s", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != testKey {
		t.Errorf("Authorization = %q, want the bare key", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	wantVars := map[string]any{
		"url":    pr.URL,
		"branch": pr.Branch,
		"i0":     "DEVOPS-123",
		"i1":     "DOC-45",
	}
	if !jsonEqual(t, r.Vars, wantVars) {
		t.Errorf("variables = %v, want %v", r.Vars, wantVars)
	}
	for _, want := range []string{
		"attachmentsForURL(url: $url)",
		"issueVcsBranchSearch(branchName: $branch)",
		"i0: issue(id: $i0) { ...I }",
		"i1: issue(id: $i1) { ...I }",
		"fragment I on Issue",
		"projectMilestone { name targetDate }",
		"state { name }",
	} {
		if !strings.Contains(r.Query, want) {
			t.Errorf("query lacks %q:\n%s", want, r.Query)
		}
	}
	for _, leaked := range []string{"DEVOPS-123", "injection", pr.URL, pr.Branch} {
		if strings.Contains(r.Query, leaked) {
			t.Errorf("query interpolates %q:\n%s", leaked, r.Query)
		}
	}
	if strings.Contains(logs.String(), testKey) {
		t.Errorf("API key was logged:\n%s", logs.String())
	}
}

func TestRequestOmitsUnusedFields(t *testing.T) {
	tests := []struct {
		name         string
		pr           PRRef
		wantBranch   bool
		wantAliasI0  bool
		wantVarNames []string
	}{
		{"no branch", PRRef{URL: "u", Title: "ABC-1 thing"}, false, true, []string{"url", "i0"}},
		{"no identifiers", PRRef{URL: "u", Title: "plain", Branch: "main"}, true, false, []string{"url", "branch"}},
		{"neither", PRRef{URL: "u", Title: "plain"}, false, false, []string{"url"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLinear(t, respondJSON(`{"data":{"attached":{"nodes":[]}}}`))
			if _, err := f.client(t).IssuesForPR(context.Background(), tt.pr); err != nil {
				t.Fatal(err)
			}
			r := f.recorded()[0]
			if got := strings.Contains(r.Query, "issueVcsBranchSearch"); got != tt.wantBranch {
				t.Errorf("branch field present = %v, want %v:\n%s", got, tt.wantBranch, r.Query)
			}
			if got := strings.Contains(r.Query, "i0: issue("); got != tt.wantAliasI0 {
				t.Errorf("i0 alias present = %v, want %v:\n%s", got, tt.wantAliasI0, r.Query)
			}
			var names []string
			for k := range r.Vars {
				names = append(names, k)
			}
			slices.Sort(names)
			slices.Sort(tt.wantVarNames)
			if !slices.Equal(names, tt.wantVarNames) {
				t.Errorf("variable names = %v, want %v", names, tt.wantVarNames)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	aj, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bj, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(aj, bj)
}

func TestMergeDedupeSort(t *testing.T) {
	body := `{"data":{
	  "attached":{"nodes":[{"issue":` + issueJSON("DOC-2") + `},{"issue":` + issueJSON("ABC-10") + `}]},
	  "branch":` + issueJSON("ABC-10") + `,
	  "i0":` + issueJSON("ABC-9") + `,
	  "i1":` + issueJSON("DOC-2") + `
	}}`
	f := newFakeLinear(t, respondJSON(body))
	issues, err := f.client(t).IssuesForPR(context.Background(), PRRef{
		URL: "u", Title: "abc-9 and doc-2", Branch: "x/abc-10",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ABC-10", "ABC-9", "DOC-2"}
	if got := identifiers(issues); !slices.Equal(got, want) {
		t.Errorf("identifiers = %v, want %v", got, want)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestIssueMapping(t *testing.T) {
	tests := []struct {
		name string
		node string
		want Issue
	}{
		{
			name: "open without project",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":"2026-11-01","canceledAt":null,"completedAt":null,"project":null,"projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", DueDate: "2026-11-01"},
		},
		{
			name: "state",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":null,"completedAt":null,"state":{"name":"In Progress"},"project":null,"projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", State: "In Progress"},
		},
		{
			name: "completed",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":null,"completedAt":"2026-10-01T10:00:00.000Z","project":null,"projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", Done: true},
		},
		{
			name: "canceled",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":"2026-10-01T10:00:00.000Z","completedAt":null,"project":null,"projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", Done: true},
		},
		{
			name: "project and milestone",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":null,"completedAt":null,
			  "project":{"name":"P","url":"PU","targetDate":"2026-12-31","completedAt":null,"canceledAt":null},
			  "projectMilestone":{"name":"M","targetDate":"2026-11-30"}}`,
			want: Issue{
				Identifier: "A-1", Title: "T", URL: "U",
				Project:   &Project{Name: "P", URL: "PU", TargetDate: "2026-12-31"},
				Milestone: &Milestone{Name: "M", TargetDate: "2026-11-30"},
			},
		},
		{
			name: "completed project",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":null,"completedAt":null,
			  "project":{"name":"P","url":"PU","targetDate":null,"completedAt":"2026-09-01T00:00:00.000Z","canceledAt":null},
			  "projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", Project: &Project{Name: "P", URL: "PU", Done: true}},
		},
		{
			name: "canceled project",
			node: `{"identifier":"A-1","title":"T","url":"U","dueDate":null,"canceledAt":null,"completedAt":null,
			  "project":{"name":"P","url":"PU","targetDate":null,"completedAt":null,"canceledAt":"2026-09-01T00:00:00.000Z"},
			  "projectMilestone":null}`,
			want: Issue{Identifier: "A-1", Title: "T", URL: "U", Project: &Project{Name: "P", URL: "PU", Done: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLinear(t, respondJSON(`{"data":{"attached":{"nodes":[{"issue":`+tt.node+`}]}}}`))
			issues, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u"})
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 1 {
				t.Fatalf("got %d issues, want 1", len(issues))
			}
			if !jsonEqual(t, issues[0], tt.want) {
				got, _ := json.Marshal(issues[0])
				want, _ := json.Marshal(tt.want)
				t.Errorf("issue = %s, want %s", got, want)
			}
		})
	}
}

func TestIgnoredIdentifierErrors(t *testing.T) {
	body := `{
	  "errors":[
	    {"message":"Entity not found: Issue","path":["i0"],"extensions":{"type":"invalid input"}},
	    {"message":"Entity not found: Issue","path":["i2"]}
	  ],
	  "data":{"attached":{"nodes":[{"issue":` + issueJSON("DOC-1") + `}]},"branch":null,"i0":null,"i1":` +
		issueJSON("ABC-5") + `,"i2":null}
	}`
	f := newFakeLinear(t, respondJSON(body))
	issues, err := f.client(t).IssuesForPR(context.Background(), PRRef{
		URL: "u", Title: "UTF-8 abc-5 CVE-2024", Branch: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := identifiers(issues), []string{"ABC-5", "DOC-1"}; !slices.Equal(got, want) {
		t.Errorf("identifiers = %v, want %v", got, want)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestNullDataFromRejectedIdentifiersRetries(t *testing.T) {
	first := `{"errors":[{"message":"Entity not found: Issue","path":["i0"]}],"data":null}`
	second := `{"data":{"attached":{"nodes":[{"issue":` + issueJSON("DOC-1") + `}]},"i0":` + issueJSON("ABC-5") + `}}`
	f := newFakeLinear(t, respondJSON(first), respondJSON(second))
	issues, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u", Title: "UTF-8 ABC-5"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := identifiers(issues), []string{"ABC-5", "DOC-1"}; !slices.Equal(got, want) {
		t.Errorf("identifiers = %v, want %v", got, want)
	}
	reqs := f.recorded()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := reqs[1].Vars["i0"]; got != "ABC-5" {
		t.Errorf("retry i0 = %v, want ABC-5", got)
	}
	if _, ok := reqs[1].Vars["i1"]; ok {
		t.Errorf("retry still sends the rejected identifier: %v", reqs[1].Vars)
	}
}

func TestNullDataAgainAfterRetryFails(t *testing.T) {
	body := `{"errors":[{"message":"Entity not found: Issue","path":["i0"]}],"data":null}`
	f := newFakeLinear(t, respondJSON(body))
	if _, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u", Title: "UTF-8"}); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(f.recorded()); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestFailingGraphQLResponses(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "error on attachments",
			body:    `{"errors":[{"message":"boom","path":["attached"]}],"data":{"attached":null}}`,
			wantErr: "boom",
		},
		{
			name:    "error on branch search",
			body:    `{"errors":[{"message":"nope","path":["branch"]}],"data":{"attached":{"nodes":[]},"branch":null}}`,
			wantErr: "nope",
		},
		{
			name:    "error without path",
			body:    `{"errors":[{"message":"Syntax Error"}],"data":null}`,
			wantErr: "Syntax Error",
		},
		{
			name: "ignored error mixed with a failing one",
			body: `{"errors":[{"message":"Entity not found","path":["i0"]},{"message":"auth failed","path":["attached"]}],` +
				`"data":null}`,
			wantErr: "auth failed",
		},
		{
			name:    "alias unknown to the request",
			body:    `{"errors":[{"message":"stray","path":["i7"]}],"data":{"attached":{"nodes":[]}}}`,
			wantErr: "stray",
		},
		{
			name:    "null data without errors",
			body:    `{"data":null}`,
			wantErr: "no data",
		},
		{
			name:    "missing data",
			body:    `{}`,
			wantErr: "no data",
		},
		{
			name:    "invalid json",
			body:    `not json`,
			wantErr: "decode response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLinear(t, respondJSON(tt.body))
			issues, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u", Title: "ABC-1"})
			if err == nil {
				t.Fatalf("expected an error, got issues %v", issues)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPStatusErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantContain []string
		wantAbsent  []string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"errors":[{"message":"AUTHENTICATION_ERROR"}]}`,
			[]string{"401", "AUTHENTICATION_ERROR"}, []string{"rate limited"}},
		{"rate limited", http.StatusTooManyRequests, `slow down`,
			[]string{"rate limited", "429", "slow down"}, nil},
		{"rate limited as 400", http.StatusBadRequest, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`,
			[]string{"rate limited", "400"}, nil},
		{"server error with long body", http.StatusInternalServerError, strings.Repeat("x", 5000),
			[]string{"500", strings.Repeat("x", maxErrorBody) + "..."}, []string{strings.Repeat("x", maxErrorBody+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLinear(t, respondStatus(tt.status, tt.body))
			_, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u"})
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, s := range tt.wantContain {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
			if n := len(f.recorded()); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
		})
	}
}

func TestContextCanceled(t *testing.T) {
	f := newFakeLinear(t, respondJSON(`{"data":{"attached":{"nodes":[]}}}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.client(t).IssuesForPR(ctx, PRRef{URL: "u"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExtractIdentifiers(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  []string
	}{
		{"title", []string{"fix: DEVOPS-123 thing"}, []string{"DEVOPS-123"}},
		{"lowercase branch", []string{"", "devops-123/fix-x"}, []string{"DEVOPS-123"}},
		{"user prefix and suffix", []string{"", "user/abc-12-foo"}, []string{"ABC-12"}},
		{"title before branch, deduplicated", []string{"DOC-2 and abc-1", "me/doc-2-and-xyz-9"},
			[]string{"DOC-2", "ABC-1", "XYZ-9"}},
		{"alphanumeric key", []string{"AB2-77"}, []string{"AB2-77"}},
		{"punctuation around", []string{"(abc-1), [def-2]: ghi-3."}, []string{"ABC-1", "DEF-2", "GHI-3"}},
		{"no match", []string{"no ids here", "main"}, nil},
		{"single letter key", []string{"a-1"}, nil},
		{"key too long", []string{"ABCDEFGHIJK-1"}, nil},
		{"number too long", []string{"ABC-12345678"}, nil},
		{"word char before", []string{"x_abc-1", "9abc-1"}, nil},
		{"no cross-text match", []string{"ABC-", "1 DEF"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIdentifiers(tt.texts...)
			if !slices.Equal(got, tt.want) {
				t.Errorf("extractIdentifiers(%q) = %v, want %v", tt.texts, got, tt.want)
			}
		})
	}
}

func TestExtractIdentifiersCap(t *testing.T) {
	got := extractIdentifiers(numberedIdentifiers(15), "ZZ-1")
	if len(got) != maxIdentifiers {
		t.Fatalf("got %d identifiers, want %d: %v", len(got), maxIdentifiers, got)
	}
	if got[0] != "AB-1" || got[maxIdentifiers-1] != "AB-10" {
		t.Errorf("identifiers = %v, want the first ten in order of appearance", got)
	}
}

func numberedIdentifiers(n int) string {
	var s strings.Builder
	for i := 1; i <= n; i++ {
		s.WriteString("AB-" + strconv.Itoa(i) + " ")
	}
	return s.String()
}

func TestIdentifierCapInRequest(t *testing.T) {
	title := numberedIdentifiers(14)
	f := newFakeLinear(t, respondJSON(`{"data":{"attached":{"nodes":[]}}}`))
	if _, err := f.client(t).IssuesForPR(context.Background(), PRRef{URL: "u", Title: title}); err != nil {
		t.Fatal(err)
	}
	r := f.recorded()[0]
	if got := strings.Count(r.Query, ": issue(id: $"); got != maxIdentifiers {
		t.Errorf("issue aliases = %d, want %d", got, maxIdentifiers)
	}
	if _, ok := r.Vars["i10"]; ok {
		t.Error("variable i10 must not be sent")
	}
}
