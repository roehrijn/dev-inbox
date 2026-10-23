package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

func stackItem(k model.PRKey, base, head string, score int) api.InboxItem {
	it := item(k, model.SectionReviewRequested, "PR "+k.String(), time.Hour)
	it.PR.BaseRef, it.PR.HeadRef, it.Score = base, head, score
	return it
}

func TestNestStacks(t *testing.T) {
	k := func(n int) model.PRKey { return prKey("o", "r", n) }
	tests := []struct {
		name      string
		items     []api.InboxItem
		wantKeys  []string
		wantDepth []int
	}{
		{
			name: "chain listed under its bottom PR",
			items: []api.InboxItem{
				stackItem(k(3), "b", "c", 80),
				stackItem(k(9), "main", "other", 60),
				stackItem(k(1), "main", "a", 50),
				stackItem(k(2), "a", "b", 40),
			},
			wantKeys:  []string{"o/r#9", "o/r#1", "o/r#2", "o/r#3"},
			wantDepth: []int{0, 0, 1, 2},
		},
		{
			name: "siblings keep their order",
			items: []api.InboxItem{
				stackItem(k(3), "a", "c", 80),
				stackItem(k(1), "main", "a", 50),
				stackItem(k(2), "a", "b", 40),
			},
			wantKeys:  []string{"o/r#1", "o/r#3", "o/r#2"},
			wantDepth: []int{0, 1, 1},
		},
		{
			name: "parent not listed",
			items: []api.InboxItem{
				stackItem(k(2), "a", "b", 80),
				stackItem(k(1), "main", "x", 50),
			},
			wantKeys:  []string{"o/r#2", "o/r#1"},
			wantDepth: []int{0, 0},
		},
		{
			name: "same branch in another repository",
			items: []api.InboxItem{
				stackItem(prKey("o", "other", 2), "a", "b", 80),
				stackItem(k(1), "main", "a", 50),
			},
			wantKeys:  []string{"o/other#2", "o/r#1"},
			wantDepth: []int{0, 0},
		},
		{
			name: "repository matched case-insensitively",
			items: []api.InboxItem{
				stackItem(prKey("O", "R", 2), "a", "b", 80),
				stackItem(k(1), "main", "a", 50),
			},
			wantKeys:  []string{"o/r#1", "O/R#2"},
			wantDepth: []int{0, 1},
		},
		{
			name: "unknown refs",
			items: []api.InboxItem{
				stackItem(k(2), "", "", 80),
				stackItem(k(1), "", "", 50),
			},
			wantKeys:  []string{"o/r#2", "o/r#1"},
			wantDepth: []int{0, 0},
		},
		{
			name: "cycle",
			items: []api.InboxItem{
				stackItem(k(1), "b", "a", 80),
				stackItem(k(2), "a", "b", 50),
				stackItem(k(3), "main", "c", 40),
			},
			wantKeys:  []string{"o/r#3", "o/r#1", "o/r#2"},
			wantDepth: []int{0, 0, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secs := groupItems(tt.items, true)
			s := secs[slices.Index(displaySections, model.SectionReviewRequested)]
			var keys []string
			for _, it := range s.items {
				keys = append(keys, it.PR.Key.String())
			}
			if !slices.Equal(keys, tt.wantKeys) || !slices.Equal(s.depth, tt.wantDepth) {
				t.Errorf("got %v depths %v, want %v depths %v", keys, s.depth, tt.wantKeys, tt.wantDepth)
			}
		})
	}
}

func TestViewIndentsStacks(t *testing.T) {
	k := func(n int) model.PRKey { return prKey("o", "r", n) }
	list := api.ListResponse{Items: []api.InboxItem{
		stackItem(k(2), "a", "b", 80),
		stackItem(k(1), "main", "a", 50),
		stackItem(k(3), "b", "c", 70),
	}}
	for _, w := range []int{120, 60, 20} {
		a, _ := loadedApp(t, list, w, 30)
		press(t, a, "2")
		out := screen(a)
		assertFits(t, out, w, 30)
		if w < 60 {
			continue
		}
		assertOrder(t, out, "    PR o/r#1", "o/r#1 · alice", "    └   PR o/r#2", "        o/r#2 · alice", "      └   PR o/r#3", "          o/r#3 · alice")
		if got := selectedKey(t, a); got != "o/r#1" {
			t.Errorf("width %d: selected %s, want the bottom of the stack", w, got)
		}
		press(t, a, "j", "j")
		if got := selectedKey(t, a); got != "o/r#3" {
			t.Errorf("width %d: selected %s after two moves, want the top of the stack", w, got)
		}
	}
}

func TestStackedRowFits(t *testing.T) {
	a := newTestApp(&fakeBackend{})
	it := item(prKey("o", "r", 1), model.SectionReviewRequested, "Fix the flaky webhook test", time.Hour)
	it.Score = 100
	for depth := range 4 {
		for w := 0; w <= 40; w++ {
			assertRowFits(t, a.renderRow(it, depth, true, w), w)
		}
		row := ansi.Strip(a.renderRow(it, depth, false, 120)[0])
		if depth > 0 && !strings.Contains(row, strings.Repeat(" ", stackIndent*(depth-1))+"└ ") {
			t.Errorf("depth %d: no connector in %q", depth, row)
		}
	}
}
