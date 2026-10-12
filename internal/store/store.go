// Package store persists PR snapshots, comments, annotations and the event outbox in SQLite.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/roehrijn/dev-inbox/internal/model"
)

// ErrPRNotFound is returned by SetAnnotation for a PR that has no stored snapshot.
var ErrPRNotFound = errors.New("pr not found")

// Store is safe for concurrent use. It runs on a single connection, so every operation is serialized.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and migrates its schema.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// Create the file up front so it is private to the user; SQLite gives the WAL files the same mode.
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	dsn := (&url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     abs,
		RawQuery: url.Values{
			"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
			// Write transactions take the write lock up front so busy_timeout applies instead of a
			// failed lock upgrade.
			"_txlock": {"immediate"},
		}.Encode(),
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	stmts, err := loadMigrations()
	if err == nil {
		err = migrate(context.Background(), db, stmts)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", abs, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// write runs fn in a write transaction that is rolled back unless fn succeeds.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// read runs fn in a read transaction so that several queries see one consistent state.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// Timestamps are stored as unix nanoseconds so they round-trip exactly and compare numerically.
// The zero time is stored as 0.
func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

const upsertPR = `
INSERT INTO prs (owner, repo, number, state, updated_at, snapshot) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    state = excluded.state, updated_at = excluded.updated_at, snapshot = excluded.snapshot`

const upsertComment = `
INSERT INTO comments (id, owner, repo, number, created_at, data) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET created_at = excluded.created_at, data = excluded.data`

// GetPR returns the stored snapshot including comments; ok is false for unknown PRs.
func (s *Store) GetPR(ctx context.Context, key model.PRKey) (pr model.PR, ok bool, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var snapshot []byte
		err := tx.QueryRowContext(ctx,
			`SELECT snapshot FROM prs WHERE owner = ? AND repo = ? AND number = ?`,
			key.Owner, key.Repo, key.Number).Scan(&snapshot)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(snapshot, &pr); err != nil {
			return fmt.Errorf("decode snapshot of %s: %w", key, err)
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT data FROM comments WHERE owner = ? AND repo = ? AND number = ? ORDER BY created_at, id`,
			key.Owner, key.Repo, key.Number)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				return err
			}
			var c model.Comment
			if err := json.Unmarshal(data, &c); err != nil {
				return fmt.Errorf("decode comment of %s: %w", key, err)
			}
			pr.Comments = append(pr.Comments, c)
		}
		ok = true
		return rows.Err()
	})
	if err != nil {
		return model.PR{}, false, err
	}
	return pr, ok, nil
}

// ListPRs returns stored snapshots including comments, optionally only open ones, most recently
// updated first.
func (s *Store) ListPRs(ctx context.Context, openOnly bool) ([]model.PR, error) {
	where := ""
	var args []any
	if openOnly {
		where = " WHERE p.state = ?"
		args = append(args, string(model.PRStateOpen))
	}

	prs := []model.PR{}
	err := s.read(ctx, func(tx *sql.Tx) error {
		// The keys come from the prs table so that comments group under the same spelling as their PR.
		rows, err := tx.QueryContext(ctx,
			`SELECT p.owner, p.repo, p.number, p.snapshot FROM prs p`+where+
				` ORDER BY p.updated_at DESC, p.owner, p.repo, p.number`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var keys []model.PRKey
		for rows.Next() {
			var (
				key      model.PRKey
				snapshot []byte
				pr       model.PR
			)
			if err := rows.Scan(&key.Owner, &key.Repo, &key.Number, &snapshot); err != nil {
				return err
			}
			if err := json.Unmarshal(snapshot, &pr); err != nil {
				return fmt.Errorf("decode snapshot of %s: %w", key, err)
			}
			keys = append(keys, key)
			prs = append(prs, pr)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Single connection: the PR cursor must be closed before the next query can run.
		if err := rows.Close(); err != nil {
			return err
		}

		crows, err := tx.QueryContext(ctx,
			`SELECT p.owner, p.repo, p.number, c.data FROM comments c JOIN prs p
			 ON c.owner = p.owner AND c.repo = p.repo AND c.number = p.number`+where+
				` ORDER BY p.owner, p.repo, p.number, c.created_at, c.id`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = crows.Close() }()
		comments := map[model.PRKey][]model.Comment{}
		for crows.Next() {
			var (
				key  model.PRKey
				data []byte
				c    model.Comment
			)
			if err := crows.Scan(&key.Owner, &key.Repo, &key.Number, &data); err != nil {
				return err
			}
			if err := json.Unmarshal(data, &c); err != nil {
				return fmt.Errorf("decode comment of %s: %w", key, err)
			}
			comments[key] = append(comments[key], c)
		}
		if err := crows.Err(); err != nil {
			return err
		}
		for i, key := range keys {
			prs[i].Comments = comments[key]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return prs, nil
}

// ApplySnapshot upserts pr and its comments and appends events to the outbox in one transaction.
// Comments missing from pr are kept, since snapshots only carry the most recent comments.
func (s *Store) ApplySnapshot(ctx context.Context, pr model.PR, events []model.Event) error {
	comments := pr.Comments
	pr.Comments = nil
	snapshot, err := json.Marshal(pr)
	if err != nil {
		return fmt.Errorf("encode snapshot of %s: %w", pr.Key, err)
	}
	encoded := make([][]byte, len(comments))
	for i, c := range comments {
		if c.ID == "" {
			return fmt.Errorf("comment without ID on %s", pr.Key)
		}
		if encoded[i], err = json.Marshal(c); err != nil {
			return fmt.Errorf("encode comment %s: %w", c.ID, err)
		}
	}

	k := pr.Key
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, upsertPR,
			k.Owner, k.Repo, k.Number, string(pr.State), toNanos(pr.UpdatedAt), string(snapshot)); err != nil {
			return fmt.Errorf("upsert %s: %w", k, err)
		}
		for i, c := range comments {
			if _, err := tx.ExecContext(ctx, upsertComment,
				c.ID, k.Owner, k.Repo, k.Number, toNanos(c.CreatedAt), string(encoded[i])); err != nil {
				return fmt.Errorf("upsert comment %s: %w", c.ID, err)
			}
		}
		for _, ev := range events {
			if err := insertEvent(ctx, tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetAnnotation upserts a, or deletes it when a.Value is empty or JSON null, and appends ev to the
// outbox in one transaction. It returns ErrPRNotFound when the PR has no stored snapshot.
func (s *Store) SetAnnotation(ctx context.Context, a model.Annotation, ev model.Event) error {
	value := bytes.TrimSpace(a.Value)
	remove := len(value) == 0 || string(value) == "null"
	if !remove && !json.Valid(value) {
		return fmt.Errorf("annotation %q on %s: value is not valid JSON", a.Key, a.PR)
	}

	return s.write(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM prs WHERE owner = ? AND repo = ? AND number = ?`,
			a.PR.Owner, a.PR.Repo, a.PR.Number).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrPRNotFound, a.PR)
		}
		if err != nil {
			return err
		}

		if remove {
			_, err = tx.ExecContext(ctx,
				`DELETE FROM annotations WHERE owner = ? AND repo = ? AND number = ? AND comment_id = ? AND key = ?`,
				a.PR.Owner, a.PR.Repo, a.PR.Number, a.CommentID, a.Key)
		} else {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO annotations (owner, repo, number, comment_id, key, value, source, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (owner, repo, number, comment_id, key) DO UPDATE SET
				    value = excluded.value, source = excluded.source, updated_at = excluded.updated_at`,
				a.PR.Owner, a.PR.Repo, a.PR.Number, a.CommentID, a.Key, string(a.Value), a.Source, toNanos(a.UpdatedAt))
		}
		if err != nil {
			return fmt.Errorf("write annotation %q on %s: %w", a.Key, a.PR, err)
		}
		return insertEvent(ctx, tx, ev)
	})
}

// The join yields the PR's spelling of owner and repo, so one PR always has one map key.
const selectAnnotations = `
SELECT p.owner, p.repo, p.number, a.comment_id, a.key, a.value, a.source, a.updated_at
FROM annotations a JOIN prs p ON a.owner = p.owner AND a.repo = p.repo AND a.number = p.number`

func (s *Store) queryAnnotations(ctx context.Context, query string, args ...any) ([]model.Annotation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []model.Annotation{}
	for rows.Next() {
		var (
			a       model.Annotation
			value   []byte
			updated int64
		)
		if err := rows.Scan(&a.PR.Owner, &a.PR.Repo, &a.PR.Number, &a.CommentID, &a.Key, &value, &a.Source, &updated); err != nil {
			return nil, err
		}
		a.Value = value
		a.UpdatedAt = fromNanos(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Annotations returns the PR- and comment-level annotations of one PR, PR-level first.
func (s *Store) Annotations(ctx context.Context, key model.PRKey) ([]model.Annotation, error) {
	return s.queryAnnotations(ctx,
		selectAnnotations+` WHERE p.owner = ? AND p.repo = ? AND p.number = ? ORDER BY a.comment_id, a.key`,
		key.Owner, key.Repo, key.Number)
}

// AllAnnotations returns the PR-level annotations (CommentID empty) of all PRs.
func (s *Store) AllAnnotations(ctx context.Context) (map[model.PRKey][]model.Annotation, error) {
	list, err := s.queryAnnotations(ctx,
		selectAnnotations+` WHERE a.comment_id = '' ORDER BY p.owner, p.repo, p.number, a.key`)
	if err != nil {
		return nil, err
	}
	out := map[model.PRKey][]model.Annotation{}
	for _, a := range list {
		out[a.PR] = append(out[a.PR], a)
	}
	return out, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, ev model.Event) error {
	var data any
	if len(ev.Data) > 0 {
		data = string(ev.Data)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (id, type, owner, repo, number, time, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Type, ev.PR.Owner, ev.PR.Repo, ev.PR.Number, toNanos(ev.Time), data); err != nil {
		return fmt.Errorf("insert event %s (%s): %w", ev.ID, ev.Type, err)
	}
	return nil
}

// PendingEvents returns up to limit unpublished outbox events in insertion order. A limit of zero
// or less returns none.
func (s *Store) PendingEvents(ctx context.Context, limit int) ([]model.Event, error) {
	events := []model.Event{}
	if limit <= 0 {
		return events, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, owner, repo, number, time, data FROM events
		WHERE published_at IS NULL ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ev   model.Event
			ts   int64
			data []byte
		)
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.PR.Owner, &ev.PR.Repo, &ev.PR.Number, &ts, &data); err != nil {
			return nil, err
		}
		ev.Time = fromNanos(ts)
		if len(data) > 0 {
			ev.Data = data
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// MarkPublished marks the events with the given IDs as published. Unknown IDs are ignored.
func (s *Store) MarkPublished(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := toNanos(time.Now())
	return s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `UPDATE events SET published_at = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, id := range ids {
			if _, err := stmt.ExecContext(ctx, now, id); err != nil {
				return fmt.Errorf("mark event %s published: %w", id, err)
			}
		}
		return nil
	})
}

// PruneEvents deletes published events older than before and returns how many were deleted.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE published_at IS NOT NULL AND time < ?`, toNanos(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Meta returns a daemon bookkeeping value such as the notifications Last-Modified header.
func (s *Store) Meta(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}
