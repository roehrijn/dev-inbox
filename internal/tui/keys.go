package tui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	Up, Down, Top, Bottom                  key.Binding
	NextTab, PrevTab, JumpTab              key.Binding
	Toggle, Collapse, Expand               key.Binding
	Open, Copy, Detail                     key.Binding
	ScrollUp, ScrollDown, PageUp, PageDown key.Binding
	Refresh, Help, Back, Quit              key.Binding
}

func newKeyMap() keyMap {
	b := func(keys []string, help, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return keyMap{
		Up:         b([]string{"k", "up"}, "↑/k", "up"),
		Down:       b([]string{"j", "down"}, "↓/j", "down"),
		Top:        b([]string{"g", "home"}, "g", "top"),
		Bottom:     b([]string{"G", "end"}, "G", "bottom"),
		NextTab:    b([]string{"tab"}, "tab", "next tab"),
		PrevTab:    b([]string{"shift+tab"}, "shift+tab", "prev tab"),
		JumpTab:    b([]string{"1", "2"}, "1/2", "jump tab"),
		Toggle:     b([]string{"space"}, "space", "fold section"),
		Collapse:   b([]string{"h", "left"}, "h", "collapse"),
		Expand:     b([]string{"l", "right"}, "l", "expand"),
		Open:       b([]string{"enter", "o"}, "enter/o", "open in browser"),
		Copy:       b([]string{"y"}, "y", "copy URL"),
		Detail:     b([]string{"d"}, "d", "details"),
		ScrollUp:   b([]string{"K"}, "K", "details up"),
		ScrollDown: b([]string{"J"}, "J", "details down"),
		PageUp:     b([]string{"pgup", "ctrl+u"}, "pgup", "details page up"),
		PageDown:   b([]string{"pgdown", "ctrl+d"}, "pgdn", "details page down"),
		Refresh:    b([]string{"r"}, "r", "refresh"),
		Help:       b([]string{"?"}, "?", "help"),
		Back:       b([]string{"esc"}, "esc", "close"),
		Quit:       b([]string{"q", "ctrl+c"}, "q", "quit"),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Down, k.Open, k.NextTab, k.Detail, k.Toggle, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Top, k.Bottom},
		{k.NextTab, k.PrevTab, k.JumpTab},
		{k.Toggle, k.Collapse, k.Expand},
		{k.Open, k.Copy, k.Refresh},
		{k.Detail, k.ScrollDown, k.ScrollUp, k.PageDown, k.PageUp},
		{k.Help, k.Back, k.Quit},
	}
}
