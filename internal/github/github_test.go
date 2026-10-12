package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/roehrijn/dev-inbox/internal/model"
)

var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := newClient(api.ClientOptions{Host: "127.0.0.1", AuthToken: "test-token", LogIgnoreEnv: true}, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	return c, srv
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func decodeGQL(t *testing.T, r *http.Request) gqlRequest {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		t.Errorf("graphql request = %s %s, want POST /graphql", r.Method, r.URL.Path)
	}
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode graphql request: %v", err)
	}
	return req
}

func TestNotifications(t *testing.T) {
	const lastModified = "Fri, 10 Oct 2026 09:00:00 GMT"
	since := time.Date(2026, 10, 1, 2, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	var srvURL string
	var requests atomic.Int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/notifications" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("all") != "true" || q.Get("per_page") != "50" || q.Get("since") != "2026-10-01T00:00:00Z" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		switch q.Get("page") {
		case "":
			if got := r.Header.Get("If-Modified-Since"); got != lastModified {
				t.Errorf("page 1 If-Modified-Since = %q, want %q", got, lastModified)
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/notifications?all=true&per_page=50&since=2026-10-01T00%%3A00%%3A00Z&page=2>; rel="next", <%s/notifications?page=2>; rel="last"`, srvURL, srvURL))
			w.Header().Set("X-Poll-Interval", "90")
			w.Header().Set("Last-Modified", "Sat, 11 Oct 2026 10:00:00 GMT")
			_, _ = w.Write(readFixture(t, "notifications_page1.json"))
		case "2":
			if got := r.Header.Get("If-Modified-Since"); got != "" {
				t.Errorf("page 2 If-Modified-Since = %q, want none", got)
			}
			w.Header().Set("X-Poll-Interval", "30")
			w.Header().Set("Last-Modified", "Sat, 11 Oct 2026 10:00:05 GMT")
			_, _ = w.Write(readFixture(t, "notifications_page2.json"))
		default:
			t.Errorf("unexpected page %q", q.Get("page"))
		}
	}))
	srvURL = srv.URL

	got, err := c.Notifications(t.Context(), since, lastModified)
	if err != nil {
		t.Fatal(err)
	}
	want := NotificationsResult{
		Notifications: []Notification{
			{
				ThreadID:  "1001",
				Reason:    "review_requested",
				PR:        model.PRKey{Owner: "acme-co", Repo: "frontend", Number: 42},
				UpdatedAt: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC),
			},
			{
				ThreadID:  "2001",
				Reason:    "subscribed",
				PR:        model.PRKey{Owner: "RoehriJN", Repo: "Dev-Inbox", Number: 7},
				UpdatedAt: time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC),
			},
		},
		LastModified: "Sat, 11 Oct 2026 10:00:00 GMT",
		PollInterval: 90 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Notifications =\n%+v\nwant\n%+v", got, want)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestNotificationsNotModified(t *testing.T) {
	tests := []struct {
		name         string
		lastModified string
		respHeader   map[string]string
		want         NotificationsResult
	}{
		{
			name:         "keeps previous Last-Modified",
			lastModified: "Fri, 10 Oct 2026 09:00:00 GMT",
			respHeader:   map[string]string{"X-Poll-Interval": "120"},
			want:         NotificationsResult{LastModified: "Fri, 10 Oct 2026 09:00:00 GMT", PollInterval: 2 * time.Minute, NotModified: true},
		},
		{
			name:         "adopts returned Last-Modified",
			lastModified: "Fri, 10 Oct 2026 09:00:00 GMT",
			respHeader:   map[string]string{"X-Poll-Interval": "60", "Last-Modified": "Fri, 10 Oct 2026 09:00:00 GMT"},
			want:         NotificationsResult{LastModified: "Fri, 10 Oct 2026 09:00:00 GMT", PollInterval: time.Minute, NotModified: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("If-Modified-Since"); got != tt.lastModified {
					t.Errorf("If-Modified-Since = %q", got)
				}
				for k, v := range tt.respHeader {
					w.Header().Set(k, v)
				}
				w.WriteHeader(http.StatusNotModified)
			}))
			got, err := c.Notifications(t.Context(), testNow.Add(-time.Hour), tt.lastModified)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Notifications = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNotificationsWithoutSinceOrLastModified(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["since"]; ok {
			t.Errorf("since sent for zero time: %s", r.URL.RawQuery)
		}
		if _, ok := r.Header["If-Modified-Since"]; ok {
			t.Error("If-Modified-Since sent without lastModified")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	got, err := c.Notifications(t.Context(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.NotModified || len(got.Notifications) != 0 || got.PollInterval != 0 {
		t.Errorf("Notifications = %+v", got)
	}
}

func searchItem(owner, repo string, number int) map[string]any {
	return map[string]any{
		"number":         number,
		"repository_url": "https://api.github.com/repos/" + owner + "/" + repo,
		"pull_request":   map[string]any{},
	}
}

func TestSearchOpenPRs(t *testing.T) {
	var srvURL string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search/issues" || q.Get("q") != "is:pr is:open archived:false involves:@me" || q.Get("per_page") != "100" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		var items []map[string]any
		switch q.Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/search/issues?q=%s&per_page=100&page=2>; rel="next"`, srvURL, url.QueryEscape(q.Get("q"))))
			items = []map[string]any{searchItem("acme-co", "frontend", 1), searchItem("RoehriJN", "Dev-Inbox", 2)}
		case "2":
			items = []map[string]any{searchItem("acme-co", "frontend", 1), searchItem("acme-co", "frontend", 3)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 4, "incomplete_results": false, "items": items})
	}))
	srvURL = srv.URL

	got, err := c.SearchOpenPRs(t.Context(), "involves:@me")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.PRKey{
		{Owner: "acme-co", Repo: "frontend", Number: 1},
		{Owner: "RoehriJN", Repo: "Dev-Inbox", Number: 2},
		{Owner: "acme-co", Repo: "frontend", Number: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SearchOpenPRs = %v, want %v", got, want)
	}
}

func TestSearchOpenPRsStopsAtResultCap(t *testing.T) {
	var srvURL string
	var requests atomic.Int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := int(requests.Add(1))
		w.Header().Set("Link", fmt.Sprintf(`<%s/search/issues?page=%d>; rel="next"`, srvURL, page+1))
		items := make([]map[string]any, 100)
		for i := range items {
			items[i] = searchItem("o", "r", (page-1)*100+i+1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	srvURL = srv.URL

	got, err := c.SearchOpenPRs(t.Context(), "involves:@me")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != searchResultCap || requests.Load() != searchResultCap/100 {
		t.Errorf("results = %d, requests = %d; want %d results in %d requests", len(got), requests.Load(), searchResultCap, searchResultCap/100)
	}
}

func TestFetchStatuses(t *testing.T) {
	var batchSizes []int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		if !strings.Contains(req.Query, "fragment Status on PullRequest") {
			t.Error("query lacks the Status fragment")
		}
		for _, field := range []string{"checkRunCountsByState", "statusContextCountsByState"} {
			if !strings.Contains(req.Query, field) {
				t.Errorf("query lacks %s", field)
			}
		}
		if strings.Contains(req.Query, "evil") {
			t.Error("owner value leaked into the query text instead of variables")
		}
		n := len(req.Variables) / 3
		batchSizes = append(batchSizes, n)
		data := map[string]any{}
		var errs []map[string]any
		for i := range n {
			alias := fmt.Sprintf("pr%d", i)
			number := int(req.Variables[fmt.Sprintf("n%d", i)].(float64))
			switch {
			case number%10 == 0:
				data[alias] = map[string]any{"pullRequest": nil}
				errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest", "path": []any{alias, "pullRequest"}})
			case number == 13:
				data[alias] = nil
				errs = append(errs, map[string]any{"type": "FORBIDDEN", "message": "SAML enforcement", "path": []any{alias}})
			default:
				data[alias] = map[string]any{"pullRequest": statusJSON(number)}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
	}))

	const total = 125
	var keys []model.PRKey
	for n := 1; n <= total; n++ {
		keys = append(keys, model.PRKey{Owner: "evil\"} owner", Repo: "r", Number: n})
	}
	got, err := c.FetchStatuses(t.Context(), keys)
	if err != nil {
		t.Fatal(err)
	}
	var wantSizes []int
	for rest := total; rest > 0; rest -= statusBatchSize {
		wantSizes = append(wantSizes, min(rest, statusBatchSize))
	}
	if !reflect.DeepEqual(batchSizes, wantSizes) {
		t.Errorf("batch sizes = %v, want %v", batchSizes, wantSizes)
	}
	for _, k := range keys {
		_, ok := got[k]
		if missing := k.Number%10 == 0 || k.Number == 13; ok == missing {
			t.Errorf("PR %d present = %t, want %t", k.Number, ok, !missing)
		}
	}
	if want := total - total/10 - 1; len(got) != want {
		t.Errorf("len = %d, want %d", len(got), want)
	}

	updated := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	want := map[int]model.PRStatus{
		1: {State: model.PRStateOpen, HeadSHA: "sha1", UpdatedAt: updated, Checks: model.ChecksSuccess, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED"},
		3: {State: model.PRStateOpen, HeadSHA: "sha3", UpdatedAt: updated, Checks: model.ChecksPending, ChecksRunning: 3, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"},
		4: {State: model.PRStateOpen, IsDraft: true, HeadSHA: "sha4", UpdatedAt: updated, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED"},
		7: {State: model.PRStateOpen, HeadSHA: "sha7", UpdatedAt: updated, Checks: model.ChecksFailure, ChecksFailing: 3, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED", InMergeQueue: true},
		9: {State: model.PRStateOpen, HeadSHA: "sha9", UpdatedAt: updated, Checks: model.ChecksFailure, ChecksRunning: 1, ChecksFailing: 1, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"},
	}
	for n, w := range want {
		if s := got[keys[n-1]]; s != w {
			t.Errorf("status %d = %+v, want %+v", n, s, w)
		}
	}
}

// statusJSON derives a PR status from its number: even numbers are drafts, multiples of 3 have no
// review decision, multiples of 4 have no checks, 3 has running checks, 7 failing ones, 9 both,
// and 7 is in the merge queue.
func statusJSON(n int) map[string]any {
	var decision any = "APPROVED"
	if n%3 == 0 {
		decision = nil
	}
	rollup := rollupJSON("SUCCESS", map[string]int{"SUCCESS": 2, "SKIPPED": 1}, nil)
	switch {
	case n%4 == 0:
		rollup = nil
	case n == 3:
		rollup = rollupJSON("PENDING", map[string]int{"IN_PROGRESS": 1, "QUEUED": 1, "SUCCESS": 1, "NOT_YET_INVENTED": 5}, map[string]int{"EXPECTED": 1})
	case n == 7:
		rollup = rollupJSON("FAILURE", map[string]int{"FAILURE": 1, "TIMED_OUT": 1, "SUCCESS": 4}, map[string]int{"ERROR": 1})
	case n == 9:
		rollup = rollupJSON("FAILURE", map[string]int{"CANCELLED": 1}, map[string]int{"PENDING": 1})
	}
	return map[string]any{
		"state":            "OPEN",
		"isDraft":          n%2 == 0,
		"headRefOid":       "sha" + strconv.Itoa(n),
		"updatedAt":        "2026-10-10T12:00:00Z",
		"mergeable":        "MERGEABLE",
		"mergeStateStatus": "CLEAN",
		"reviewDecision":   decision,
		"isInMergeQueue":   n == 7,
		"rollupCommit":     map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": rollup}}}},
	}
}

// rollupJSON builds a statusCheckRollup with the given check run and status context counts by state.
func rollupJSON(state string, checkRuns, statusContexts map[string]int) map[string]any {
	counts := func(byState map[string]int) []any {
		out := []any{}
		for s, n := range byState {
			out = append(out, map[string]any{"state": s, "count": n})
		}
		return out
	}
	return map[string]any{
		"state": state,
		"contexts": map[string]any{
			"checkRunCountsByState":      counts(checkRuns),
			"statusContextCountsByState": counts(statusContexts),
		},
	}
}

func TestFetchStatusesSplitsOnTimeout(t *testing.T) {
	const timeoutBody = `{"data": null, "errors": [{"message": "Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug."}]}`
	tests := []struct {
		name string
		// fail returns the status and body of a failed response for a batch of n PRs, or 0 to answer.
		fail      func(n int) (int, string)
		wantSizes []int
		wantErr   bool
	}{
		{
			name: "gateway errors split until batches succeed",
			fail: func(n int) (int, string) {
				if n > 3 {
					return http.StatusBadGateway, "502 Bad Gateway"
				}
				return 0, ""
			},
			wantSizes: []int{10, 5, 2, 3, 5, 2, 3},
		},
		{
			name: "GraphQL timeout splits",
			fail: func(n int) (int, string) {
				if n > 5 {
					return http.StatusOK, timeoutBody
				}
				return 0, ""
			},
			wantSizes: []int{10, 5, 5},
		},
		{
			name:      "a single PR that keeps failing returns the error",
			fail:      func(int) (int, string) { return http.StatusGatewayTimeout, "504 Gateway Timeout" },
			wantSizes: []int{10, 5, 2, 1},
			wantErr:   true,
		},
		{
			name:      "other errors do not split",
			fail:      func(int) (int, string) { return http.StatusInternalServerError, `{"message": "boom"}` },
			wantSizes: []int{10},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sizes []int
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := decodeGQL(t, r)
				n := len(req.Variables) / 3
				sizes = append(sizes, n)
				if status, body := tt.fail(n); status != 0 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
					return
				}
				data := map[string]any{}
				for i := range n {
					number := int(req.Variables[fmt.Sprintf("n%d", i)].(float64))
					data[fmt.Sprintf("pr%d", i)] = map[string]any{"pullRequest": statusJSON(number)}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))

			var keys []model.PRKey
			for n := 1; n <= statusBatchSize; n++ {
				keys = append(keys, model.PRKey{Owner: "o", Repo: "r", Number: n})
			}
			got, err := c.FetchStatuses(t.Context(), keys)
			if !reflect.DeepEqual(sizes, tt.wantSizes) {
				t.Errorf("batch sizes = %v, want %v", sizes, tt.wantSizes)
			}
			if tt.wantErr {
				if err == nil {
					t.Error("FetchStatuses succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range keys {
				if s, ok := got[k]; !ok || s.HeadSHA != "sha"+strconv.Itoa(k.Number) {
					t.Errorf("status of %d = %+v, %t", k.Number, s, ok)
				}
			}
		})
	}
}

func TestFetchStatusesFailsOnQueryError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"pr0": null}, "errors": [{"message": "something broke"}]}`))
	}))
	if _, err := c.FetchStatuses(t.Context(), []model.PRKey{{Owner: "o", Repo: "r", Number: 1}}); err == nil {
		t.Error("FetchStatuses succeeded despite an error without path")
	}
}

func TestFetchStatusesEmpty(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected request")
	}))
	got, err := c.FetchStatuses(t.Context(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("FetchStatuses(nil) = %v, %v", got, err)
	}
}

func TestViewer(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		switch {
		case strings.HasPrefix(req.Query, "query ViewerLogin"):
			_, _ = w.Write([]byte(`{"data": {"viewer": {"login": "roehrijn"}}}`))
		case strings.HasPrefix(req.Query, "query ViewerTeams"):
			if req.Variables["login"] != "roehrijn" {
				t.Errorf("login variable = %v", req.Variables["login"])
			}
			_, _ = w.Write([]byte(`{
				"data": {"viewer": {"organizations": {"nodes": [
					{"teams": {"nodes": [{"combinedSlug": "acme-co/control-plane"}, {"combinedSlug": "acme-co/eng"}]}},
					null,
					{"teams": {"nodes": [{"combinedSlug": "other/devs"}, {"combinedSlug": "acme-co/eng"}]}}
				]}}},
				"errors": [{"type": "FORBIDDEN", "message": "SAML enforcement", "path": ["viewer", "organizations", "nodes", 1, "teams"]}]
			}`))
		default:
			t.Errorf("unexpected query %q", req.Query)
		}
	}))
	got, err := c.Viewer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := model.Viewer{Login: "roehrijn", Teams: []string{"acme-co/control-plane", "acme-co/eng", "other/devs"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Viewer = %+v, want %+v", got, want)
	}
}

func TestRateLimit(t *testing.T) {
	reset := testNow.Add(30 * time.Minute).Truncate(time.Second)
	tests := []struct {
		name      string
		graphQL   bool
		status    int
		header    map[string]string
		body      string
		wantReset time.Time // zero: not a rate limit error
		wantHTTP  int       // status of the expected *api.HTTPError when not rate limited
	}{
		{
			name:      "primary REST limit uses X-RateLimit-Reset",
			status:    http.StatusForbidden,
			header:    map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)},
			body:      `{"message": "API rate limit exceeded for user ID 1."}`,
			wantReset: reset,
		},
		{
			name:      "429 honors Retry-After",
			status:    http.StatusTooManyRequests,
			header:    map[string]string{"Retry-After": "30", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)},
			wantReset: testNow.Add(30 * time.Second),
		},
		{
			name:      "secondary limit without headers waits a minute",
			status:    http.StatusForbidden,
			body:      `{"message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			wantReset: testNow.Add(time.Minute),
		},
		{
			name:     "403 permission error is not a rate limit",
			status:   http.StatusForbidden,
			header:   map[string]string{"X-RateLimit-Remaining": "4999"},
			body:     `{"message": "Resource not accessible by integration"}`,
			wantHTTP: http.StatusForbidden,
		},
		{
			name:     "500 is not a rate limit",
			status:   http.StatusInternalServerError,
			body:     `{"message": "boom"}`,
			wantHTTP: http.StatusInternalServerError,
		},
		{
			name:      "GraphQL RATE_LIMITED uses X-RateLimit-Reset",
			graphQL:   true,
			status:    http.StatusOK,
			header:    map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)},
			body:      `{"data": null, "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`,
			wantReset: reset,
		},
		{
			name:      "GraphQL secondary limit honors Retry-After",
			graphQL:   true,
			status:    http.StatusForbidden,
			header:    map[string]string{"Retry-After": "60"},
			body:      `{"message": "You have exceeded a secondary rate limit."}`,
			wantReset: testNow.Add(time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			var err error
			if tt.graphQL {
				_, err = c.FetchPR(t.Context(), model.PRKey{Owner: "o", Repo: "r", Number: 1})
			} else {
				_, err = c.SearchOpenPRs(t.Context(), "involves:@me")
			}
			var rl *RateLimitError
			isRL := errors.As(err, &rl)
			if tt.wantReset.IsZero() {
				var herr *api.HTTPError
				if isRL || !errors.As(err, &herr) || herr.StatusCode != tt.wantHTTP {
					t.Fatalf("err = %v, want *api.HTTPError %d", err, tt.wantHTTP)
				}
				return
			}
			if !isRL {
				t.Fatalf("err = %v, want *RateLimitError", err)
			}
			if !rl.Reset.Equal(tt.wantReset) {
				t.Errorf("Reset = %s, want %s", rl.Reset, tt.wantReset)
			}
		})
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", ""},
		{`<https://api.github.com/notifications?page=2>; rel="next", <https://api.github.com/notifications?page=5>; rel="last"`, "https://api.github.com/notifications?page=2"},
		{`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=3>; rel="next"`, "https://api.github.com/x?page=3"},
		{`<https://api.github.com/x?page=1>; rel="first", <https://api.github.com/x?page=4>; rel="prev"`, ""},
	}
	for _, tt := range tests {
		if got := nextLink(tt.header); got != tt.want {
			t.Errorf("nextLink(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestPRNumber(t *testing.T) {
	tests := []struct {
		url    string
		want   int
		wantOK bool
	}{
		{"https://api.github.com/repos/o/r/pulls/42", 42, true},
		{"https://api.github.com/repos/o/r/issues/42", 0, false},
		{"https://api.github.com/repos/o/r/pulls/abc", 0, false},
		{"https://api.github.com/repos/o/r/pulls/0", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := prNumber(tt.url)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("prNumber(%q) = %d, %t; want %d, %t", tt.url, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestOwnerRepo(t *testing.T) {
	tests := []struct {
		url               string
		wantOwner, wantRe string
		wantOK            bool
	}{
		{"https://api.github.com/repos/acme-co/frontend", "acme-co", "frontend", true},
		{"https://api.github.com/repos/acme-co", "", "", false},
		{"https://api.github.com/repos/acme-co/frontend/extra", "", "", false},
		{"https://api.github.com/users/acme-co", "", "", false},
	}
	for _, tt := range tests {
		owner, repo, ok := ownerRepo(tt.url)
		if ok != tt.wantOK || (ok && (owner != tt.wantOwner || repo != tt.wantRe)) {
			t.Errorf("ownerRepo(%q) = %q, %q, %t", tt.url, owner, repo, ok)
		}
	}
}
