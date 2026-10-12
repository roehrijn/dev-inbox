package github

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

// TestLive runs read-only calls against github.com with the gh CLI credentials. It logs counts only,
// never PR contents.
func TestLive(t *testing.T) {
	if os.Getenv("DEVINBOX_LIVE") != "1" {
		t.Skip("set DEVINBOX_LIVE=1 to run against github.com")
	}
	ctx := t.Context()
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}

	var warnings bytes.Buffer
	c.SetLogger(slog.New(slog.NewTextHandler(&warnings, nil)))

	v, err := c.Viewer(ctx)
	if err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	t.Logf("viewer: login=%s teams=%d", v.Login, len(v.Teams))

	if len(v.Teams) > 0 {
		members, err := c.TeamMembers(ctx, v.Teams[0])
		if err != nil {
			t.Fatalf("TeamMembers: %v", err)
		}
		distinct := map[string]bool{}
		for _, m := range members {
			distinct[m] = true
		}
		if len(members) == 0 || len(distinct) != len(members) {
			t.Errorf("TeamMembers: members=%d distinct=%d", len(members), len(distinct))
		}
		t.Logf("TeamMembers: members=%d viewerIncluded=%t", len(members), distinct[v.Login])
	}
	if _, err := c.TeamMembers(ctx, v.Login+"/dev-inbox-no-such-team-7f3a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TeamMembers of a nonexistent team: err = %v, want ErrNotFound", err)
	}

	since := time.Now().Add(-14 * 24 * time.Hour)
	n, err := c.Notifications(ctx, since, "")
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	reasons := map[string]int{}
	for _, x := range n.Notifications {
		reasons[x.Reason]++
	}
	t.Logf("notifications (14d): pr=%d reasons=%v pollInterval=%s lastModified=%t", len(n.Notifications), reasons, n.PollInterval, n.LastModified != "")
	n2, err := c.Notifications(ctx, since, n.LastModified)
	if err != nil {
		t.Fatalf("Notifications with If-Modified-Since: %v", err)
	}
	t.Logf("notifications repeat: notModified=%t pr=%d", n2.NotModified, len(n2.Notifications))

	keys, err := c.SearchOpenPRs(ctx, "involves:@me")
	if err != nil {
		t.Fatalf("SearchOpenPRs: %v", err)
	}
	t.Logf("search involves:@me: open prs=%d", len(keys))
	if len(keys) == 0 {
		t.Skip("no open PRs involving the viewer")
	}

	sample := slices.Clone(keys[:min(2, len(keys))])
	for _, q := range []string{"reviewed-by:@me", "review-requested:@me"} {
		more, err := c.SearchOpenPRs(ctx, q)
		if err != nil {
			t.Fatalf("SearchOpenPRs %s: %v", q, err)
		}
		t.Logf("search %s: open prs=%d", q, len(more))
		for _, k := range more[:min(2, len(more))] {
			if !slices.Contains(sample, k) {
				sample = append(sample, k)
			}
		}
	}
	var withReviewSHAs, reviewsWithSHA, threads, resolved, inlineWithSHA int
	for _, k := range sample {
		start := time.Now()
		pr, err := c.FetchPR(ctx, k)
		fetchElapsed := time.Since(start)
		if err != nil {
			t.Fatalf("FetchPR %s: %v", k, err)
		}
		if !strings.EqualFold(pr.Key.Owner, k.Owner) || !strings.EqualFold(pr.Key.Repo, k.Repo) || pr.Key.Number != k.Number {
			t.Errorf("FetchPR key = %s, want %s", pr.Key, k)
		}
		if pr.NodeID == "" || pr.HeadSHA == "" || pr.State != model.PRStateOpen || pr.Author == "" || pr.LastPushAt.IsZero() {
			t.Errorf("FetchPR %d: incomplete snapshot: node=%t head=%t state=%s author=%t lastPush=%t",
				k.Number, pr.NodeID != "", pr.HeadSHA != "", pr.State, pr.Author != "", !pr.LastPushAt.IsZero())
		}
		if !slices.IsSortedFunc(pr.Comments, func(a, b model.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) }) {
			t.Errorf("FetchPR %d: comments not sorted", k.Number)
		}
		kinds := map[model.CommentKind]int{}
		bots, replies, lines, reacted := 0, 0, 0, 0
		for _, cm := range pr.Comments {
			kinds[cm.Kind]++
			if len(cm.ViewerReactions) > 0 {
				reacted++
			}
			if slices.Contains(cm.ViewerReactions, "") {
				t.Errorf("FetchPR %d: comment with an empty reaction content", k.Number)
			}
			if strings.HasSuffix(cm.Author, "[bot]") {
				bots++
			}
			if cm.ReplyTo != "" {
				replies++
			}
			if cm.Kind == model.CommentInline && cm.Path != "" && cm.Line > 0 {
				lines++
			}
		}
		shas := 0
		for _, r := range pr.Reviews {
			if r.CommitSHA != "" {
				shas++
			}
		}
		if shas > 0 {
			withReviewSHAs++
		}
		reviewsWithSHA += shas
		threadIDs := map[string]bool{}
		for _, th := range pr.ReviewThreads {
			threadIDs[th.ID] = true
			if th.IsResolved {
				resolved++
			}
		}
		threads += len(pr.ReviewThreads)
		for _, cm := range pr.Comments {
			if cm.Kind != model.CommentInline {
				continue
			}
			if !threadIDs[cm.ThreadID] {
				t.Errorf("FetchPR %d: inline comment without a known thread", k.Number)
			}
			if cm.CommitSHA != "" {
				inlineWithSHA++
			}
		}
		teams := 0
		for _, r := range pr.ReviewRequests {
			if r.Team != "" {
				teams++
			}
		}
		if pr.Checks == model.ChecksNone && (pr.ChecksRunning != 0 || pr.ChecksFailing != 0) {
			t.Errorf("FetchPR %d: no checks but running=%d failing=%d", k.Number, pr.ChecksRunning, pr.ChecksFailing)
		}
		t.Logf("FetchPR: elapsed=%s reviewsWithSHA=%d/%d threads=%d", fetchElapsed.Round(time.Millisecond), shas, len(pr.Reviews), len(pr.ReviewThreads))
		t.Logf("FetchPR: checks=%q running=%d failing=%d checkRuns=%d reviews=%d reviewRequests=%d (teams=%d) comments=%v botComments=%d inlineWithPathLine=%d replies=%d reactedByViewer=%d labels=%d mergeable=%s mergeState=%s decision=%q",
			pr.Checks, pr.ChecksRunning, pr.ChecksFailing, len(pr.CheckRuns), len(pr.Reviews), len(pr.ReviewRequests), teams, kinds, bots, lines, replies, reacted, len(pr.Labels),
			pr.Mergeable, pr.MergeStateStatus, pr.ReviewDecision)
	}

	t.Logf("review data: prs=%d prsWithReviewSHAs=%d reviewsWithSHA=%d threads=%d resolved=%d inlineWithSHA=%d truncationWarnings=%d",
		len(sample), withReviewSHAs, reviewsWithSHA, threads, resolved, inlineWithSHA, bytes.Count(warnings.Bytes(), []byte("truncated")))

	missing := []model.PRKey{
		{Owner: v.Login, Repo: "dev-inbox-does-not-exist-7f3a", Number: 1},
		{Owner: sample[0].Owner, Repo: sample[0].Repo, Number: 999999999},
	}
	statuses, err := c.FetchStatuses(ctx, append(slices.Clone(sample), missing...))
	if err != nil {
		t.Fatalf("FetchStatuses: %v", err)
	}
	for _, k := range sample {
		if _, ok := statuses[k]; !ok {
			t.Errorf("FetchStatuses: missing %d", k.Number)
		}
	}
	for _, k := range missing {
		if _, ok := statuses[k]; ok {
			t.Errorf("FetchStatuses: nonexistent PR %s not omitted", k)
		}
	}
	t.Logf("FetchStatuses: requested=%d returned=%d", len(sample)+len(missing), len(statuses))

	batch := keys[:min(statusBatchSize, len(keys))]
	start := time.Now()
	statuses, err = c.FetchStatuses(ctx, batch)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("FetchStatuses batch: %v", err)
	}
	var withRunning, withFailing int
	for _, s := range statuses {
		if s.ChecksRunning > 0 {
			withRunning++
		}
		if s.ChecksFailing > 0 {
			withFailing++
		}
	}
	t.Logf("FetchStatuses batch: requested=%d returned=%d elapsed=%s withRunning=%d withFailing=%d",
		len(batch), len(statuses), elapsed.Round(time.Millisecond), withRunning, withFailing)
}
