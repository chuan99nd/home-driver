from __future__ import annotations

import sqlite3
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta
from functools import cache
from pathlib import Path

import pytest
from fastapi.testclient import TestClient
from PIL import Image

from home_driver_curator import db
from home_driver_curator.config import Settings
from home_driver_curator.immich import ImmichClient
from home_driver_curator.web import create_app

from . import images
from .fake_immich import API_KEY, FakeState, create_fake

USER = "owner@example.com"
T0 = datetime(2024, 5, 1, 10, 0, 0, tzinfo=UTC)


@cache
def jpeg(kind: str, *args: object) -> bytes:
    """Cached synthetic JPEGs (generation is the slow part of the suite)."""
    img: Image.Image = getattr(images, kind)(*args)
    return images.to_jpeg(img)


@pytest.fixture
def fake() -> FakeState:
    return FakeState()


@pytest.fixture
def sleeps() -> list[float]:
    return []


@pytest.fixture
def immich(fake: FakeState, sleeps: list[float]) -> Iterator[ImmichClient]:
    http = TestClient(create_fake(fake), base_url="http://immich.test")
    client = ImmichClient("http://immich.test", API_KEY, http=http, retries=3, sleep=sleeps.append)
    yield client
    http.close()


@pytest.fixture
def settings(tmp_path: Path) -> Settings:
    return Settings(
        db_path=str(tmp_path / "curator.db"),
        immich_url="http://immich.test",
        immich_api_key=API_KEY,
        immich_public_url="https://photos.example.com",
        notify_script=str(tmp_path / "no-such-notify.sh"),
        scan_concurrency=1,
        scan_batch=0,
    )


@pytest.fixture
def conn(settings: Settings) -> Iterator[sqlite3.Connection]:
    with db.open_db(settings.db_path) as c:
        yield c


@pytest.fixture
def web(settings: Settings, immich: ImmichClient) -> Iterator[TestClient]:
    client = TestClient(
        create_app(settings, immich),
        base_url="http://curator.test",
        headers={"X-Home-Driver-Email": USER},
        follow_redirects=False,
    )
    yield client
    client.close()


def seed_library(fake: FakeState) -> dict[str, str]:
    """One asset of every interesting kind. Returns name -> asset id."""
    ids: dict[str, str] = {}

    def add(name: str, data: bytes, **kw: object) -> None:
        kw.setdefault("taken", T0 + timedelta(hours=len(fake.assets)))
        ids[name] = fake.add(name, data, **kw).id  # type: ignore[arg-type]

    add("sharp.jpg", jpeg("natural", 1))
    add("blurry.jpg", jpeg("blurred", 2, 14.0))
    add("blurry_fav.jpg", jpeg("blurred", 3, 14.0), favorite=True)
    add("blurry_archived.jpg", jpeg("blurred", 4, 14.0), visibility="archive")
    add("blurry_trashed.jpg", jpeg("blurred", 5, 14.0), trashed=True)
    add("blurry_video.mp4", jpeg("blurred", 6, 14.0), type="VIDEO")
    add("black.jpg", jpeg("black", 1))
    add("sky.jpg", jpeg("sky", 1))
    add(
        "Screenshot_20240501-101500.png",
        jpeg("screenshot"),
        mime="image/png",
        make=None,
        model=None,
        width=1080,
        height=2400,
    )
    burst = T0 + timedelta(days=3)
    add("burst_a.jpg", jpeg("burst_frame", 7, 0, 2.0), taken=burst)
    add("burst_b.jpg", jpeg("burst_frame", 7, 3, 0.0), taken=burst + timedelta(seconds=1))
    add("burst_c.jpg", jpeg("burst_frame", 7, 6, 1.0), taken=burst + timedelta(seconds=2))
    return ids


def pending(conn: sqlite3.Connection) -> set[tuple[str, str]]:
    rows = conn.execute(
        "SELECT a.file_name, s.category FROM suggestions s JOIN assets a USING (asset_id) "
        "WHERE s.status='pending'"
    )
    return {(r[0], r[1]) for r in rows}


def suggestion_id(conn: sqlite3.Connection, name: str, category: str) -> int:
    row = conn.execute(
        "SELECT s.id FROM suggestions s JOIN assets a USING (asset_id) "
        "WHERE a.file_name=? AND s.category=?",
        (name, category),
    ).fetchone()
    assert row is not None, f"no {category} suggestion for {name}"
    return int(row[0])
