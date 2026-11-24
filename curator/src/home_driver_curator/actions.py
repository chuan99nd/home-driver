"""What a human can do to suggestions. Every function here logs to the activity table.

Safety properties (tested):
  * The only Immich mutations are ``trash_assets`` (force=false, recoverable),
    ``restore_assets`` (its undo) and ``create_stack`` (deletes nothing).
  * Before trashing, every asset is re-read from Immich: favourites, archived assets and
    keep-listed assets are refused even if a stale suggestion exists.
  * A burst/duplicate suggestion is refused when its group's keeper is no longer available,
    so approving a whole group can never remove every copy.
  * In dry-run mode nothing is sent to Immich and suggestions stay pending.

Functions return an ``Outcome`` whose ``key`` is an i18n message id (see i18n.py), so the
web layer can show it in the user's language.
"""

from __future__ import annotations

import json
import sqlite3
import time
from dataclasses import dataclass

from . import db
from .config import Settings
from .immich import Asset, ImmichClient, ImmichNotFound

MAX_BULK = 500


@dataclass
class Outcome:
    key: str  # i18n message id
    n: int = 0  # how many photos the action applied to
    skipped: int = 0  # photos deliberately left alone
    undo_id: int | None = None  # activity id that can be undone
    dry_run: bool = False


def _unique_pending(conn: sqlite3.Connection, ids: list[int]) -> list[sqlite3.Row]:
    """Pending suggestions for these ids, one per asset."""
    seen: set[str] = set()
    out: list[sqlite3.Row] = []
    for row in db.suggestions_by_ids(conn, ids[:MAX_BULK]):
        if row["status"] == "pending" and row["asset_id"] not in seen:
            seen.add(row["asset_id"])
            out.append(row)
    return out


def _names(rows: list[sqlite3.Row]) -> list[dict[str, str]]:
    return [{"id": r["asset_id"], "name": r["file_name"] or ""} for r in rows]


def _refresh_flags(conn: sqlite3.Connection, asset: Asset) -> None:
    conn.execute(
        "UPDATE assets SET is_favorite=?, visibility=?, is_trashed=? WHERE asset_id=?",
        (int(asset.is_favorite), asset.visibility, int(asset.is_trashed), asset.id),
    )


def keep(conn: sqlite3.Connection, suggestion_ids: list[int], user: str) -> Outcome:
    """Remember these assets forever: they are never suggested again, in any category."""
    rows = _unique_pending(conn, suggestion_ids)
    if not rows:
        return Outcome("msg_stale")
    for row in rows:
        db.add_keep(conn, row["asset_id"], user, row["category"])
        db.resolve_asset(conn, row["asset_id"], "kept", user)
    db.log_activity(conn, user, "keep", category=rows[0]["category"], assets=_names(rows))
    conn.commit()
    return Outcome("msg_kept", n=len(rows))


def _keeper_available(
    conn: sqlite3.Connection, client: ImmichClient, group_id: int, cache: dict[int, bool]
) -> bool:
    if group_id in cache:
        return cache[group_id]
    row = conn.execute("SELECT keeper_asset_id FROM groups WHERE id=?", (group_id,)).fetchone()
    ok = False
    if row is not None:
        try:
            ok = not client.get_asset(row["keeper_asset_id"]).is_trashed
        except ImmichNotFound:
            ok = False
    cache[group_id] = ok
    return ok


def trash(
    conn: sqlite3.Connection,
    client: ImmichClient,
    settings: Settings,
    suggestion_ids: list[int],
    user: str,
) -> Outcome:
    """Move the suggested assets to the Immich trash (recoverable there for 30 days)."""
    rows = _unique_pending(conn, suggestion_ids)
    if not rows:
        return Outcome("msg_stale")
    category = rows[0]["category"]

    if settings.dry_run:
        db.log_activity(
            conn,
            user,
            "trash",
            category=category,
            assets=_names(rows),
            detail="DRY RUN: nothing was sent to Immich",
            dry_run=True,
        )
        conn.commit()
        return Outcome("msg_trash_dry", n=len(rows), dry_run=True)

    keep_listed = db.keep_ids(conn)
    keeper_ok: dict[int, bool] = {}
    to_trash: list[sqlite3.Row] = []
    already: list[sqlite3.Row] = []
    skipped: list[str] = []
    for row in rows:
        asset_id, name = row["asset_id"], row["file_name"] or row["asset_id"]
        if asset_id in keep_listed:
            db.withdraw_asset(conn, asset_id)
            skipped.append(f"{name}: on the keep list")
            continue
        try:
            live = client.get_asset(asset_id)
        except ImmichNotFound:
            db.withdraw_asset(conn, asset_id)
            skipped.append(f"{name}: no longer in Immich")
            continue
        _refresh_flags(conn, live)
        if live.is_favorite or live.is_archived or live.type != "IMAGE":
            db.withdraw_asset(conn, asset_id)
            what = "favourite" if live.is_favorite else "archived or not a photo"
            skipped.append(f"{name}: now {what} in Immich")
            continue
        if live.is_trashed:
            already.append(row)
            continue
        group_id = row["group_id"]
        if group_id is not None and not _keeper_available(conn, client, group_id, keeper_ok):
            skipped.append(f"{name}: the photo chosen to keep is gone")
            continue
        to_trash.append(row)
    conn.commit()

    if to_trash:
        client.trash_assets([r["asset_id"] for r in to_trash])
    for row in [*to_trash, *already]:
        db.resolve_asset(conn, row["asset_id"], "trashed", user)
        conn.execute("UPDATE assets SET is_trashed=1 WHERE asset_id=?", (row["asset_id"],))
    undo_id: int | None = None
    if to_trash or skipped:
        activity_id = db.log_activity(
            conn,
            user,
            "trash",
            category=category,
            assets=_names(to_trash),
            detail=("skipped: " + "; ".join(skipped))[:1000] if skipped else "",
        )
        undo_id = activity_id if to_trash else None
    conn.commit()
    if not to_trash:
        return Outcome("msg_stale", skipped=len(skipped))
    return Outcome("msg_trashed", n=len(to_trash), skipped=len(skipped), undo_id=undo_id)


def undo_trash(
    conn: sqlite3.Connection,
    client: ImmichClient,
    settings: Settings,
    activity_id: int,
    user: str,
) -> Outcome:
    """Bring back what one earlier "trash" action removed and reopen its suggestions."""
    row = conn.execute("SELECT * FROM activity WHERE id=?", (activity_id,)).fetchone()
    if row is None or row["action"] != "trash" or row["undone"] or row["dry_run"]:
        return Outcome("msg_stale")
    assets = json.loads(row["assets"] or "[]")
    ids = [a["id"] for a in assets]
    if not ids:
        return Outcome("msg_stale")
    if settings.dry_run:
        return Outcome("msg_restore_dry", dry_run=True)
    client.restore_assets(ids)
    now = time.time()
    for asset_id in ids:
        conn.execute("UPDATE assets SET is_trashed=0 WHERE asset_id=?", (asset_id,))
        # Reopen only what this trash action resolved; nothing a human kept is touched.
        conn.execute(
            "UPDATE suggestions SET status='pending', resolved_at=NULL, resolved_by=NULL "
            "WHERE asset_id=? AND status='trashed' AND resolved_at >= ?",
            (asset_id, row["ts"] - 5),
        )
    conn.execute("UPDATE activity SET undone=1 WHERE id=?", (activity_id,))
    db.log_activity(
        conn,
        user,
        "restore",
        category=row["category"],
        assets=assets,
        detail=f"undo of entry {activity_id} at {now:.0f}",
    )
    conn.commit()
    return Outcome("msg_restored", n=len(ids))


def set_keeper(conn: sqlite3.Connection, group_id: int, asset_id: str, user: str) -> Outcome:
    """Change which frame of a burst/duplicate group is proposed to keep."""
    group = conn.execute("SELECT * FROM groups WHERE id=?", (group_id,)).fetchone()
    members = db.group_members(conn, group_id) if group else []
    by_id = {m["asset_id"]: m for m in members}
    if group is None or asset_id not in by_id:
        return Outcome("msg_group_changed")
    old = group["keeper_asset_id"]
    if old == asset_id:
        return Outcome("msg_keeper", n=1)
    chosen = by_id[asset_id]
    if chosen["is_trashed"] or not chosen["present"]:
        return Outcome("msg_group_changed")
    conn.execute(
        "UPDATE groups SET keeper_asset_id=?, keeper_by_user=1 WHERE id=?", (asset_id, group_id)
    )
    # The new keeper must not be suggested for removal any more...
    conn.execute(
        "UPDATE suggestions SET status='withdrawn', resolved_at=? "
        "WHERE asset_id=? AND category=? AND status='pending'",
        (time.time(), asset_id, group["kind"]),
    )
    # ...and the old keeper becomes a candidate, unless it is protected.
    prev = by_id.get(old)
    if (
        prev is not None
        and not prev["is_favorite"]
        and not prev["keep_listed"]
        and not prev["is_trashed"]
        and prev["present"]
        and prev["visibility"] == "timeline"
        and prev["type"] == "IMAGE"
    ):
        db.offer(
            conn,
            old,
            group["kind"],
            float(prev["sharpness"] or 0.0),
            "Part of a group of near-identical photos; another one was chosen to keep",
            group_id,
        )
    db.log_activity(
        conn,
        user,
        "set_keeper",
        category=group["kind"],
        assets=[{"id": asset_id, "name": chosen["file_name"] or ""}],
    )
    conn.commit()
    return Outcome("msg_keeper", n=1)


def stack(
    conn: sqlite3.Connection,
    client: ImmichClient,
    settings: Settings,
    group_id: int,
    user: str,
) -> Outcome:
    """Stack a burst in Immich with the keeper on top. Deletes nothing."""
    group = conn.execute("SELECT * FROM groups WHERE id=?", (group_id,)).fetchone()
    if group is None or group["kind"] != "burst":
        return Outcome("msg_group_changed")
    members = [m for m in db.group_members(conn, group_id) if not m["is_trashed"] and m["present"]]
    keeper = group["keeper_asset_id"]
    ids = [m["asset_id"] for m in members]
    if keeper not in ids or len(ids) < 2:
        return Outcome("msg_group_changed")
    assets = [{"id": m["asset_id"], "name": m["file_name"] or ""} for m in members]
    if settings.dry_run:
        db.log_activity(
            conn,
            user,
            "stack",
            category="burst",
            assets=assets,
            detail="DRY RUN: nothing was sent to Immich",
            dry_run=True,
        )
        conn.commit()
        return Outcome("msg_stack_dry", n=len(ids), dry_run=True)
    stack_id = client.create_stack(keeper, [i for i in ids if i != keeper])
    now = time.time()
    for m in members:
        if m["suggestion_status"] == "pending":
            conn.execute(
                "UPDATE suggestions SET status='stacked', resolved_at=?, resolved_by=? WHERE id=?",
                (now, user, m["suggestion_id"]),
            )
    db.log_activity(
        conn, user, "stack", category="burst", assets=assets, detail=f"Immich stack {stack_id}"
    )
    conn.commit()
    return Outcome("msg_stacked", n=len(ids))
