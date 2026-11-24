"""Incremental, resumable library scan.

Phases (see curator/CLAUDE.md "Scan state machine"):
  1. lock      - one scan at a time (flock on <db>.scan.lock; released if the process dies)
  2. list      - page through all IMAGE assets; refresh metadata; analyse new/changed ones
                 (bounded by CURATOR_SCAN_BATCH per run); commit after every page
  3. suggest   - only after a complete listing: rebuild the pending queue from stored scores
  4. notify    - best effort, never fails the scan

The scan never calls a mutating Immich endpoint.
"""

from __future__ import annotations

import fcntl
import hashlib
import json
import logging
import os
import sqlite3
import subprocess
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

from . import db, imaging
from .config import Settings
from .detectors import DETECTORS, AssetMeta
from .immich import (
    Asset,
    DuplicateGroup,
    ImmichAuthError,
    ImmichClient,
    ImmichError,
    ImmichPermissionError,
    ImmichUnavailable,
)

log = logging.getLogger(__name__)

# Bump when detector maths changes so every asset is re-analysed on the next scans.
ANALYSIS_VERSION = 1
MAX_ATTEMPTS = 3


class ScanAlreadyRunning(Exception):
    pass


class ScanLock:
    def __init__(self, db_path: str) -> None:
        self._path = db_path + ".scan.lock"
        self._fd: int | None = None

    def __enter__(self) -> ScanLock:
        fd = os.open(self._path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            os.close(fd)
            raise ScanAlreadyRunning("another scan is already running") from exc
        self._fd = fd
        return self

    def __exit__(self, *exc: object) -> None:
        if self._fd is not None:
            fcntl.flock(self._fd, fcntl.LOCK_UN)
            os.close(self._fd)
            self._fd = None


def signature(asset: Asset) -> str:
    return f"v{ANALYSIS_VERSION}|{asset.checksum}|{asset.updated_at}"


@dataclass(frozen=True)
class Analysis:
    asset_id: str
    signature: str
    sharpness: float | None = None
    dhash: str | None = None
    scores: dict[str, dict[str, object]] | None = None
    error: str | None = None


def analyse_bytes(data: bytes, meta: AssetMeta) -> tuple[float, str, dict[str, dict[str, object]]]:
    """Decode a preview and run every detector. Pure: no I/O. Returns (sharpness, dhash, scores)."""
    frame = imaging.decode(data)
    scores: dict[str, dict[str, object]] = {}
    for det in DETECTORS:
        finding = det.fn(frame, meta)
        scores[det.name] = {
            "score": finding.score,
            "reason": finding.reason,
            "veto": finding.veto,
        }
    sharp = imaging.sharpness(frame.gray).absolute
    return sharp, f"{imaging.dhash(frame.gray):016x}", scores


def analyse_asset(client: ImmichClient, asset: Asset) -> Analysis:
    sig = signature(asset)
    try:
        data, _ = client.get_thumbnail(asset.id, "preview")
        meta = AssetMeta(
            file_name=asset.file_name,
            mime=asset.mime,
            width=asset.width,
            height=asset.height,
            make=asset.make,
            model=asset.model,
        )
        sharp, dh, scores = analyse_bytes(data, meta)
    except (ImmichAuthError, ImmichPermissionError, ImmichUnavailable):
        raise  # systemic: abort the scan instead of marking the whole library as failed
    except (ImmichError, OSError, ValueError, SyntaxError) as exc:
        return Analysis(asset.id, sig, error=f"{type(exc).__name__}: {exc}"[:300])
    return Analysis(asset.id, sig, sharpness=sharp, dhash=dh, scores=scores)


@dataclass
class ScanResult:
    scan_id: int = 0
    status: str = "running"
    listed: int = 0
    analysed: int = 0
    skipped: int = 0
    errors: int = 0
    remaining: int = 0
    new_suggestions: int = 0
    message: str = ""
    warnings: list[str] = field(default_factory=list)


def _upsert_meta(conn: sqlite3.Connection, asset: Asset, scan_id: int) -> None:
    conn.execute(
        "INSERT INTO assets (asset_id, owner_id, type, file_name, mime, taken_at, taken_at_iso, "
        "camera, width, height, is_favorite, visibility, is_trashed, checksum, updated_at, "
        "present, last_seen_scan) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?) "
        "ON CONFLICT(asset_id) DO UPDATE SET owner_id=excluded.owner_id, type=excluded.type, "
        "file_name=excluded.file_name, mime=excluded.mime, taken_at=excluded.taken_at, "
        "taken_at_iso=excluded.taken_at_iso, camera=excluded.camera, width=excluded.width, "
        "height=excluded.height, is_favorite=excluded.is_favorite, "
        "visibility=excluded.visibility, is_trashed=excluded.is_trashed, "
        "checksum=excluded.checksum, updated_at=excluded.updated_at, present=1, "
        "last_seen_scan=excluded.last_seen_scan",
        (
            asset.id,
            asset.owner_id,
            asset.type,
            asset.file_name,
            asset.mime,
            asset.taken_at,
            asset.taken_at_iso,
            asset.camera,
            asset.width,
            asset.height,
            int(asset.is_favorite),
            asset.visibility,
            int(asset.is_trashed),
            asset.checksum,
            asset.updated_at,
            scan_id,
        ),
    )


def _store_analysis(conn: sqlite3.Connection, a: Analysis) -> None:
    if a.error is not None:
        conn.execute(
            "UPDATE assets SET error=?, analysed_at=?, "
            "attempts = CASE WHEN signature IS ? THEN attempts + 1 ELSE 1 END, "
            "signature=?, sharpness=NULL, dhash=NULL, scores=NULL WHERE asset_id=?",
            (a.error, time.time(), a.signature, a.signature, a.asset_id),
        )
    else:
        conn.execute(
            "UPDATE assets SET signature=?, analysed_at=?, sharpness=?, dhash=?, scores=?, "
            "error=NULL, attempts=0 WHERE asset_id=?",
            (a.signature, time.time(), a.sharpness, a.dhash, json.dumps(a.scores), a.asset_id),
        )


def _needs_analysis(conn: sqlite3.Connection, asset: Asset) -> bool:
    if asset.type != "IMAGE" or asset.is_trashed:
        return False
    row = conn.execute(
        "SELECT signature, error, attempts FROM assets WHERE asset_id=?", (asset.id,)
    ).fetchone()
    if row is None or row["signature"] != signature(asset):
        return True
    # Same signature: redo only failed analyses, a limited number of times.
    return row["error"] is not None and row["attempts"] < MAX_ATTEMPTS


# --- suggestion rebuild ----------------------------------------------------------------

ELIGIBLE_SQL = (
    "present=1 AND type='IMAGE' AND is_favorite=0 AND visibility='timeline' AND is_trashed=0"
)


def _group_key(kind: str, extra: str, member_ids: list[str]) -> str:
    raw = kind + "|" + extra + "|" + ",".join(sorted(member_ids))
    return hashlib.sha256(raw.encode()).hexdigest()[:32]


def find_bursts(
    rows: list[sqlite3.Row], window: float, max_hamming: int
) -> list[list[sqlite3.Row]]:
    """Group rows (sorted by owner, camera, taken_at) into bursts.

    A frame joins the current burst when it was taken within ``window`` seconds of the
    previous frame AND its dHash is within ``max_hamming`` bits of the burst's FIRST frame
    (anchoring to the first frame stops a slow pan from chaining unrelated photos together).
    """
    bursts: list[list[sqlite3.Row]] = []
    current: list[sqlite3.Row] = []
    for row in rows:
        if current:
            prev, anchor = current[-1], current[0]
            same = row["owner_id"] == prev["owner_id"] and row["camera"] == prev["camera"]
            close = same and 0 <= row["taken_at"] - prev["taken_at"] <= window
            alike = close and (
                imaging.hamming(int(row["dhash"], 16), int(anchor["dhash"], 16)) <= max_hamming
            )
            if alike:
                current.append(row)
                continue
            if len(current) > 1:
                bursts.append(current)
        current = [row]
    if len(current) > 1:
        bursts.append(current)
    return bursts


def choose_keeper(members: list[sqlite3.Row]) -> str:
    """Favourites always win; otherwise the sharpest frame; ties go to the earliest."""
    favourites = [m for m in members if m["is_favorite"]]
    if favourites:
        return str(favourites[0]["asset_id"])
    best = max(members, key=lambda m: m["sharpness"] or 0.0)
    return str(best["asset_id"])


Desired = dict[tuple[str, str], tuple[float, str, int | None]]


def _desired_single(conn: sqlite3.Connection, settings: Settings, keep: set[str]) -> Desired:
    desired: Desired = {}
    cur = conn.execute(
        f"SELECT asset_id, scores FROM assets WHERE {ELIGIBLE_SQL} "  # noqa: S608 - constant SQL
        "AND scores IS NOT NULL AND error IS NULL"
    )
    for row in cur:
        if row["asset_id"] in keep:
            continue
        scores = json.loads(row["scores"])
        for det in DETECTORS:
            f = scores.get(det.name)
            if f and det.flags(float(f["score"]), bool(f.get("veto")), settings):
                desired[(row["asset_id"], det.name)] = (float(f["score"]), str(f["reason"]), None)
    return desired


def _desired_bursts(conn: sqlite3.Connection, settings: Settings, keep: set[str]) -> Desired:
    desired: Desired = {}
    # Favourites take part (as untouchable keepers); archived/trashed/unknown-camera do not.
    rows = conn.execute(
        "SELECT asset_id, owner_id, camera, taken_at, dhash, sharpness, is_favorite, file_name "
        "FROM assets WHERE present=1 AND type='IMAGE' AND visibility='timeline' AND is_trashed=0 "
        "AND dhash IS NOT NULL AND taken_at IS NOT NULL AND camera != '' "
        "ORDER BY owner_id, camera, taken_at, asset_id"
    ).fetchall()
    for burst in find_bursts(rows, settings.burst_window_seconds, settings.burst_hamming):
        ids = [str(m["asset_id"]) for m in burst]
        removable = [m for m in burst if not m["is_favorite"] and m["asset_id"] not in keep]
        if not removable:
            continue
        group_id, keeper = db.upsert_group(
            conn, "burst", _group_key("burst", "", ids), ids, choose_keeper(burst)
        )
        for m in removable:
            if m["asset_id"] == keeper:
                continue
            desired[(str(m["asset_id"]), "burst")] = (
                float(m["sharpness"] or 0.0),
                f"One of {len(burst)} near-identical shots taken within seconds; "
                "another frame is proposed as the one to keep",
                group_id,
            )
    return desired


def _desired_duplicates(
    conn: sqlite3.Connection, groups: list[DuplicateGroup], keep: set[str], scan_id: int
) -> Desired:
    desired: Desired = {}
    for group in groups:
        members = [a for a in group.assets if not a.is_trashed]
        if len(members) < 2:
            continue
        for a in members:
            _upsert_meta(conn, a, scan_id)
        ids = [a.id for a in members]
        favourite = next((a.id for a in members if a.is_favorite), None)
        suggested = next((i for i in group.suggested_keep_ids if i in ids), None)
        keeper = favourite or suggested or max(members, key=lambda a: a.file_size or 0).id
        group_id, keeper = db.upsert_group(
            conn, "duplicate", _group_key("duplicate", group.duplicate_id, ids), ids, keeper
        )
        for a in members:
            protected = a.is_favorite or a.is_archived or a.id in keep or a.type != "IMAGE"
            if a.id == keeper or protected:
                continue
            desired[(a.id, "duplicate")] = (
                float(len(members)),
                f"Immich found {len(members)} copies of this photo; another copy is kept",
                group_id,
            )
    return desired


def rebuild_suggestions(
    conn: sqlite3.Connection,
    settings: Settings,
    duplicates: list[DuplicateGroup] | None,
    scan_id: int,
) -> int:
    """Recompute the pending queue from stored analysis and current thresholds.

    ``duplicates=None`` means "could not ask Immich": existing duplicate suggestions are
    left untouched.
    """
    keep = db.keep_ids(conn)
    desired = _desired_single(conn, settings, keep)
    categories = [d.name for d in DETECTORS]
    categories.append("burst")
    if settings.detect_burst:
        desired.update(_desired_bursts(conn, settings, keep))
    if duplicates is not None:
        categories.append("duplicate")
        if settings.detect_duplicates:
            desired.update(_desired_duplicates(conn, duplicates, keep, scan_id))
    new = db.reconcile(conn, desired, categories)
    db.prune_groups(conn)
    return new


# --- the scan --------------------------------------------------------------------------


class Scanner:
    def __init__(
        self,
        settings: Settings,
        client: ImmichClient,
        stop: threading.Event | None = None,
    ) -> None:
        self.settings = settings
        self.client = client
        self.stop = stop or threading.Event()

    def run(self, *, suggest: bool = True, limit: int | None = None) -> ScanResult:
        """Run one scan. ``limit`` overrides CURATOR_SCAN_BATCH (0 = unlimited)."""
        with ScanLock(self.settings.db_path), db.open_db(self.settings.db_path) as conn:
            # We hold the lock, so any scan still marked running died without cleaning up.
            conn.execute(
                "UPDATE scans SET status='interrupted', finished_at=?, "
                "message='process ended before the scan finished' WHERE status='running'",
                (time.time(),),
            )
            cur = conn.execute(
                "INSERT INTO scans (started_at, status) VALUES (?, 'running')", (time.time(),)
            )
            conn.commit()
            result = ScanResult(scan_id=int(cur.lastrowid or 0))
            try:
                self._run(conn, result, suggest, limit)
            except ImmichError as exc:
                conn.rollback()
                result.status = "failed"
                result.message = str(exc)
                log.error("scan failed: %s", exc)
            except BaseException:
                conn.rollback()
                result.status = "interrupted"
                result.message = "interrupted"
                self._finish(conn, result)
                raise
            self._finish(conn, result)
        if result.status == "completed" and suggest and result.new_suggestions > 0:
            notify(self.settings, result)
        return result

    def _finish(self, conn: sqlite3.Connection, r: ScanResult) -> None:
        message = "; ".join([m for m in [r.message, *r.warnings] if m])
        conn.execute(
            "UPDATE scans SET finished_at=?, status=?, listed=?, analysed=?, skipped=?, errors=?, "
            "remaining=?, new_suggestions=?, message=? WHERE id=?",
            (
                time.time(),
                r.status,
                r.listed,
                r.analysed,
                r.skipped,
                r.errors,
                r.remaining,
                r.new_suggestions,
                message[:1000],
                r.scan_id,
            ),
        )
        conn.commit()

    def _run(
        self, conn: sqlite3.Connection, r: ScanResult, suggest: bool, limit: int | None
    ) -> None:
        s = self.settings
        budget = s.scan_batch if limit is None else limit
        unlimited = budget == 0
        with ThreadPoolExecutor(max_workers=s.scan_concurrency) as pool:
            for page in self.client.iter_image_pages(s.scan_page_size):
                todo: list[Asset] = []
                for asset in page:
                    _upsert_meta(conn, asset, r.scan_id)
                    r.listed += 1
                    if not _needs_analysis(conn, asset):
                        r.skipped += 1
                    elif unlimited or r.analysed + len(todo) < budget:
                        todo.append(asset)
                    else:
                        r.remaining += 1
                try:
                    for analysis in pool.map(self._analyse_one, todo):
                        if analysis is None:  # stop requested before this one started
                            continue
                        _store_analysis(conn, analysis)
                        r.analysed += 1
                        r.errors += analysis.error is not None
                finally:
                    # Keep whatever finished, even when one download aborts the scan.
                    conn.commit()
                    conn.execute(
                        "UPDATE scans SET listed=?, analysed=?, skipped=?, errors=? WHERE id=?",
                        (r.listed, r.analysed, r.skipped, r.errors, r.scan_id),
                    )
                    conn.commit()
                if self.stop.is_set():
                    r.status = "interrupted"
                    r.message = "stopped on request; the next scan continues where this one left"
                    return

        # The listing is complete: anything not seen has left the library (or was trashed).
        conn.execute(
            "UPDATE assets SET present=0 WHERE last_seen_scan != ? AND present=1", (r.scan_id,)
        )
        if suggest:
            duplicates: list[DuplicateGroup] | None = None
            if s.detect_duplicates:
                try:
                    duplicates = self.client.get_duplicates()
                except ImmichError as exc:
                    r.warnings.append(f"duplicates skipped: {exc}")
                    log.warning("could not read Immich duplicates: %s", exc)
            else:
                duplicates = []
            r.new_suggestions = rebuild_suggestions(conn, s, duplicates, r.scan_id)
        conn.commit()
        r.status = "completed"
        if r.remaining:
            r.message = (
                f"{r.remaining} photos still to analyse; the next scan continues "
                f"(CURATOR_SCAN_BATCH={budget})"
            )

    def _analyse_one(self, asset: Asset) -> Analysis | None:
        if self.stop.is_set():
            return None
        if self.settings.scan_sleep_ms:
            time.sleep(self.settings.scan_sleep_ms / 1000.0)
        return analyse_asset(self.client, asset)


def notify(settings: Settings, result: ScanResult) -> None:
    """Call notify.sh if it exists. Never raises."""
    script = settings.notify_script
    try:
        if not script or not os.path.isfile(script):
            return
        title = "Photo curator: new suggestions"
        body = (
            f"{result.new_suggestions} new photo(s) to review "
            f"({result.analysed} analysed this run). Open Drive > Curator to review."
        )
        subprocess.run([script, title, body], timeout=60, check=False)  # noqa: S603
    except Exception as exc:
        log.warning("notify script failed (ignored): %s", exc)
