package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/model"
)

func review(author, state string) model.Review {
	return model.Review{Author: author, State: state}
}

func TestReviewVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		reviews []model.Review
		want    string
	}{
		{"none", nil, ""},
		{"latest verdict wins", []model.Review{review("bob", "APPROVED"), review("bob", "CHANGES_REQUESTED")}, "bob:CHANGES_REQUESTED"},
		{"approval after change request", []model.Review{review("bob", "CHANGES_REQUESTED"), review("bob", "APPROVED")}, "bob:APPROVED"},
		{"comment keeps the verdict", []model.Review{review("bob", "APPROVED"), review("Bob", "COMMENTED")}, "bob:APPROVED"},
		{"verdict replaces a comment", []model.Review{review("bob", "COMMENTED"), review("bob", "APPROVED")}, "bob:APPROVED"},
		{"comments only", []model.Review{review("bob", "COMMENTED"), review("bob", "COMMENTED")}, "bob:COMMENTED"},
		{"dismissal replaces the verdict", []model.Review{review("bob", "APPROVED"), review("bob", "DISMISSED")}, "bob:DISMISSED"},
		{"author left out", []model.Review{review("Alice", "COMMENTED"), review("carol", "APPROVED")}, "carol:APPROVED"},
		{"order of first review", []model.Review{review("carol", "COMMENTED"), review("bob", "APPROVED"), review("carol", "APPROVED")}, "carol:APPROVED bob:APPROVED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range reviewVerdicts(model.PR{Author: "alice", Reviews: tt.reviews}) {
				got = append(got, r.Author+":"+r.State)
			}
			if g := strings.Join(got, " "); g != tt.want {
				t.Errorf("verdicts = %q, want %q", g, tt.want)
			}
		})
	}
}

func ownItemWithReviews() (it model.PR) {
	it.Author = "alice"
	it.ReviewDecision = "REVIEW_REQUIRED"
	it.Reviews = []model.Review{
		review("dave", "COMMENTED"),
		review("erin", "APPROVED"),
		review("carol", "CHANGES_REQUESTED"),
		review("bob", "CHANGES_REQUESTED"),
		review("frank", "APPROVED"),
		review("alice", "COMMENTED"),
	}
	// bob is asked again after requesting changes.
	it.ReviewRequests = []model.ReviewRequest{{Team: "acme-co/eng"}, {Login: "bob"}}
	return it
}

func TestRowReviewSummaryOnOwnPRs(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	a.viewer = model.Viewer{Login: "Alice"}
	it := item(prKey("o", "r", 1), model.SectionWaitingForReview, "Mine", time.Hour)
	it.PR = ownItemWithReviews()
	it.PR.Key = prKey("o", "r", 1)

	want := "✗ carol · ✓ erin, frank · ● dave · awaiting acme-co/eng, bob"
	wide := a.renderRow(it, false, 140)
	if got := ansi.Strip(wide[1]); !strings.HasSuffix(got, want) {
		t.Errorf("wide line 2 = %q, want suffix %q", got, want)
	}
	if strings.Contains(ansi.Strip(wide[1]), "review required") {
		t.Errorf("decision shown next to the summary: %q", ansi.Strip(wide[1]))
	}
	for _, s := range []string{a.st.danger.Render("✗"), a.st.success.Render("✓"), a.st.warning.Render("awaiting")} {
		if !strings.Contains(wide[1], s) {
			t.Errorf("line 2 lacks %q: %q", s, wide[1])
		}
	}
	narrow := a.renderRow(it, false, 80)
	if got := strings.TrimSpace(ansi.Strip(narrow[2])); got != want {
		t.Errorf("narrow line 3 = %q, want %q", got, want)
	}
}

func TestRowReviewSummaryFallsBackToDecision(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	a.viewer = model.Viewer{Login: "alice"}
	it := item(prKey("o", "r", 1), model.SectionWaitingForReview, "Mine", time.Hour)
	it.PR.ReviewDecision = "REVIEW_REQUIRED"
	if got := ansi.Strip(a.renderRow(it, false, 140)[1]); !strings.HasSuffix(got, "review required") {
		t.Errorf("own PR without reviews: line 2 = %q, want the decision", got)
	}

	// Someone else's PR keeps the decision even with reviews.
	other := item(prKey("o", "r", 2), model.SectionReviewRequested, "Theirs", time.Hour)
	other.PR.Author = "zoe"
	other.PR.ReviewDecision = "CHANGES_REQUESTED"
	other.PR.Reviews = []model.Review{review("bob", "CHANGES_REQUESTED")}
	got := ansi.Strip(a.renderRow(other, false, 140)[1])
	if !strings.HasSuffix(got, "changes requested") || strings.Contains(got, "✗ bob") {
		t.Errorf("others' PR: line 2 = %q, want the decision only", got)
	}
}

func TestDetailReviewsUseVerdicts(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	var b strings.Builder
	a.writeReviews(&b, ownItemWithReviews(), 100)
	out := ansi.Strip(b.String())
	for _, s := range []string{"✗ bob changes requested", "✓ erin approved", "● dave commented", "awaiting acme-co/eng, bob"} {
		if !strings.Contains(out, s) {
			t.Errorf("reviews lack %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "alice") {
		t.Errorf("author's own review listed:\n%s", out)
	}
}
