from __future__ import annotations

import dataclasses
import sqlite3
import threading
from datetime import timedelta
from pathlib import Path

import pytest

from home_driver_curator import db
from home_driver_curator.config import Settings
from home_driver_curator.immich import ImmichClient
from home_driver_curator.scan import ScanAlreadyRunning, ScanLock, Scanner

from .conftest import T0, USER, jpeg, pending, seed_library
from .fake_immich import FakeState

MUTATING = [("DELETE", "/api/"), ("POST", "/api/stacks"), ("POST", "/api/trash"), ("PUT", "/api/")]


def scan(settings: Settings, immich: ImmichClient, **kw: object):  # type: ignore[no-untyped-def]
    return Scanner(settings, immich).run(**kw)  # type: ignore[arg-type]


def test_scan_fills_the_queue_and_respects_exclusions(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    result = scan(settings, immich)
    assert result.status == "completed" and result.errors == 0
    assert pending(conn) == {
        ("blurry.jpg", "blurry"),
        ("black.jpg", "dark"),
        ("Screenshot_20240501-101500.png", "screenshot"),
        ("burst_a.jpg", "burst"),
        ("burst_c.jpg", "burst"),
    }
    assert result.new_suggestions == 5
    # favourite / archived / trashed / video / sharp / sky are never suggested
    suggested = {name for name, _ in pending(conn)}
    assert not suggested & {
        "blurry_fav.jpg",
        "blurry_archived.jpg",
        "blurry_trashed.jpg",
        "blurry_video.mp4",
        "sharp.jpg",
        "sky.jpg",
        "burst_b.jpg",
    }
    # the scan is read-only towards Immich
    for method, prefix in MUTATING:
        assert fake.calls(method, prefix) == []
    scan_row = db.last_scan(conn)
    assert scan_row is not None and scan_row["status"] == "completed"


def test_second_scan_does_no_reanalysis_and_changed_asset_is_redone(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    ids = seed_library(fake)
    first = scan(settings, immich)
    served = fake.thumbnails_served
    assert first.analysed == served > 0

    second = scan(settings, immich)
    assert second.analysed == 0 and second.new_suggestions == 0
    assert fake.thumbnails_served == served
    assert second.skipped == second.listed

    # the blurry photo is replaced by a sharp edit: new checksum + updatedAt
    changed = fake.assets[ids["blurry.jpg"]]
    changed.image = jpeg("natural", 9)
    changed.updated = "2024-07-01T00:00:00.000Z"
    third = scan(settings, immich)
    assert third.analysed == 1 and fake.thumbnails_served == served + 1
    assert ("blurry.jpg", "blurry") not in pending(conn)
    status = conn.execute(
        "SELECT s.status FROM suggestions s WHERE s.asset_id=?", (ids["blurry.jpg"],)
    ).fetchone()[0]
    assert status == "withdrawn"


def test_interrupted_scan_resumes_where_it_stopped(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    fake.fail_thumbnails_after = 4  # Immich "goes down" mid-scan
    first = scan(settings, immich)
    assert first.status == "failed" and first.analysed == 4
    assert pending(conn) == set()  # no half-built queue
    done = conn.execute("SELECT COUNT(*) FROM assets WHERE signature IS NOT NULL").fetchone()[0]
    assert done == 4

    fake.fail_thumbnails_after = None
    served = fake.thumbnails_served
    second = scan(settings, immich)
    assert second.status == "completed"
    assert second.skipped == 4  # nothing analysed twice
    assert fake.thumbnails_served - served == second.analysed == second.listed - 4
    assert ("blurry.jpg", "blurry") in pending(conn)


def test_stop_request_ends_the_scan_cleanly(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    stop = threading.Event()
    stop.set()
    result = Scanner(dataclasses.replace(settings, scan_page_size=3), immich, stop).run()
    assert result.status == "interrupted" and result.analysed == 0
    assert db.last_scan(conn)["status"] == "interrupted"
    assert scan(settings, immich).status == "completed"


def test_batch_budget_spreads_the_work_over_several_runs(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    small = dataclasses.replace(settings, scan_batch=4, scan_page_size=3)
    runs = [scan(small, immich) for _ in range(4)]
    assert [r.analysed for r in runs[:2]] == [4, 4]
    assert runs[0].remaining > 0 and runs[-1].remaining == 0
    assert sum(r.analysed for r in runs) == runs[0].listed
    assert ("blurry.jpg", "blurry") in pending(conn)
    assert len(fake.calls("POST", "/api/search/metadata")) >= 4 * 4  # paginated every run


def test_only_one_scan_at_a_time(immich: ImmichClient, settings: Settings) -> None:
    with ScanLock(settings.db_path), pytest.raises(ScanAlreadyRunning):
        scan(settings, immich)
    assert scan(settings, immich).status == "completed"


def test_crashed_scan_is_marked_interrupted(
    immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    conn.execute("INSERT INTO scans (started_at, status) VALUES (1, 'running')")
    conn.commit()
    scan(settings, immich)
    statuses = [r[0] for r in conn.execute("SELECT status FROM scans ORDER BY id")]
    assert statuses == ["interrupted", "completed"]


def test_keep_listed_assets_are_never_suggested_again(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    ids = seed_library(fake)
    db.add_keep(conn, ids["blurry.jpg"], USER, "blurry")
    db.add_keep(conn, ids["burst_a.jpg"], USER, "burst")
    conn.commit()
    scan(settings, immich)
    names = {name for name, _ in pending(conn)}
    assert "blurry.jpg" not in names and "burst_a.jpg" not in names
    assert ("burst_c.jpg", "burst") in pending(conn)


def test_favouriting_after_the_scan_withdraws_the_suggestion(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    ids = seed_library(fake)
    scan(settings, immich)
    fake.assets[ids["blurry.jpg"]].favorite = True
    fake.assets[ids["black.jpg"]].visibility = "archive"
    fake.assets[ids["Screenshot_20240501-101500.png"]].trashed = True
    scan(settings, immich)
    names = {name for name, _ in pending(conn)}
    assert not names & {"blurry.jpg", "black.jpg", "Screenshot_20240501-101500.png"}


def test_thresholds_apply_without_downloading_again(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    scan(settings, immich)
    served = fake.thumbnails_served
    off = dataclasses.replace(settings, detect_blurry=False, dark_threshold=0.5, detect_burst=False)
    scan(off, immich)
    assert fake.thumbnails_served == served
    assert pending(conn) == {("Screenshot_20240501-101500.png", "screenshot")}


# --- bursts ---------------------------------------------------------------------------


def burst(fake: FakeState, **overrides: dict[str, object]) -> dict[str, str]:
    start = T0 + timedelta(days=9)
    frames = {
        "b_soft.jpg": (0, 2.0, 0),
        "b_crisp.jpg": (3, 0.0, 1),
        "b_mid.jpg": (6, 1.0, 2),
    }
    ids = {}
    for name, (shift, blur, second) in frames.items():
        kw = {"taken": start + timedelta(seconds=second), **overrides.get(name, {})}
        ids[name] = fake.add(name, jpeg("burst_frame", 7, shift, blur), **kw).id  # type: ignore[arg-type]
    return ids


def keeper_of(conn: sqlite3.Connection) -> str:
    return conn.execute(
        "SELECT a.file_name FROM groups g JOIN assets a ON a.asset_id=g.keeper_asset_id"
    ).fetchone()[0]


def test_burst_keeps_the_sharpest(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    burst(fake)
    scan(settings, immich)
    assert keeper_of(conn) == "b_crisp.jpg"
    assert pending(conn) == {("b_soft.jpg", "burst"), ("b_mid.jpg", "burst")}


def test_favourite_wins_over_sharpness_and_is_never_suggested(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    burst(fake, **{"b_soft.jpg": {"favorite": True}})
    scan(settings, immich)
    assert keeper_of(conn) == "b_soft.jpg"
    assert pending(conn) == {("b_crisp.jpg", "burst"), ("b_mid.jpg", "burst")}


def test_frames_far_apart_or_from_other_cameras_are_not_a_burst(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    start = T0 + timedelta(days=9)
    fake.add("t0.jpg", jpeg("burst_frame", 7, 0, 0.0), taken=start)
    fake.add("t60.jpg", jpeg("burst_frame", 7, 3, 0.0), taken=start + timedelta(seconds=60))
    fake.add(
        "other_cam.jpg",
        jpeg("burst_frame", 7, 6, 0.0),
        taken=start + timedelta(seconds=61),
        make="Samsung",
        model="S23",
    )
    fake.add("no_cam.jpg", jpeg("burst_frame", 7, 2, 0.0), taken=start, make=None, model=None)
    fake.add("different.jpg", jpeg("natural", 2), taken=start + timedelta(seconds=1))
    scan(settings, immich)
    assert {c for _, c in pending(conn)} <= {"screenshot"}  # no burst suggestions at all
    assert conn.execute("SELECT COUNT(*) FROM groups").fetchone()[0] == 0


# --- duplicates -----------------------------------------------------------------------

DUP = "44444444-4444-4444-8444-444444444444"


def test_immich_duplicates_are_surfaced(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    a = fake.add("copy1.jpg", jpeg("natural", 1)).id
    b = fake.add("copy2.jpg", jpeg("natural", 1)).id
    c = fake.add("copy3.jpg", jpeg("natural", 1), favorite=True).id
    fake.duplicates.append({"duplicateId": DUP, "ids": [a, b, c], "keep": [a]})
    scan(settings, immich)
    # the favourite becomes the keeper and is untouchable; Immich's pick loses to it
    assert keeper_of(conn) == "copy3.jpg"
    assert pending(conn) == {("copy1.jpg", "duplicate"), ("copy2.jpg", "duplicate")}


def test_missing_duplicate_permission_does_not_fail_the_scan(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    seed_library(fake)
    fake.permissions = ["asset.read", "asset.view"]
    result = scan(settings, immich)
    assert result.status == "completed"
    assert any("duplicate.read" in w for w in result.warnings)
    assert ("blurry.jpg", "blurry") in pending(conn)


def test_missing_read_permission_fails_with_a_clear_message(
    fake: FakeState, immich: ImmichClient, settings: Settings
) -> None:
    seed_library(fake)
    fake.permissions = ["asset.read"]
    result = scan(settings, immich)
    assert result.status == "failed" and "asset.view" in result.message


# --- notify ---------------------------------------------------------------------------


def test_notify_script_is_called_when_there_is_news(
    fake: FakeState, immich: ImmichClient, settings: Settings, tmp_path: Path
) -> None:
    seed_library(fake)
    out = tmp_path / "notified.txt"
    script = tmp_path / "notify.sh"
    script.write_text(f'#!/bin/sh\nprintf "%s|%s\\n" "$1" "$2" >> "{out}"\n')
    script.chmod(0o755)
    with_script = dataclasses.replace(settings, notify_script=str(script))
    scan(with_script, immich)
    assert "new photo(s) to review" in out.read_text()
    scan(with_script, immich)  # nothing new: no second notification
    assert len(out.read_text().splitlines()) == 1


def test_missing_or_broken_notify_script_never_fails_the_scan(
    fake: FakeState, immich: ImmichClient, settings: Settings, tmp_path: Path
) -> None:
    seed_library(fake)
    assert scan(settings, immich).status == "completed"  # script path does not exist
    broken = tmp_path / "broken.sh"
    broken.write_text("#!/bin/sh\nexit 7\n")  # not executable -> PermissionError
    fake.add("late_black.jpg", jpeg("black", 2))
    result = scan(dataclasses.replace(settings, notify_script=str(broken)), immich)
    assert result.status == "completed" and result.new_suggestions == 1


def test_unreadable_preview_is_recorded_not_fatal(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> None:
    fake.add("broken.jpg", b"this is not an image")
    fake.add("fine.jpg", jpeg("black", 1))
    first = scan(settings, immich)
    assert first.status == "completed" and first.errors == 1 and first.analysed == 2
    assert pending(conn) == {("fine.jpg", "dark")}
    for _ in range(4):  # retried a limited number of times, then left alone
        last = scan(settings, immich)
    assert last.analysed == 0
