from __future__ import annotations

import dataclasses
import re
import sqlite3
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urljoin, urlsplit

import pytest
from fastapi.testclient import TestClient

from home_driver_curator import cli, i18n
from home_driver_curator.config import ConfigError, Settings, parse_addr
from home_driver_curator.immich import ImmichClient
from home_driver_curator.scan import Scanner
from home_driver_curator.web import create_app

from .conftest import USER, jpeg, pending, seed_library, suggestion_id
from .fake_immich import API_KEY, FakeState

FORM = {"category": "blurry", "mode": "one", "i": "0", "page": "1"}


@pytest.fixture
def lib(
    fake: FakeState, immich: ImmichClient, settings: Settings, conn: sqlite3.Connection
) -> dict[str, str]:
    ids = seed_library(fake)
    assert Scanner(settings, immich).run().status == "completed"
    fake.requests.clear()
    return ids


def post(web: TestClient, path: str, data: dict[str, object], **kw: object):  # type: ignore[no-untyped-def]
    return web.post(path, data=data, **kw)  # type: ignore[arg-type]


def status_of(conn: sqlite3.Connection, sid: int) -> str:
    return conn.execute("SELECT status FROM suggestions WHERE id=?", (sid,)).fetchone()[0]


def forced_deletes(fake: FakeState) -> list[dict]:
    return [r for r in fake.calls("DELETE", "/api/") if (r["json"] or {}).get("force") is not False]


# --- security ---------------------------------------------------------------------------


@pytest.mark.parametrize("path", ["/", "/review?category=blurry", "/activity", "/thumb?id=x"])
def test_pages_need_the_proxy_header(web: TestClient, lib: dict[str, str], path: str) -> None:
    assert web.get(path, headers={"X-Home-Driver-Email": ""}).status_code == 403


@pytest.mark.parametrize("path", ["/keep", "/trash", "/stack", "/keeper", "/undo"])
def test_actions_need_the_proxy_header_and_post(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection, path: str
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    r = post(web, path, {**FORM, "one": sid}, headers={"X-Home-Driver-Email": ""})
    assert r.status_code == 403
    assert web.get(path).status_code == 405
    assert status_of(conn, sid) == "pending" and fake.requests == []


@pytest.mark.parametrize(
    "headers",
    [
        {"Origin": "https://evil.example"},
        {"Sec-Fetch-Site": "cross-site"},
        {"Sec-Fetch-Site": "same-site", "Origin": "http://curator.test"},
    ],
)
def test_cross_site_posts_are_refused(
    web: TestClient,
    fake: FakeState,
    lib: dict[str, str],
    conn: sqlite3.Connection,
    headers: dict[str, str],
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    assert post(web, "/trash", {**FORM, "one": sid}, headers=headers).status_code == 403
    assert status_of(conn, sid) == "pending" and fake.requests == []


@pytest.mark.parametrize(
    "headers",
    [
        {"Sec-Fetch-Site": "same-origin"},
        {"Origin": "http://curator.test"},
        {"Origin": "https://drive.example.com", "X-Forwarded-Host": "drive.example.com"},
    ],
)
def test_same_origin_posts_are_accepted(
    web: TestClient, lib: dict[str, str], conn: sqlite3.Connection, headers: dict[str, str]
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    assert post(web, "/keep", {**FORM, "one": sid}, headers=headers).status_code == 303
    assert status_of(conn, sid) == "kept"


@pytest.mark.parametrize("addr", ["0.0.0.0:8091", "192.168.1.10:8091", "[::]:8091", ":8091", "x"])
def test_non_loopback_bind_is_refused(addr: str) -> None:
    with pytest.raises(ConfigError):
        parse_addr(addr)
    with pytest.raises(ConfigError):
        Settings.from_env({"CURATOR_ADDR": addr})


def test_loopback_addresses_are_accepted() -> None:
    assert parse_addr("127.0.0.1:8091") == ("127.0.0.1", 8091)
    assert parse_addr("[::1]:9000") == ("::1", 9000)
    assert Settings.from_env({}).port == 8091


def test_serve_refuses_to_start_on_a_public_address(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setenv("CURATOR_ADDR", "0.0.0.0:8091")
    monkeypatch.setenv("CURATOR_IMMICH_API_KEY", "k")
    assert cli.main(["serve"]) == 2
    assert "loopback" in capsys.readouterr().err


# --- trash / undo -----------------------------------------------------------------------


def test_single_approval_moves_to_trash_without_force(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    r = post(web, "/trash", {**FORM, "one": sid})
    assert r.status_code == 303
    location = r.headers["location"]
    assert location.startswith("review?") and "m=msg_trashed" in location and "u=" in location

    (call,) = fake.calls("DELETE", "/api/assets")
    assert call["json"] == {"ids": [lib["blurry.jpg"]], "force": False}
    assert forced_deletes(fake) == []
    assert fake.assets[lib["blurry.jpg"]].trashed
    assert not fake.assets[lib["blurry.jpg"]].deleted_forever
    assert status_of(conn, sid) == "trashed"

    row = conn.execute("SELECT * FROM activity ORDER BY id DESC").fetchone()
    assert (row["user"], row["action"], row["count"], row["dry_run"]) == (USER, "trash", 1, 0)
    assert "blurry.jpg" in row["assets"]

    page = web.get("/" + location)
    assert page.status_code == 200 and "30" in page.text and 'action="undo"' in page.text


def test_bulk_approval_needs_one_confirmation_showing_the_count(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    for n in range(3):
        fake.add(f"black_{n}.jpg", jpeg("black", n + 2))
    Scanner(
        dataclasses.replace(
            Settings(),
            db_path=conn.execute("PRAGMA database_list").fetchone()[2],
            immich_api_key=API_KEY,
            scan_concurrency=1,
            scan_batch=0,
            notify_script="",
        ),
        web.app.state.immich,
    ).run()
    fake.requests.clear()
    sids = [
        r[0]
        for r in conn.execute(
            "SELECT id FROM suggestions WHERE category='dark' AND status='pending'"
        )
    ]
    assert len(sids) == 4
    form = {"category": "dark", "mode": "grid", "page": "1", "sel": sids}

    ask = post(web, "/trash", form, headers={"Accept-Language": "en"})
    assert ask.status_code == 200 and "Move 4 photos to the trash?" in ask.text
    assert fake.requests == []  # nothing happened yet

    done = post(web, "/trash", {**form, "confirm": "yes"})
    assert done.status_code == 303 and "n=4" in done.headers["location"]
    (call,) = fake.calls("DELETE", "/api/assets")
    assert len(call["json"]["ids"]) == 4 and call["json"]["force"] is False
    assert forced_deletes(fake) == []


def test_undo_restores_from_the_immich_trash(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    r = post(web, "/trash", {**FORM, "one": sid})
    undo_id = re.search(r"u=(\d+)", r.headers["location"]).group(1)  # type: ignore[union-attr]

    back = post(web, "/undo", {**FORM, "activity": undo_id})
    assert back.status_code == 303 and "m=msg_restored" in back.headers["location"]
    (call,) = fake.calls("POST", "/api/trash/restore/assets")
    assert call["json"] == {"ids": [lib["blurry.jpg"]]}
    assert not fake.assets[lib["blurry.jpg"]].trashed
    assert status_of(conn, sid) == "pending"
    actions = [r[0] for r in conn.execute("SELECT action FROM activity ORDER BY id")]
    assert actions == ["trash", "restore"]

    again = post(web, "/undo", {**FORM, "activity": undo_id})  # cannot be undone twice
    assert "m=msg_stale" in again.headers["location"]
    assert len(fake.calls("POST", "/api/trash/restore/assets")) == 1


def test_dry_run_calls_nothing_and_says_so(
    fake: FakeState,
    immich: ImmichClient,
    settings: Settings,
    lib: dict[str, str],
    conn: sqlite3.Connection,
) -> None:
    dry = dataclasses.replace(settings, dry_run=True)
    web = TestClient(
        create_app(dry, immich),
        headers={"X-Home-Driver-Email": USER, "Accept-Language": "en"},
        follow_redirects=False,
    )
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    r = post(web, "/trash", {**FORM, "one": sid})
    assert "m=msg_trash_dry" in r.headers["location"]
    gid = conn.execute("SELECT id FROM groups WHERE kind='burst'").fetchone()[0]
    r2 = post(web, "/stack", {"category": "burst", "group": gid})
    assert "m=msg_stack_dry" in r2.headers["location"]
    assert fake.requests == []  # not a single call reached Immich
    assert status_of(conn, sid) == "pending"
    assert not fake.assets[lib["blurry.jpg"]].trashed
    page = web.get("/" + r.headers["location"])
    assert "Practice mode" in page.text and "Nothing was changed" in page.text
    row = conn.execute("SELECT dry_run, action FROM activity ORDER BY id").fetchone()
    assert tuple(row) == (1, "trash")


def test_favourited_since_the_scan_is_refused_at_approval_time(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    fake.assets[lib["blurry.jpg"]].favorite = True
    r = post(web, "/trash", {**FORM, "one": sid})
    assert "m=msg_stale" in r.headers["location"] and "sk=1" in r.headers["location"]
    assert fake.calls("DELETE", "/api/") == []
    assert status_of(conn, sid) == "withdrawn"


def test_archived_since_the_scan_is_refused_at_approval_time(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "black.jpg", "dark")
    fake.assets[lib["black.jpg"]].visibility = "archive"
    post(web, "/trash", {**FORM, "category": "dark", "one": sid})
    assert fake.calls("DELETE", "/api/") == []


def test_immich_refusal_is_reported_and_changes_nothing(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    fake.permissions = ["asset.read", "asset.view"]
    r = post(web, "/trash", {**FORM, "one": sid})
    assert "m=msg_immich_denied" in r.headers["location"]
    assert status_of(conn, sid) == "pending" and not fake.assets[lib["blurry.jpg"]].trashed


# --- keep -------------------------------------------------------------------------------


def test_keep_is_remembered_forever(
    web: TestClient,
    fake: FakeState,
    immich: ImmichClient,
    settings: Settings,
    lib: dict[str, str],
    conn: sqlite3.Connection,
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    r = post(web, "/keep", {**FORM, "one": sid})
    assert "m=msg_kept" in r.headers["location"]
    assert fake.requests == []  # keeping never touches Immich
    assert status_of(conn, sid) == "kept"
    assert conn.execute("SELECT added_by FROM keep_list").fetchone()[0] == USER

    changed = fake.assets[lib["blurry.jpg"]]  # even a re-analysis must not bring it back
    changed.updated = "2025-01-01T00:00:00.000Z"
    Scanner(settings, immich).run()
    assert ("blurry.jpg", "blurry") not in pending(conn)
    # and a stale form post cannot trash a kept photo
    post(web, "/trash", {**FORM, "one": sid})
    assert fake.calls("DELETE", "/api/") == []


# --- bursts -----------------------------------------------------------------------------


def group_id(conn: sqlite3.Connection) -> int:
    return conn.execute("SELECT id FROM groups WHERE kind='burst'").fetchone()[0]


BURST = {"category": "burst", "mode": "one", "i": "0", "page": "1"}


def test_burst_group_approval_keeps_the_keeper(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    r = post(web, "/trash", {**BURST, "group": group_id(conn)})
    assert "m=msg_trashed" in r.headers["location"] and "n=2" in r.headers["location"]
    (call,) = fake.calls("DELETE", "/api/assets")
    assert sorted(call["json"]["ids"]) == sorted([lib["burst_a.jpg"], lib["burst_c.jpg"]])
    assert call["json"]["force"] is False
    assert not fake.assets[lib["burst_b.jpg"]].trashed


def test_keeper_can_be_changed_before_approving(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    gid = group_id(conn)
    r = post(web, "/keeper", {**BURST, "choose": f"{gid}:{lib['burst_c.jpg']}"})
    assert "m=msg_keeper" in r.headers["location"] and fake.requests == []
    assert pending(conn) >= {("burst_a.jpg", "burst"), ("burst_b.jpg", "burst")}
    assert ("burst_c.jpg", "burst") not in pending(conn)

    # the choice survives the next scan
    Scanner(web.app.state.settings, web.app.state.immich).run()
    assert ("burst_c.jpg", "burst") not in pending(conn)
    fake.requests.clear()

    post(web, "/trash", {**BURST, "group": gid})
    (call,) = fake.calls("DELETE", "/api/assets")
    assert sorted(call["json"]["ids"]) == sorted([lib["burst_a.jpg"], lib["burst_b.jpg"]])
    assert not fake.assets[lib["burst_c.jpg"]].trashed


def test_stack_instead_deletes_nothing(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    r = post(web, "/stack", {**BURST, "group": group_id(conn)})
    assert "m=msg_stacked" in r.headers["location"]
    assert fake.stacks == [[lib["burst_b.jpg"], lib["burst_a.jpg"], lib["burst_c.jpg"]]]
    assert fake.calls("DELETE", "/api/") == []
    assert not any(a.trashed for a in fake.assets.values() if a.name.startswith("burst"))
    assert not {c for _, c in pending(conn)} & {"burst"}
    assert conn.execute("SELECT action FROM activity").fetchone()[0] == "stack"


def test_group_is_refused_when_its_keeper_is_gone(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    fake.assets[lib["burst_b.jpg"]].trashed = True  # keeper trashed directly in Immich
    r = post(web, "/trash", {**BURST, "group": group_id(conn)})
    assert "m=msg_stale" in r.headers["location"]
    assert fake.calls("DELETE", "/api/") == []
    assert not fake.assets[lib["burst_a.jpg"]].trashed


# --- pages, prefix, escaping, language --------------------------------------------------


class Links(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.urls: list[tuple[str, str]] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        for name, value in attrs:
            if name in ("href", "src", "action", "formaction", "poster", "srcset") and value:
                self.urls.append((name, value))


PAGES = [
    "/",
    "/review?category=blurry",
    "/review?category=blurry&mode=grid",
    "/review?category=screenshot",
    "/review?category=burst",
    "/review?category=duplicate",
    "/review?category=dark&m=msg_trashed&n=1&u=1",
    "/activity",
    "/nope",
]


@pytest.mark.parametrize("lang", ["vi", "en"])
def test_every_url_is_relative_and_works_under_the_curator_prefix(
    web: TestClient, fake: FakeState, lib: dict[str, str], conn: sqlite3.Connection, lang: str
) -> None:
    post(
        web, "/trash", {**FORM, "category": "dark", "one": suggestion_id(conn, "black.jpg", "dark")}
    )
    sids = [r[0] for r in conn.execute("SELECT id FROM suggestions WHERE status='pending' LIMIT 2")]
    bodies = {p: web.get(p, headers={"Accept-Language": lang}).text for p in PAGES}
    bodies["/trash"] = post(web, "/trash", {"category": "blurry", "mode": "grid", "sel": sids}).text
    seen = 0
    for page, html in bodies.items():
        parser = Links()
        parser.feed(html)
        assert parser.urls, page
        for attr, url in parser.urls:
            if url.startswith("https://photos.example.com/photos/"):
                continue  # the configured CURATOR_IMMICH_PUBLIC_URL
            if url.startswith("#"):
                continue  # same-document reference into the inline SVG sprite
            assert not url.startswith("/"), f"absolute path {url!r} on {page}"
            assert "://" not in url and not url.startswith("//"), f"absolute URL {url!r} on {page}"
            public = urljoin("http://family.example/curator" + page.split("?")[0], url)
            parts = urlsplit(public)
            if url == "../":
                assert parts.path == "/"  # the link back to Drive
                continue
            assert parts.path.startswith("/curator/"), f"{url!r} on {page} escapes the prefix"
            inner = parts.path[len("/curator") :] + ("?" + parts.query if parts.query else "")
            if attr in ("action", "formaction"):
                assert web.get(inner).status_code == 405, inner  # POST-only route exists
            else:
                assert web.get(inner).status_code == 200, (
                    f"{inner} via {attr}={url!r} (from {page})"
                )
            seen += 1
    assert seen > 60


def test_redirects_are_relative(
    web: TestClient, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    sid = suggestion_id(conn, "blurry.jpg", "blurry")
    for path, data in [
        ("/keep", {**FORM, "one": sid}),
        ("/trash", {**FORM}),
        ("/undo", {"activity": "999", "from": "activity"}),
        ("/undo", {"activity": "999"}),
    ]:
        location = post(web, path, data).headers["location"]
        assert not location.startswith("/") and "://" not in location


def test_immich_strings_are_escaped(
    fake: FakeState, immich: ImmichClient, settings: Settings, web: TestClient
) -> None:
    evil = "<script>alert(1)</script>\"'><img src=x onerror=alert(2)>.png"
    fake.add(evil, jpeg("black", 1), make='<b>"cam', model="</title><script>x</script>")
    fake.add('Screenshot "<i>".png', jpeg("screenshot"), mime="image/png", make=None, model=None)
    Scanner(settings, immich).run()
    sid = 1
    pages = [
        web.get("/review?category=dark").text,
        web.get("/review?category=dark&mode=grid").text,
        web.get("/review?category=screenshot").text,
        post(web, "/trash", {"category": "dark", "mode": "grid", "sel": [1, 2]}).text,
    ]
    post(web, "/keep", {"category": "dark", "one": sid})
    pages.append(web.get("/activity").text)
    pages.append(web.get("/?m=<script>alert(3)</script>&n=1").text)
    for html in pages:
        assert "<script>alert" not in html and "<img src=x" not in html
        assert "<script>x</script>" not in html and "<i>" not in html and "<b>" not in html
    assert "&lt;script&gt;alert(1)" in pages[0]


def test_language_follows_the_browser_and_can_be_switched(
    web: TestClient, lib: dict[str, str]
) -> None:
    vi = web.get("/", headers={"Accept-Language": "vi-VN,vi;q=0.9,en;q=0.8"})
    assert '<html lang="vi">' in vi.text and "Dọn dẹp ảnh" in vi.text
    assert "Bắt đầu xem" in vi.text
    en = web.get("/", headers={"Accept-Language": "fr-FR,fr;q=0.9"})
    assert '<html lang="en">' in en.text and "Tidy up your photos" in en.text

    switched = web.get("/?lang=en", headers={"Accept-Language": "vi"})
    cookie = switched.headers["set-cookie"]
    assert "curator_lang=en" in cookie and "path=/" not in cookie.lower().replace(" ", "")
    web.cookies.set("curator_lang", "en")
    assert "Tidy up your photos" in web.get("/", headers={"Accept-Language": "vi"}).text


def test_dictionaries_are_complete() -> None:
    en, vi = i18n.STRINGS["en"], i18n.STRINGS["vi"]

    def plural(k: str, table: dict[str, str]) -> bool:
        return k.endswith("_one") and k[:-4] in table

    assert {k for k in en if not plural(k, en)} == {k for k in vi if not plural(k, vi)}
    for key, text in vi.items():
        if key not in en:
            continue
        assert set(re.findall(r"{(\w+)}", text)) == set(re.findall(r"{(\w+)}", en[key])), key


def test_templates_and_scripts_contain_no_string_literals(web: TestClient) -> None:
    root = Path(i18n.__file__).parent
    for template in (root / "templates").glob("*.html"):
        text = re.sub(r"{% block body_class %}.*?{% endblock %}", "", template.read_text())
        text = re.sub(r"{%.*?%}|{{.*?}}|{#.*?#}", "", text, flags=re.S)
        text = re.sub(r"<svg class=\"sprite\".*?</svg>", "", text, flags=re.S)
        text = re.sub(r"<[^>]+>", " ", text)
        words = re.findall(r"[A-Za-zÀ-ỹ]{3,}", text)
        assert set(words) <= {"Tiếng", "Việt", "English"}, (template.name, words)
    script = (root / "static" / "app.js").read_text()
    assert "innerHTML" not in script


def test_tokens_css_matches_the_design_standard() -> None:
    design = Path(__file__).resolve().parents[2] / "docs" / "DESIGN.md"
    if not design.exists():
        pytest.skip("docs/DESIGN.md not present")
    block = re.search(r"### Tokens.*?```css\n(.*?)```", design.read_text(), re.S).group(1)  # type: ignore[union-attr]
    tokens = (Path(i18n.__file__).parent / "static" / "tokens.css").read_text()
    for line in block.splitlines():
        if "same values as the dark block above" in line:
            continue
        assert line in tokens, line


def test_review_screens_follow_the_safety_rules(web: TestClient, lib: dict[str, str]) -> None:
    en = {"Accept-Language": "en"}
    one = web.get("/review?category=blurry", headers=en).text
    assert "This photo is blurry" in one and "1 left" in one
    assert 'id="btn-keep"' in one and 'id="btn-trash"' in one
    assert "30 days" in one
    grid = web.get("/review?category=screenshot", headers=en).text
    assert "data-select-all" in grid and "checked" not in grid  # nothing preselected
    group = web.get("/review?category=burst", headers=en).text
    assert "3 near-identical photos" in group and "Best" in group and "Stack them instead" in group
    home = web.get("/", headers=en).text
    assert "Only photos are checked" in home  # videos are not analysed, and the UI says so
    for html in (one, grid, group):
        assert "danger" not in html and "autofocus" not in html.split("<dialog")[0]
    empty = web.get("/review?category=duplicate", headers=en).text
    assert "All done here" in empty
    assert web.get("/review?category=bogus").status_code == 404


def test_thumbnails_are_proxied_without_exposing_the_api_key(
    web: TestClient, fake: FakeState, lib: dict[str, str]
) -> None:
    r = web.get(f"/thumb?id={lib['blurry.jpg']}&size=preview")
    assert r.status_code == 200 and r.headers["content-type"] == "image/jpeg"
    assert r.headers["x-content-type-options"] == "nosniff"
    assert web.get(f"/thumb?id={lib['blurry.jpg']}").headers["content-type"] == "image/webp"
    assert web.get("/thumb?id=99999999-9999-4999-8999-999999999999").status_code == 404
    assert web.get("/thumb?id=../../x").status_code == 400
    assert web.get(f"/thumb?id={lib['blurry.jpg']}&size=original").status_code == 400
    for page in PAGES:
        assert API_KEY not in web.get(page).text


def test_activity_log_lists_actions_with_user_and_undo(
    web: TestClient, lib: dict[str, str], conn: sqlite3.Connection
) -> None:
    post(web, "/keep", {**FORM, "one": suggestion_id(conn, "blurry.jpg", "blurry")})
    post(
        web, "/trash", {**FORM, "category": "dark", "one": suggestion_id(conn, "black.jpg", "dark")}
    )
    html = web.get("/activity", headers={"Accept-Language": "en"}).text
    assert html.count(USER) >= 2 and "Kept" in html and "Moved 1 photo to the trash" in html
    assert "black.jpg" in html and "blurry.jpg" in html
    assert html.count('action="undo"') == 1  # only the removal can be undone


def test_healthz_and_static_need_no_header(web: TestClient) -> None:
    anon = {"X-Home-Driver-Email": ""}
    assert web.get("/healthz", headers=anon).text == "ok\n"
    for name in ("tokens.css", "style.css", "app.js", "theme.js"):
        assert web.get(f"/static/{name}", headers=anon).status_code == 200
    assert web.get("/static/../web.py", headers=anon).status_code == 404


def test_parallel_requests_share_nothing(web: TestClient, lib: dict[str, str]) -> None:
    """Regression: per-request SQLite connections are opened and closed on pool threads."""
    from concurrent.futures import ThreadPoolExecutor

    urls = [f"/thumb?id={asset_id}" for asset_id in lib.values() if asset_id] * 3
    with ThreadPoolExecutor(max_workers=8) as pool:
        codes = list(pool.map(lambda u: web.get(u).status_code, urls))
    assert set(codes) <= {200, 404} and codes.count(200) >= 20
