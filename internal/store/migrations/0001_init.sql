-- GitHub logins and repo names are case-insensitive, hence the NOCASE key columns.

CREATE TABLE prs (
    owner      TEXT    NOT NULL COLLATE NOCASE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    number     INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL, -- unix nanoseconds, 0 for the zero time
    snapshot   TEXT    NOT NULL, -- model.PR as JSON, without comments
    PRIMARY KEY (owner, repo, number)
);

CREATE TABLE comments (
    id         TEXT    PRIMARY KEY,
    owner      TEXT    NOT NULL COLLATE NOCASE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    number     INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    data       TEXT    NOT NULL, -- model.Comment as JSON
    FOREIGN KEY (owner, repo, number) REFERENCES prs (owner, repo, number) ON DELETE CASCADE
);

CREATE INDEX comments_by_pr ON comments (owner, repo, number, created_at, id);

CREATE TABLE annotations (
    owner      TEXT    NOT NULL COLLATE NOCASE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    number     INTEGER NOT NULL,
    comment_id TEXT    NOT NULL, -- '' for PR-level annotations
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL, -- JSON
    source     TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, number, comment_id, key),
    FOREIGN KEY (owner, repo, number) REFERENCES prs (owner, repo, number) ON DELETE CASCADE
);

-- Outbox and short event log. seq is never reused, so pruning cannot reorder pending events.
CREATE TABLE events (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL,
    owner        TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    number       INTEGER NOT NULL,
    time         INTEGER NOT NULL,
    data         TEXT,
    published_at INTEGER
);

CREATE INDEX events_pending ON events (seq) WHERE published_at IS NULL;

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
