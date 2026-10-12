// Package tui is the terminal inbox. It is a pure client of the daemon's NATS API.
package tui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/roehrijn/dev-inbox/internal/model"
)

// Run connects to the daemon at natsURL and runs the TUI until the user quits or ctx ends.
func Run(ctx context.Context, natsURL string) error {
	a := newApp(ctx, nil, natsURL)
	p := tea.NewProgram(a, tea.WithContext(ctx))
	// Program.Send blocks until the program runs, so early NATS callbacks wait for it.
	c, err := dial(natsURL, func(connected bool) { p.Send(connMsg{connected: connected}) })
	if err != nil {
		return err
	}
	defer c.Close()
	a.be = c

	sub, err := c.subscribeEvents(func(k *model.PRKey) { p.Send(eventMsg{key: k}) })
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()

	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
