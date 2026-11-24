"""SQLite state. Schema and invariants are documented in curator/CLAUDE.md.

Invariants enforced here:
  * ``keep_list`` rows are never deleted (there is no function that does it).
  * A suggestion that a human resolved (kept / trashed / stacked) is never reopened.
  * Only ``pending`` suggestions are ever changed automatically (to ``withdrawn``).
"""

from __future__ import annotations

import json
import sqlite3
import time
from collections.abc import Iterable, Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

SCHEMA_VERSION = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS assets (
    asset_id      TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL DEFAULT '',
    type          TEXT NOT NULL DEFAULT 'IMAGE',
    file_name     TEXT NOT NULL DEFAULT '',
    mime          TEXT NOT NULL DEFAULT '',
    taken_at      REAL,
    taken_at_iso  TEXT NOT NULL DEFAULT '',
    camera        TEXT NOT NULL DEFAULT '',
    width         INTEGER,
    height        INTEGER,
    is_favorite   INTEGER NOT NULL DEFAULT 0,
    visibility    TEXT NOT NULL DEFAULT 'timeline',
    is_trashed    INTEGER NOT NULL DEFAULT 0,
    checksum      TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT '',
    present       INTEGER NOT NULL DEFAULT 1,
    last_seen_scan INTEGER NOT NULL DEFAULT 0,
    -- analysis cache: valid while signature matches the asset's current signature
    signature     TEXT,
    analysed_at   REAL,
    sharpness     REAL,
    dhash         TEXT,
    scores        TEXT,
    error         TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS assets_burst ON assets (owner_id, camera, taken_at);

CREATE TABLE IF NOT EXISTS groups (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    kind             TEXT NOT NULL,            -- burst | duplicate
    key              TEXT NOT NULL UNIQUE,     -- hash of kind + sorted member ids
    keeper_asset_id  TEXT NOT NULL,
    keeper_by_user   INTEGER NOT NULL DEFAULT 0,
    created_at       REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS group_members (
    group_id  INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    asset_id  TEXT NOT NULL,
    position  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (group_id, asset_id)
);

CREATE TABLE IF NOT EXISTS suggestions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    asset_id    TEXT NOT NULL,
    category    TEXT NOT NULL,
    score       REAL NOT NULL,
    reason      TEXT NOT NULL,
    group_id    INTEGER,
    status      TEXT NOT NULL DEFAULT 'pending',
                -- pending | withdrawn (automatic) | kept | trashed | stacked (human)
    created_at  REAL NOT NULL,
    resolved_at REAL,
    resolved_by TEXT,
    UNIQUE (asset_id, category)
);
CREATE INDEX IF NOT EXISTS suggestions_queue ON suggestions (status, category, score);
CREATE INDEX IF NOT EXISTS suggestions_group ON suggestions (group_id);

CREATE TABLE IF NOT EXISTS keep_list (
    asset_id   TEXT PRIMARY KEY,
    added_at   REAL NOT NULL,
    added_by   TEXT NOT NULL,
    category   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS activity (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        REAL NOT NULL,
    user      TEXT NOT NULL,
    action    TEXT NOT NULL,   -- keep | trash | restore | stack | set_keeper
    category  TEXT NOT NULL DEFAULT '',
    count     INTEGER NOT NULL DEFAULT 0,
    assets    TEXT NOT NULL DEFAULT '[]',   -- JSON [{"id":..., "name":...}]
    detail    TEXT NOT NULL DEFAULT '',
    dry_run   INTEGER NOT NULL DEFAULT 0,
    undone    INTEGER NOT NULL DEFAULT 0   -- a trash entry that was restored via Undo
);

CREATE TABLE IF NOT EXISTS scans (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at      REAL NOT NULL,
    finished_at     REAL,
    status          TEXT NOT NULL,  -- running | completed | interrupted | failed
    listed          INTEGER NOT NULL DEFAULT 0,
    analysed        INTEGER NOT NULL DEFAULT 0,
    skipped         INTEGER NOT NULL DEFAULT 0,
    errors          INTEGER NOT NULL DEFAULT 0,
    remaining       INTEGER NOT NULL DEFAULT 0,
    new_suggestions INTEGER NOT NULL DEFAULT 0,
    message         TEXT NOT NULL DEFAULT ''
);
"""

HUMAN_STATUSES = ("kept", "trashed", "stacked")


def connect(path: str) -> sqlite3.Connection:
    if path != ":memory:":
        Path(path).parent.mkdir(parents=True, exist_ok=True)
    # check_same_thread=False: the web layer opens one connection per request, and FastAPI may
    # run the dependency set-up, the handler and the teardown on different pool threads.
    # A connection is never used by two requests at once.
    conn = sqlite3.connect(path, timeout=30.0, check_same_thread=False)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.execute("PRAGMA busy_timeout=30000")
    return conn


def init(conn: sqlite3.Connection) -> None:
    conn.executescript(SCHEMA)
    conn.execute(f"PRAGMA user_version={SCHEMA_VERSION}")
    conn.commit()


@contextmanager
def open_db(path: str) -> Iterator[sqlite3.Connection]:
    conn = connect(path)
    try:
        init(conn)
        yield conn
    finally:
        conn.close()


# --- keep list -------------------------------------------------------------------------


def keep_ids(conn: sqlite3.Connection) -> set[str]:
    return {r[0] for r in conn.execute("SELECT asset_id FROM keep_list")}


def add_keep(conn: sqlite3.Connection, asset_id: str, user: str, category: str) -> None:
    conn.execute(
        "INSERT OR IGNORE INTO keep_list (asset_id, added_at, added_by, category) VALUES (?,?,?,?)",
        (asset_id, time.time(), user, category),
    )


# --- suggestions -----------------------------------------------------------------------


def resolve_asset(conn: sqlite3.Connection, asset_id: str, status: str, user: str) -> None:
    """Resolve every pending suggestion of an asset (it may be flagged in several categories)."""
    assert status in HUMAN_STATUSES
    conn.execute(
        "UPDATE suggestions SET status=?, resolved_at=?, resolved_by=? "
        "WHERE asset_id=? AND status='pending'",
        (status, time.time(), user, asset_id),
    )


def withdraw_asset(conn: sqlite3.Connection, asset_id: str) -> None:
    conn.execute(
        "UPDATE suggestions SET status='withdrawn', resolved_at=? "
        "WHERE asset_id=? AND status='pending'",
        (time.time(), asset_id),
    )


def offer(
    conn: sqlite3.Connection,
    asset_id: str,
    category: str,
    score: float,
    reason: str,
    group_id: int | None,
) -> bool:
    """Create or refresh a pending suggestion. Returns True if it is newly created.

    Never reopens a suggestion a human already resolved.
    """
    row = conn.execute(
        "SELECT id, status FROM suggestions WHERE asset_id=? AND category=?", (asset_id, category)
    ).fetchone()
    if row is None:
        conn.execute(
            "INSERT INTO suggestions (asset_id, category, score, reason, group_id, created_at) "
            "VALUES (?,?,?,?,?,?)",
            (asset_id, category, score, reason, group_id, time.time()),
        )
        return True
    if row["status"] in HUMAN_STATUSES:
        return False
    conn.execute(
        "UPDATE suggestions SET score=?, reason=?, group_id=?, status='pending', "
        "resolved_at=NULL, resolved_by=NULL WHERE id=?",
        (score, reason, group_id, row["id"]),
    )
    return False


def reconcile(
    conn: sqlite3.Connection,
    desired: dict[tuple[str, str], tuple[float, str, int | None]],
    categories: Iterable[str],
) -> int:
    """Make the pending queue for ``categories`` equal ``desired``. Returns new suggestions."""
    new = 0
    cats = list(categories)
    marks = ",".join("?" * len(cats))
    pending = conn.execute(
        f"SELECT id, asset_id, category FROM suggestions "  # noqa: S608 - marks are '?' only
        f"WHERE status='pending' AND category IN ({marks})",
        cats,
    ).fetchall()
    now = time.time()
    for row in pending:
        if (row["asset_id"], row["category"]) not in desired:
            conn.execute(
                "UPDATE suggestions SET status='withdrawn', resolved_at=? WHERE id=?",
                (now, row["id"]),
            )
    for (asset_id, category), (score, reason, group_id) in desired.items():
        if offer(conn, asset_id, category, score, reason, group_id):
            new += 1
    return new


def pending_counts(conn: sqlite3.Connection) -> dict[str, int]:
    rows = conn.execute(
        "SELECT category, COUNT(*) AS n FROM suggestions WHERE status='pending' GROUP BY category"
    )
    return {r["category"]: r["n"] for r in rows}


def suggestions_by_ids(conn: sqlite3.Connection, ids: list[int]) -> list[sqlite3.Row]:
    if not ids:
        return []
    marks = ",".join("?" * len(ids))
    return conn.execute(
        "SELECT s.*, a.file_name, a.taken_at_iso FROM suggestions s "  # noqa: S608
        f"LEFT JOIN assets a ON a.asset_id = s.asset_id WHERE s.id IN ({marks}) ORDER BY s.id",
        ids,
    ).fetchall()


# --- groups ----------------------------------------------------------------------------


def upsert_group(
    conn: sqlite3.Connection, kind: str, key: str, member_ids: list[str], keeper: str
) -> tuple[int, str]:
    """Create/refresh a group. A keeper the user chose is kept if it is still a member.

    Returns (group_id, effective keeper id).
    """
    row = conn.execute("SELECT * FROM groups WHERE key=?", (key,)).fetchone()
    if row is None:
        cur = conn.execute(
            "INSERT INTO groups (kind, key, keeper_asset_id, created_at) VALUES (?,?,?,?)",
            (kind, key, keeper, time.time()),
        )
        group_id = int(cur.lastrowid or 0)
    else:
        group_id = int(row["id"])
        if row["keeper_by_user"] and row["keeper_asset_id"] in member_ids:
            keeper = row["keeper_asset_id"]
        else:
            conn.execute(
                "UPDATE groups SET keeper_asset_id=?, keeper_by_user=0 WHERE id=?",
                (keeper, group_id),
            )
    conn.execute("DELETE FROM group_members WHERE group_id=?", (group_id,))
    conn.executemany(
        "INSERT INTO group_members (group_id, asset_id, position) VALUES (?,?,?)",
        [(group_id, asset_id, i) for i, asset_id in enumerate(member_ids)],
    )
    return group_id, keeper


def prune_groups(conn: sqlite3.Connection) -> None:
    """Drop groups no suggestion refers to any more."""
    conn.execute(
        "DELETE FROM groups WHERE id NOT IN "
        "(SELECT DISTINCT group_id FROM suggestions WHERE group_id IS NOT NULL)"
    )


def group_members(conn: sqlite3.Connection, group_id: int) -> list[sqlite3.Row]:
    return conn.execute(
        "SELECT a.*, m.position, s.id AS suggestion_id, s.status AS suggestion_status, "
        "s.reason AS suggestion_reason, "
        "(SELECT 1 FROM keep_list k WHERE k.asset_id = m.asset_id) AS keep_listed "
        "FROM group_members m JOIN groups g ON g.id = m.group_id "
        "JOIN assets a ON a.asset_id = m.asset_id "
        "LEFT JOIN suggestions s ON s.asset_id = m.asset_id AND s.category = g.kind "
        "WHERE m.group_id=? ORDER BY m.position",
        (group_id,),
    ).fetchall()


# --- activity & scans ------------------------------------------------------------------


def log_activity(
    conn: sqlite3.Connection,
    user: str,
    action: str,
    *,
    category: str = "",
    assets: list[dict[str, str]] | None = None,
    detail: str = "",
    dry_run: bool = False,
) -> int:
    assets = assets or []
    cur = conn.execute(
        "INSERT INTO activity (ts, user, action, category, count, assets, detail, dry_run) "
        "VALUES (?,?,?,?,?,?,?,?)",
        (
            time.time(),
            user,
            action,
            category,
            len(assets),
            json.dumps(assets),
            detail,
            int(dry_run),
        ),
    )
    return int(cur.lastrowid or 0)


def last_scan(conn: sqlite3.Connection) -> sqlite3.Row | None:
    return conn.execute("SELECT * FROM scans ORDER BY id DESC LIMIT 1").fetchone()


def library_stats(conn: sqlite3.Connection) -> dict[str, Any]:
    row = conn.execute(
        "SELECT COUNT(*) AS known, "
        "SUM(CASE WHEN signature IS NOT NULL AND error IS NULL THEN 1 ELSE 0 END) AS analysed, "
        "SUM(CASE WHEN error IS NOT NULL THEN 1 ELSE 0 END) AS errors "
        "FROM assets WHERE present=1 AND type='IMAGE'"
    ).fetchone()
    kept = conn.execute("SELECT COUNT(*) FROM keep_list").fetchone()[0]
    return {
        "known": row["known"] or 0,
        "analysed": row["analysed"] or 0,
        "errors": row["errors"] or 0,
        "kept": kept,
    }
