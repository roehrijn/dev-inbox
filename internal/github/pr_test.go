package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/roehrijn/dev-inbox/internal/model"
)

func at(hour, minute int) time.Time { return time.Date(2026, 10, 9, hour, minute, 0, 0, time.UTC) }

// logRecords captures what the client logs as decoded JSON records.
func logRecords(c *Client) func() []map[string]any {
	var buf bytes.Buffer
	c.SetLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	return func() []map[string]any {
		var out []map[string]any
		for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				panic(err)
			}
			out = append(out, rec)
		}
		return out
	}
}

func TestFetchPR(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		if !strings.HasPrefix(req.Query, "query PullRequest(") {
			t.Errorf("unexpected query %q", req.Query)
		}
		for _, field := range []string{"checkRunCountsByState", "statusContextCountsByState", "isResolved", "resolvedBy", "commit { oid }", "originalCommit { oid }"} {
			if !strings.Contains(req.Query, field) {
				t.Errorf("query lacks %s", field)
			}
		}
		// review summaries, issue comments and inline comments
		if n := strings.Count(req.Query, "reactionGroups { content viewerHasReacted }"); n != 3 {
			t.Errorf("query selects the viewer's reactions %d times, want 3", n)
		}
		want := map[string]any{"owner": "roehrijn", "repo": "dev-inbox", "number": float64(7)}
		if !reflect.DeepEqual(req.Variables, want) {
			t.Errorf("variables = %v, want %v", req.Variables, want)
		}
		_, _ = w.Write(readFixture(t, "pr.json"))
	}))

	logs := logRecords(c)
	got, err := c.FetchPR(t.Context(), model.PRKey{Owner: "roehrijn", Repo: "dev-inbox", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if recs := logs(); len(recs) != 0 {
		t.Errorf("FetchPR logged %v, want nothing", recs)
	}
	const prURL = "https://github.com/RoehriJN/Dev-Inbox/pull/7"
	want := model.PR{
		Key: model.PRKey{Owner: "RoehriJN", Repo: "Dev-Inbox", Number: 7},
		PRStatus: model.PRStatus{
			State:            model.PRStateOpen,
			HeadSHA:          "abc123",
			UpdatedAt:        time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
			Checks:           model.ChecksFailure,
			ChecksRunning:    4,
			ChecksFailing:    2,
			Mergeable:        "CONFLICTING",
			MergeStateStatus: "DIRTY",
			ReviewDecision:   "CHANGES_REQUESTED",
		},
		NodeID:       "PR_kwDO1",
		Title:        "bump deps",
		URL:          prURL,
		Author:       "renovate[bot]",
		BaseRef:      "main",
		HeadRef:      "renovate/deps",
		Labels:       []string{"dependencies", "ci"},
		Assignees:    []string{"alice"},
		Additions:    10,
		Deletions:    3,
		ChangedFiles: 2,
		CreatedAt:    time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
		LastPushAt:   at(7, 0),
		ReviewRequests: []model.ReviewRequest{
			{Login: "bob"},
			{Team: "acme-co/control-plane"},
			{Login: "copilot-pull-request-reviewer[bot]"},
		},
		Reviews: []model.Review{
			{ID: "R1", Author: "ghost", State: "APPROVED", SubmittedAt: at(9, 0)},
			{ID: "R2", Author: "alice", State: "CHANGES_REQUESTED", SubmittedAt: at(10, 0), CommitSHA: "abc122"},
			{ID: "R4", Author: "bob", State: "COMMENTED", SubmittedAt: at(10, 30), CommitSHA: "abc123"},
		},
		ReviewThreads: []model.ReviewThread{
			{ID: "PRRT_1"},
			{ID: "PRRT_2", IsResolved: true, ResolvedBy: "alice"},
			{ID: "PRRT_3", IsResolved: true, ResolvedBy: "github-actions[bot]"},
		},
		CheckRuns: []model.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://ci/build"},
			{Name: "lint", Status: "IN_PROGRESS", URL: "https://ci/lint"},
			{Name: "ci/jenkins", Status: "ERROR", Conclusion: "ERROR", URL: "https://jenkins/1"},
			{Name: "deploy", Status: "PENDING"},
		},
		Comments: []model.Comment{
			{ID: "IC1", Kind: model.CommentIssue, Author: "alice", Body: "Why now?", URL: prURL + "#issuecomment-1", ViewerReactions: []string{"THUMBS_UP", "HEART"}, CreatedAt: at(8, 0)},
			{ID: "RC1", Kind: model.CommentInline, Author: "alice", Body: "Pin this.", URL: prURL + "#discussion_r1", Path: "go.mod", Line: 12, ThreadID: "PRRT_1", CommitSHA: "abc121", ViewerReactions: []string{"THUMBS_UP"}, CreatedAt: at(9, 30)},
			{ID: "RC2", Kind: model.CommentInline, Author: "bob", Body: "Agreed.", URL: prURL + "#discussion_r2", Path: "go.mod", Line: 7, ReplyTo: "RC1", ThreadID: "PRRT_1", CreatedAt: at(9, 45)},
			{ID: "R2", Kind: model.CommentReview, Author: "alice", Body: "Please pin the version.", URL: prURL + "#pullrequestreview-2", ViewerReactions: []string{"EYES"}, CreatedAt: at(10, 0)},
			{ID: "RC4", Kind: model.CommentInline, Author: "alice", Body: "Typo.", URL: prURL + "#discussion_r4", Path: "README.md", Line: 3, ThreadID: "PRRT_2", CommitSHA: "abc122", ViewerReactions: []string{"ROCKET", "EYES"}, CreatedAt: at(10, 15)},
			{ID: "RC5", Kind: model.CommentInline, Author: "copilot-pull-request-reviewer[bot]", Body: "Consider a constant.", URL: prURL + "#discussion_r5", Path: "main.go", ThreadID: "PRRT_3", CommitSHA: "abc122", CreatedAt: at(10, 20)},
			{ID: "IC2", Kind: model.CommentIssue, Author: "github-actions[bot]", Body: "Coverage report", URL: prURL + "#issuecomment-2", CreatedAt: at(11, 0)},
		},
		FetchedAt: testNow,
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("FetchPR =\n%s\nwant\n%s", g, w)
	}
}

func TestFetchPRErrors(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantNotFound bool
	}{
		{
			name:         "missing repository",
			body:         `{"data": {"repository": null}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a Repository", "path": ["repository"]}]}`,
			wantNotFound: true,
		},
		{
			name:         "missing pull request",
			body:         `{"data": {"repository": {"name": "r", "owner": {"login": "o"}, "pullRequest": null}}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest", "path": ["repository", "pullRequest"]}]}`,
			wantNotFound: true,
		},
		{
			name:         "no data at all",
			body:         `{"data": null, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a Repository"}]}`,
			wantNotFound: true,
		},
		{
			name:         "repository behind SAML enforcement",
			body:         `{"data": {"repository": null}, "errors": [{"type": "FORBIDDEN", "message": "Resource protected by organization SAML enforcement", "path": ["repository"]}]}`,
			wantNotFound: true,
		},
		{
			name:         "forbidden nested field",
			body:         `{"data": {"repository": {"name": "r", "owner": {"login": "o"}, "pullRequest": {"state": "OPEN"}}}, "errors": [{"type": "FORBIDDEN", "message": "Resource protected by organization SAML enforcement", "path": ["repository", "pullRequest", "reviewRequests"]}]}`,
			wantNotFound: true,
		},
		{
			name: "transient partial error is rejected",
			body: `{"data": {"repository": {"name": "r", "owner": {"login": "o"}, "pullRequest": {"state": "OPEN"}}}, "errors": [{"message": "Something went wrong while executing your query.", "path": ["repository", "pullRequest", "reviewThreads"]}]}`,
		},
		{
			name: "query error",
			body: `{"errors": [{"message": "Field 'bogus' doesn't exist on type 'PullRequest'"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			_, err := c.FetchPR(t.Context(), model.PRKey{Owner: "o", Repo: "r", Number: 1})
			if err == nil {
				t.Fatal("FetchPR succeeded")
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(%v, ErrNotFound) = %t, want %t", err, got, tt.wantNotFound)
			}
			var gerr *api.GraphQLError
			if !errors.As(err, &gerr) {
				t.Errorf("err = %v, want it to wrap *api.GraphQLError", err)
			}
		})
	}
}

func TestViewerReactions(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{"absent", `{}`, nil},
		{"null", `{"reactionGroups": null}`, nil},
		{"empty", `{"reactionGroups": []}`, nil},
		{"others only", `{"reactionGroups": [{"content": "THUMBS_UP", "viewerHasReacted": false}, {"content": "EYES", "viewerHasReacted": false}]}`, nil},
		{
			name: "keeps GraphQL order",
			json: `{"reactionGroups": [{"content": "THUMBS_UP", "viewerHasReacted": false}, {"content": "LAUGH", "viewerHasReacted": true}, {"content": "HEART", "viewerHasReacted": false}, {"content": "EYES", "viewerHasReacted": true}]}`,
			want: []string{"LAUGH", "EYES"},
		},
	}
	for _, tt := range tests {
		var n struct {
			Reactions gqlReactions `json:"reactionGroups"`
		}
		if err := json.Unmarshal([]byte(tt.json), &n); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := n.Reactions.viewer(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: viewer() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name  string
		actor *gqlActor
		want  string
	}{
		{"deleted user", nil, "ghost"},
		{"user", &gqlActor{Typename: "User", Login: "alice"}, "alice"},
		{"bot gets suffix", &gqlActor{Typename: "Bot", Login: "dependabot"}, "dependabot[bot]"},
		{"bot suffix not doubled", &gqlActor{Typename: "Bot", Login: "dependabot[bot]"}, "dependabot[bot]"},
		{"mannequin", &gqlActor{Typename: "Mannequin", Login: "imported"}, "imported"},
	}
	for _, tt := range tests {
		if got := login(tt.actor); got != tt.want {
			t.Errorf("%s: login = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCollapseChecks(t *testing.T) {
	tests := map[string]model.ChecksState{
		"":         model.ChecksNone,
		"SUCCESS":  model.ChecksSuccess,
		"FAILURE":  model.ChecksFailure,
		"ERROR":    model.ChecksFailure,
		"PENDING":  model.ChecksPending,
		"EXPECTED": model.ChecksPending,
	}
	for in, want := range tests {
		if got := collapseChecks(in); got != want {
			t.Errorf("collapseChecks(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRollupCounts(t *testing.T) {
	const (
		neither = iota
		running
		failing
	)
	checkRuns := map[string]int{
		"QUEUED": running, "IN_PROGRESS": running, "PENDING": running, "WAITING": running, "REQUESTED": running,
		"FAILURE": failing, "TIMED_OUT": failing, "CANCELLED": failing, "STARTUP_FAILURE": failing, "ACTION_REQUIRED": failing,
		"SUCCESS": neither, "NEUTRAL": neither, "SKIPPED": neither, "STALE": neither, "COMPLETED": neither, "NOT_YET_INVENTED": neither,
	}
	statusContexts := map[string]int{
		"PENDING": running, "EXPECTED": running,
		"FAILURE": failing, "ERROR": failing,
		"SUCCESS": neither, "NOT_YET_INVENTED": neither,
	}
	check := func(kind, state string, r gqlRollup, bucket int) {
		t.Helper()
		var wantRunning, wantFailing int
		switch bucket {
		case running:
			wantRunning = 3
		case failing:
			wantFailing = 3
		}
		if gotRunning, gotFailing, _ := r.counts(); gotRunning != wantRunning || gotFailing != wantFailing {
			t.Errorf("%s %s: running=%d failing=%d, want %d and %d", kind, state, gotRunning, gotFailing, wantRunning, wantFailing)
		}
	}
	for state, bucket := range checkRuns {
		var r gqlRollup
		r.Contexts.CheckRunCountsByState = []gqlStateCount{{State: state, Count: 3}}
		check("check run", state, r, bucket)
	}
	for state, bucket := range statusContexts {
		var r gqlRollup
		r.Contexts.StatusContextCountsByState = []gqlStateCount{{State: state, Count: 3}}
		check("status context", state, r, bucket)
	}
}

func TestStatusChecksFromJSON(t *testing.T) {
	tests := []struct {
		name        string
		rollupNodes string
		want        model.PRStatus
	}{
		{name: "null rollup", rollupNodes: `[{"commit": {"statusCheckRollup": null}}]`},
		{name: "no commit", rollupNodes: `[]`},
		{
			name:        "rollup without checks",
			rollupNodes: `[{"commit": {"statusCheckRollup": {"state": "PENDING", "contexts": {"checkRunCountsByState": [], "statusContextCountsByState": []}}}}]`,
			want:        model.PRStatus{Checks: model.ChecksPending},
		},
		{
			name: "check runs and status contexts add up",
			rollupNodes: `[{"commit": {"statusCheckRollup": {"state": "FAILURE", "contexts": {
				"checkRunCountsByState": [{"state": "FAILURE", "count": 2}, {"state": "IN_PROGRESS", "count": 1}, {"state": "SUCCESS", "count": 9}],
				"statusContextCountsByState": [{"state": "ERROR", "count": 1}, {"state": "EXPECTED", "count": 2}, {"state": "SUCCESS", "count": 4}]}}}}]`,
			want: model.PRStatus{Checks: model.ChecksFailure, ChecksRunning: 3, ChecksFailing: 3},
		},
		{
			name: "rerun supersedes a cancelled run",
			rollupNodes: `[{"commit": {"statusCheckRollup": {"state": "FAILURE", "contexts": {
				"checkRunCountsByState": [{"state": "CANCELLED", "count": 1}, {"state": "SUCCESS", "count": 3}],
				"statusContextCountsByState": [],
				"nodes": [
					{"__typename": "CheckRun", "databaseId": 2, "name": "lint", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"workflowRun": {"workflow": {"name": "Commitlint"}}}},
					{"__typename": "CheckRun", "databaseId": 1, "name": "lint", "status": "COMPLETED", "conclusion": "CANCELLED", "checkSuite": {"workflowRun": {"workflow": {"name": "Commitlint"}}}},
					{"__typename": "CheckRun", "databaseId": 3, "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"workflowRun": {"workflow": {"name": "CI"}}}},
					{"__typename": "CheckRun", "databaseId": 4, "name": "lint", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"workflowRun": null}}
				]}}}}]`,
			want: model.PRStatus{Checks: model.ChecksSuccess},
		},
		{
			name: "rerun still running supersedes a failed run",
			rollupNodes: `[{"commit": {"statusCheckRollup": {"state": "FAILURE", "contexts": {
				"checkRunCountsByState": [{"state": "FAILURE", "count": 1}, {"state": "IN_PROGRESS", "count": 1}],
				"statusContextCountsByState": [],
				"nodes": [
					{"__typename": "CheckRun", "databaseId": 1, "name": "e2e", "status": "COMPLETED", "conclusion": "FAILURE"},
					{"__typename": "CheckRun", "databaseId": 2, "name": "e2e", "status": "IN_PROGRESS", "conclusion": null}
				]}}}}]`,
			want: model.PRStatus{Checks: model.ChecksPending, ChecksRunning: 1},
		},
		{
			name: "same job name in different workflows is not a rerun",
			rollupNodes: `[{"commit": {"statusCheckRollup": {"state": "FAILURE", "contexts": {
				"checkRunCountsByState": [{"state": "FAILURE", "count": 1}, {"state": "SUCCESS", "count": 1}],
				"statusContextCountsByState": [],
				"nodes": [
					{"__typename": "CheckRun", "databaseId": 1, "name": "lint", "status": "COMPLETED", "conclusion": "FAILURE", "checkSuite": {"workflowRun": {"workflow": {"name": "Go"}}}},
					{"__typename": "CheckRun", "databaseId": 2, "name": "lint", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"workflowRun": {"workflow": {"name": "Docs"}}}}
				]}}}}]`,
			want: model.PRStatus{Checks: model.ChecksFailure, ChecksFailing: 1},
		},
	}
	for _, tt := range tests {
		var s gqlStatus
		if err := json.Unmarshal([]byte(`{"rollupCommit": {"nodes": `+tt.rollupNodes+`}}`), &s); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := s.toModel(); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestCheckContextToModel(t *testing.T) {
	tests := []struct {
		name string
		in   gqlCheckContext
		want model.CheckRun
	}{
		{
			name: "completed check run",
			in:   gqlCheckContext{Typename: "CheckRun", Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "u"},
			want: model.CheckRun{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "u"},
		},
		{
			name: "successful status context",
			in:   gqlCheckContext{Typename: "StatusContext", Context: "ci", State: "SUCCESS", TargetURL: "u"},
			want: model.CheckRun{Name: "ci", Status: "SUCCESS", Conclusion: "SUCCESS", URL: "u"},
		},
		{
			name: "pending status context has no conclusion",
			in:   gqlCheckContext{Typename: "StatusContext", Context: "ci", State: "PENDING"},
			want: model.CheckRun{Name: "ci", Status: "PENDING"},
		},
		{
			name: "expected status context has no conclusion",
			in:   gqlCheckContext{Typename: "StatusContext", Context: "ci", State: "EXPECTED"},
			want: model.CheckRun{Name: "ci", Status: "EXPECTED"},
		},
	}
	for _, tt := range tests {
		if got := tt.in.toModel(); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func threadJSON(id, resolvedBy string, comments int) string {
	resolved := "null"
	if resolvedBy != "" {
		resolved = `{"__typename": "User", "login": "` + resolvedBy + `"}`
	}
	nodes := make([]string, comments)
	for i := range nodes {
		nodes[i] = `{"id": "` + id + `-c` + string(rune('a'+i)) + `", "author": {"__typename": "User", "login": "alice"}, "createdAt": "2026-10-09T09:00:00Z", "path": "a.go", "line": 1, "state": "SUBMITTED", "replyTo": null, "originalCommit": {"oid": "sha-` + id + `"}}`
	}
	return `{"id": "` + id + `", "isResolved": ` + strconv.FormatBool(resolvedBy != "") + `, "resolvedBy": ` + resolved +
		`, "comments": {"totalCount": ` + strconv.Itoa(comments) + `, "nodes": [` + strings.Join(nodes, ",") + `]}}`
}

func threadsPageJSON(hasNext bool, cursor string, threads ...string) string {
	return `{"pageInfo": {"hasNextPage": ` + strconv.FormatBool(hasNext) + `, "endCursor": "` + cursor + `"}, "nodes": [` + strings.Join(threads, ",") + `]}`
}

func prWithThreadsJSON(threadsPage string) string {
	return `{"data": {"repository": {"name": "r", "owner": {"login": "o"}, "pullRequest": {"state": "OPEN", "reviewThreads": ` + threadsPage + `}}}}`
}

func TestFetchPRPaginatesReviewThreads(t *testing.T) {
	var queries []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		queries = append(queries, req.Query)
		base := map[string]any{"owner": "o", "repo": "r", "number": float64(1)}
		switch len(queries) {
		case 1:
			if !reflect.DeepEqual(req.Variables, base) {
				t.Errorf("page 1 variables = %v, want %v", req.Variables, base)
			}
			_, _ = w.Write([]byte(prWithThreadsJSON(threadsPageJSON(true, "C1", threadJSON("T1", "", 1)))))
		case 2:
			want := map[string]any{"owner": "o", "repo": "r", "number": float64(1), "after": "C1"}
			if !reflect.DeepEqual(req.Variables, want) {
				t.Errorf("page 2 variables = %v, want %v", req.Variables, want)
			}
			_, _ = w.Write([]byte(`{"data": {"repository": {"pullRequest": {"reviewThreads": ` +
				threadsPageJSON(false, "C2", threadJSON("T2", "bob", 2), threadJSON("T3", "", 1)) + `}}}}`))
		default:
			t.Errorf("unexpected request %d", len(queries))
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	got, err := c.FetchPR(t.Context(), model.PRKey{Owner: "o", Repo: "r", Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("requests = %d, want 2", len(queries))
	}
	if !strings.HasPrefix(queries[0], "query PullRequest(") || !strings.HasPrefix(queries[1], "query ReviewThreads(") {
		t.Errorf("queries = %.30q and %.30q", queries[0], queries[1])
	}
	wantThreads := []model.ReviewThread{{ID: "T1"}, {ID: "T2", IsResolved: true, ResolvedBy: "bob"}, {ID: "T3"}}
	if !reflect.DeepEqual(got.ReviewThreads, wantThreads) {
		t.Errorf("ReviewThreads = %+v, want %+v", got.ReviewThreads, wantThreads)
	}
	threadOf := map[string]string{}
	for _, cm := range got.Comments {
		threadOf[cm.ID] = cm.ThreadID + "@" + cm.CommitSHA
	}
	wantComments := map[string]string{"T1-ca": "T1@sha-T1", "T2-ca": "T2@sha-T2", "T2-cb": "T2@sha-T2", "T3-ca": "T3@sha-T3"}
	if !reflect.DeepEqual(threadOf, wantComments) {
		t.Errorf("comment threads = %v, want %v", threadOf, wantComments)
	}
}

func TestFetchPRThreadPageErrors(t *testing.T) {
	tests := []struct {
		name         string
		page2        string
		wantNotFound bool
		wantErr      string
	}{
		{
			name:         "pull request vanished",
			page2:        `{"data": {"repository": {"pullRequest": null}}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest", "path": ["repository", "pullRequest"]}]}`,
			wantNotFound: true,
		},
		{
			name:    "transient error",
			page2:   `{"errors": [{"message": "Something went wrong while executing your query."}]}`,
			wantErr: "Something went wrong",
		},
		{
			name:    "cursor does not advance",
			page2:   `{"data": {"repository": {"pullRequest": {"reviewThreads": ` + threadsPageJSON(true, "C1", threadJSON("T2", "", 1)) + `}}}}`,
			wantErr: "does not advance",
		},
		{
			name:    "next page without cursor",
			page2:   `{"data": {"repository": {"pullRequest": {"reviewThreads": ` + threadsPageJSON(true, "", threadJSON("T2", "", 1)) + `}}}}`,
			wantErr: "does not advance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests int
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					_, _ = w.Write([]byte(prWithThreadsJSON(threadsPageJSON(true, "C1", threadJSON("T1", "", 1)))))
					return
				}
				_, _ = w.Write([]byte(tt.page2))
			}))
			_, err := c.FetchPR(t.Context(), model.PRKey{Owner: "o", Repo: "r", Number: 1})
			if err == nil {
				t.Fatal("FetchPR succeeded")
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(%v, ErrNotFound) = %t, want %t", err, got, tt.wantNotFound)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if requests != 2 {
				t.Errorf("requests = %d, want 2", requests)
			}
		})
	}
}

func TestFetchPRWarnsAboutTruncatedComments(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		threads := threadsPageJSON(false, "C1",
			strings.Replace(threadJSON("T1", "", 1), `"totalCount": 1`, `"totalCount": 51`, 1),
			threadJSON("T2", "", 2),
		)
		body := strings.Replace(prWithThreadsJSON(threads), `"state": "OPEN",`,
			`"state": "OPEN", "comments": {"totalCount": 150, "nodes": [{"id": "IC1", "author": null, "createdAt": "2026-10-09T08:00:00Z"}]},`, 1)
		_, _ = w.Write([]byte(body))
	}))
	logs := logRecords(c)
	if _, err := c.FetchPR(t.Context(), model.PRKey{Owner: "o", Repo: "r", Number: 1}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"level": "WARN", "msg": "pull request comments truncated", "pr": "o/r#1", "fetched": float64(1), "total": float64(150)},
		{"level": "WARN", "msg": "review thread comments truncated", "pr": "o/r#1", "thread": "T1", "fetched": float64(1), "total": float64(51)},
	}
	got := logs()
	for _, rec := range got {
		delete(rec, "time")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("log records = %v, want %v", got, want)
	}
}
