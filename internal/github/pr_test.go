package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/roehrijn/dev-inbox/internal/model"
)

func at(hour, minute int) time.Time { return time.Date(2026, 10, 9, hour, minute, 0, 0, time.UTC) }

func TestFetchPR(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		if !strings.HasPrefix(req.Query, "query PullRequest(") {
			t.Errorf("unexpected query %q", req.Query)
		}
		for _, field := range []string{"checkRunCountsByState", "statusContextCountsByState"} {
			if !strings.Contains(req.Query, field) {
				t.Errorf("query lacks %s", field)
			}
		}
		want := map[string]any{"owner": "roehrijn", "repo": "dev-inbox", "number": float64(7)}
		if !reflect.DeepEqual(req.Variables, want) {
			t.Errorf("variables = %v, want %v", req.Variables, want)
		}
		_, _ = w.Write(readFixture(t, "pr.json"))
	}))

	got, err := c.FetchPR(t.Context(), model.PRKey{Owner: "roehrijn", Repo: "dev-inbox", Number: 7})
	if err != nil {
		t.Fatal(err)
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
			{ID: "R2", Author: "alice", State: "CHANGES_REQUESTED", SubmittedAt: at(10, 0)},
			{ID: "R4", Author: "bob", State: "COMMENTED", SubmittedAt: at(10, 30)},
		},
		CheckRuns: []model.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://ci/build"},
			{Name: "lint", Status: "IN_PROGRESS", URL: "https://ci/lint"},
			{Name: "ci/jenkins", Status: "ERROR", Conclusion: "ERROR", URL: "https://jenkins/1"},
			{Name: "deploy", Status: "PENDING"},
		},
		Comments: []model.Comment{
			{ID: "IC1", Kind: model.CommentIssue, Author: "alice", Body: "Why now?", URL: prURL + "#issuecomment-1", CreatedAt: at(8, 0)},
			{ID: "RC1", Kind: model.CommentInline, Author: "alice", Body: "Pin this.", URL: prURL + "#discussion_r1", Path: "go.mod", Line: 12, CreatedAt: at(9, 30)},
			{ID: "RC2", Kind: model.CommentInline, Author: "bob", Body: "Agreed.", URL: prURL + "#discussion_r2", Path: "go.mod", Line: 7, ReplyTo: "RC1", CreatedAt: at(9, 45)},
			{ID: "R2", Kind: model.CommentReview, Author: "alice", Body: "Please pin the version.", URL: prURL + "#pullrequestreview-2", CreatedAt: at(10, 0)},
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
		if gotRunning, gotFailing := r.counts(); gotRunning != wantRunning || gotFailing != wantFailing {
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
