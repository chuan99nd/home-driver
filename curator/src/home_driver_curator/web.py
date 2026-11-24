"""Review UI. Server-rendered HTML, no build step, no external assets. Follows docs/DESIGN.md.

URL rule: the app is served by Drive under ``/curator/`` with the prefix stripped, so the
app never learns its public path. Therefore EVERY route is one path segment deep
(``review``, ``thumb``, ``trash`` ...) and every link, form action, redirect and asset URL
is relative with no leading slash. The one exception is ``static/<file>``, which is only
ever referenced from one-segment pages. tests/test_web.py enforces this.

String rule: no user-visible literals here or in templates; everything goes through
``i18n.STRINGS`` (Vietnamese + English).
"""

from __future__ import annotations

import json
import logging
import re
import sqlite3
from collections.abc import Iterator
from datetime import UTC, datetime
from importlib import resources
from typing import Any
from urllib.parse import parse_qs, urlencode, urlsplit

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import HTMLResponse, PlainTextResponse, RedirectResponse, Response
from jinja2 import Environment, PackageLoader, select_autoescape
from markupsafe import Markup
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import actions, db, i18n
from .config import Settings
from .detectors import CATEGORY_LABELS, DETECTORS, GROUP_CATEGORIES
from .immich import (
    ImmichAuthError,
    ImmichClient,
    ImmichError,
    ImmichNotFound,
    ImmichPermissionError,
    is_uuid,
)

log = logging.getLogger(__name__)

USER_HEADER = "x-home-driver-email"
LANG_COOKIE = "curator_lang"
GRID_PAGE_SIZE = 60
ACTIVITY_PAGE_SIZE = 50
MAX_FORM_BYTES = 1_000_000
FLASH_PARAMS = ("m", "n", "sk", "u")
# Bulk categories open in grid mode; the emotional ones open one photo at a time.
DEFAULT_MODE = {"screenshot": "grid"}

SECURITY_HEADERS = {
    "Content-Security-Policy": (
        "default-src 'none'; img-src 'self'; style-src 'self'; script-src 'self'; "
        "form-action 'self'; base-uri 'none'; frame-ancestors 'self'"
    ),
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "same-origin",
}

STATIC_TYPES = {
    "tokens.css": "text/css; charset=utf-8",
    "style.css": "text/css; charset=utf-8",
    "theme.js": "text/javascript; charset=utf-8",
    "app.js": "text/javascript; charset=utf-8",
}
PLAIN_PATHS = ("/thumb", "/static/", "/healthz")

Form = dict[str, list[str]]

_ICON_NAME = re.compile(r"^[a-z]+$")


def _icon(name: str) -> Markup:
    """Reference one symbol of the inline SVG sprite in base.html (decorative; labels are text)."""
    if not _ICON_NAME.match(name):
        raise ValueError(name)
    return Markup(  # noqa: S704 - name is validated above
        f'<svg class="icon" aria-hidden="true" focusable="false"><use href="#i-{name}"/></svg>'
    )


def _iso(epoch: float | None) -> str:
    if not epoch:
        return ""
    return datetime.fromtimestamp(float(epoch), tz=UTC).isoformat(timespec="seconds")


def _dt(epoch: float | None) -> str:
    """Fallback date text; app.js replaces it with an Intl-formatted one."""
    if not epoch:
        return ""
    return datetime.fromtimestamp(float(epoch)).strftime("%d/%m/%Y %H:%M")


def require_user(request: Request) -> str:
    """The acting admin, set by Drive's admin-only proxy. Without it nothing is served."""
    email = request.headers.get(USER_HEADER, "").strip()
    if not email:
        raise HTTPException(status_code=403, detail="missing X-Home-Driver-Email")
    return email[:254]


def require_same_origin(request: Request) -> None:
    """Reject cross-site form posts (CSRF). Browsers always send one of these headers."""
    site = request.headers.get("sec-fetch-site")
    if site is not None:
        if site.lower() != "same-origin":
            raise HTTPException(status_code=403, detail="cross-site request refused")
        return
    origin = request.headers.get("origin")
    if origin is None:
        return  # not a browser (curl, tests); the header check still applies
    allowed = {
        request.headers.get("host", "").lower(),
        request.headers.get("x-forwarded-host", "").lower(),
    } - {""}
    if urlsplit(origin).netloc.lower() not in allowed:
        raise HTTPException(status_code=403, detail="cross-site request refused")


async def read_form(request: Request) -> Form:
    ctype = request.headers.get("content-type", "").split(";")[0].strip().lower()
    if ctype != "application/x-www-form-urlencoded":
        raise HTTPException(status_code=415, detail="expected a form post")
    body = await request.body()
    if len(body) > MAX_FORM_BYTES:
        raise HTTPException(status_code=413, detail="form too large")
    return parse_qs(body.decode("utf-8", "replace"), keep_blank_values=False)


def _int(value: str | None, default: int = 0) -> int:
    return int(value) if value and value.isdigit() and len(value) < 10 else default


def _ints(values: list[str]) -> list[int]:
    return [int(v) for v in values if v.isdigit() and len(v) < 10]


def _first(form: Form, name: str, default: str = "") -> str:
    values = form.get(name)
    return values[0] if values else default


def request_lang(request: Request) -> str:
    return i18n.pick_lang(
        request.query_params.get("lang"),
        request.cookies.get(LANG_COOKIE),
        request.headers.get("accept-language"),
    )


def create_app(settings: Settings, client: ImmichClient | None = None) -> FastAPI:
    owns_client = client is None
    immich = client or ImmichClient(
        settings.immich_url,
        settings.immich_api_key,
        timeout=settings.immich_timeout,
        retries=settings.immich_retries,
    )
    with db.open_db(settings.db_path):
        pass  # create the schema at startup

    env = Environment(
        loader=PackageLoader("home_driver_curator", "templates"),
        autoescape=select_autoescape(default=True, default_for_string=True),
        trim_blocks=True,
        lstrip_blocks=True,
    )
    env.filters["iso"] = _iso
    env.filters["dt"] = _dt
    env.globals["icon"] = _icon

    app = FastAPI(title="home-driver curator", docs_url=None, redoc_url=None, openapi_url=None)
    app.state.settings = settings
    app.state.immich = immich
    if owns_client:
        app.router.on_shutdown.append(immich.close)

    def get_conn() -> Iterator[sqlite3.Connection]:
        conn = db.connect(settings.db_path)
        try:
            yield conn
        finally:
            conn.close()

    guard = [Depends(require_same_origin)]

    @app.middleware("http")
    async def security_headers(request: Request, call_next: Any) -> Response:
        response: Response = await call_next(request)
        for name, value in SECURITY_HEADERS.items():
            response.headers.setdefault(name, value)
        return response

    # -- rendering ---------------------------------------------------------------------

    def render(
        request: Request, name: str, user: str, status_code: int = 200, **ctx: Any
    ) -> HTMLResponse:
        lang = request_lang(request)
        t = i18n.translator(lang)
        # Relative link to "this page in another language", without one-shot flash params.
        here = request.url.path.rsplit("/", 1)[-1] or "./"
        addressable = status_code == 200 and request.method == "GET"
        if not addressable:
            here = "./"  # error pages and POST results have no address worth returning to
        keep_q = [
            (k, v)
            for k, v in request.query_params.multi_items()
            if k != "lang" and k not in FLASH_PARAMS and addressable
        ]
        lang_links = {code: f"{here}?{urlencode([*keep_q, ('lang', code)])}" for code in i18n.LANGS}
        html = env.get_template(name).render(
            t=t,
            lang=lang,
            lang_links=lang_links,
            user=user,
            dry_run=settings.dry_run,
            immich_public_url=settings.immich_public_url,
            js_strings={k: t(k) for k in i18n.JS_KEYS},
            **ctx,
        )
        response = HTMLResponse(
            html, status_code=status_code, headers={"Cache-Control": "no-store"}
        )
        if request.query_params.get("lang") in i18n.LANGS:
            # No Path attribute: the browser scopes it to the directory it sees (/curator/).
            response.set_cookie(
                LANG_COOKIE, lang, max_age=365 * 24 * 3600, samesite="lax", httponly=True, path=""
            )
        return response

    def error_page(request: Request, status_code: int) -> Response:
        if request.url.path.startswith(PLAIN_PATHS):
            return PlainTextResponse("error\n", status_code=status_code)
        key = {403: "err_forbidden", 404: "err_notfound"}.get(status_code, "err_generic")
        user = request.headers.get(USER_HEADER, "").strip()[:254]
        return render(request, "error.html", user, status_code=status_code, error_key=key)

    @app.exception_handler(StarletteHTTPException)
    async def http_error(request: Request, exc: StarletteHTTPException) -> Response:
        return error_page(request, exc.status_code)

    @app.exception_handler(RequestValidationError)
    async def validation_error(request: Request, exc: RequestValidationError) -> Response:
        return error_page(request, 400)

    def flash(conn: sqlite3.Connection, request: Request) -> dict[str, Any] | None:
        q = request.query_params
        key = q.get("m", "")
        if key not in i18n.MESSAGE_KEYS:
            return None
        undo_id = _int(q.get("u"))
        if undo_id:
            row = conn.execute(
                "SELECT 1 FROM activity WHERE id=? AND action='trash' AND undone=0 AND dry_run=0",
                (undo_id,),
            ).fetchone()
            undo_id = undo_id if row else 0
        return {"key": key, "n": _int(q.get("n")), "skipped": _int(q.get("sk")), "undo": undo_id}

    def back(form: Form, out: actions.Outcome | str) -> RedirectResponse:
        """Redirect (relative!) to the review screen the form came from, with a toast."""
        if isinstance(out, str):
            out = actions.Outcome(out)
        category = _first(form, "category")
        if category not in CATEGORY_LABELS:
            params: dict[str, Any] = {}
            target = "activity" if _first(form, "from") == "activity" else "./"
        else:
            target = "review"
            params = {
                "category": category,
                "mode": _first(form, "mode"),
                "i": _int(_first(form, "i")),
                "page": max(1, _int(_first(form, "page"), 1)),
            }
        params["m"] = out.key
        if out.n:
            params["n"] = out.n
        if out.skipped:
            params["sk"] = out.skipped
        if out.undo_id:
            params["u"] = out.undo_id
        return RedirectResponse(f"{target}?{urlencode(params)}", status_code=303)

    def immich_failure(exc: ImmichError) -> str:
        log.error("Immich call failed: %s", exc)
        if isinstance(exc, ImmichPermissionError | ImmichAuthError):
            return "msg_immich_denied"
        return "msg_immich_error"

    # -- pages -------------------------------------------------------------------------

    @app.get("/healthz")
    def healthz() -> PlainTextResponse:
        return PlainTextResponse("ok\n")

    @app.get("/static/{name}")
    def static(name: str) -> Response:
        if name not in STATIC_TYPES:
            raise HTTPException(status_code=404)
        data = resources.files("home_driver_curator").joinpath("static", name).read_bytes()
        return Response(data, media_type=STATIC_TYPES[name], headers={"Cache-Control": "no-cache"})

    @app.get("/")
    def dashboard(
        request: Request,
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
    ) -> HTMLResponse:
        counts = db.pending_counts(conn)
        enabled = {d.name: d.enabled(settings) for d in DETECTORS}
        enabled["burst"] = settings.detect_burst
        enabled["duplicate"] = settings.detect_duplicates
        cards = []
        for name in CATEGORY_LABELS:
            row = conn.execute(
                "SELECT asset_id FROM suggestions WHERE status='pending' AND category=? "
                "ORDER BY id LIMIT 1",
                (name,),
            ).fetchone()
            cards.append(
                {
                    "name": name,
                    "count": counts.get(name, 0),
                    "enabled": enabled[name],
                    "cover": row["asset_id"] if row else None,
                }
            )
        first = next((c["name"] for c in cards if c["count"]), None)
        return render(
            request,
            "dashboard.html",
            user,
            cards=cards,
            total=sum(counts.values()),
            first=first,
            scan=db.last_scan(conn),
            stats=db.library_stats(conn),
            flash=flash(conn, request),
        )

    def single_order(category: str) -> str:
        spec = next(d for d in DETECTORS if d.name == category)
        return "ASC" if spec.flag_when == "below" else "DESC"

    @app.get("/review")
    def review(
        request: Request,
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
    ) -> HTMLResponse:
        q = request.query_params
        category = q.get("category", "")
        if category not in CATEGORY_LABELS:
            raise HTTPException(status_code=404)
        total = db.pending_counts(conn).get(category, 0)
        ctx: dict[str, Any] = {
            "category": category,
            "total": total,
            "flash": flash(conn, request),
            "i": 0,
            "page": 1,
        }
        if total == 0:
            return render(request, "review_empty.html", user, mode="", **ctx)

        if category in GROUP_CATEGORIES:
            count = conn.execute(
                "SELECT COUNT(DISTINCT group_id) FROM suggestions "
                "WHERE status='pending' AND category=?",
                (category,),
            ).fetchone()[0]
            i = min(_int(q.get("i")), max(0, count - 1))
            group = conn.execute(
                "SELECT g.* FROM groups g WHERE g.id IN (SELECT group_id FROM suggestions "
                "WHERE status='pending' AND category=?) ORDER BY g.id LIMIT 1 OFFSET ?",
                (category, i),
            ).fetchone()
            if group is None:
                return render(request, "review_empty.html", user, mode="", **ctx)
            members = db.group_members(conn, group["id"])
            ctx.update(i=i, count=count, group=group, members=members, mode="one")
            return render(request, "review_group.html", user, **ctx)

        mode = q.get("mode") or DEFAULT_MODE.get(category, "one")
        mode = mode if mode in ("one", "grid") else "one"
        order = single_order(category)
        sql = (
            "SELECT s.id, s.asset_id, s.score, s.reason, a.file_name, a.taken_at_iso, a.camera "  # noqa: S608
            "FROM suggestions s JOIN assets a ON a.asset_id = s.asset_id "
            f"WHERE s.status='pending' AND s.category=? ORDER BY s.score {order}, s.id "
            "LIMIT ? OFFSET ?"
        )
        if mode == "grid":
            pages = max(1, -(-total // GRID_PAGE_SIZE))
            page = min(max(1, _int(q.get("page"), 1)), pages)
            items = conn.execute(
                sql, (category, GRID_PAGE_SIZE, (page - 1) * GRID_PAGE_SIZE)
            ).fetchall()
            ctx.update(mode=mode, page=page, pages=pages, items=items)
            return render(request, "review_grid.html", user, **ctx)
        i = min(_int(q.get("i")), total - 1)
        rows = conn.execute(sql, (category, 2, i)).fetchall()
        ctx.update(mode=mode, i=i, item=rows[0], upcoming=rows[1] if len(rows) > 1 else None)
        return render(request, "review_one.html", user, **ctx)

    @app.get("/activity")
    def activity(
        request: Request,
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
    ) -> HTMLResponse:
        total = conn.execute("SELECT COUNT(*) FROM activity").fetchone()[0]
        pages = max(1, -(-total // ACTIVITY_PAGE_SIZE))
        page = min(max(1, _int(request.query_params.get("page"), 1)), pages)
        rows = conn.execute(
            "SELECT * FROM activity ORDER BY id DESC LIMIT ? OFFSET ?",
            (ACTIVITY_PAGE_SIZE, (page - 1) * ACTIVITY_PAGE_SIZE),
        ).fetchall()
        entries = [{"row": r, "assets": json.loads(r["assets"] or "[]")} for r in rows]
        return render(
            request,
            "activity.html",
            user,
            entries=entries,
            page=page,
            pages=pages,
            flash=flash(conn, request),
        )

    @app.get("/thumb")
    def thumb(
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        id: str = "",
        size: str = "thumbnail",
    ) -> Response:
        if not is_uuid(id) or size not in ("thumbnail", "preview"):
            raise HTTPException(status_code=400)
        known = conn.execute("SELECT 1 FROM assets WHERE asset_id=?", (id,)).fetchone()
        if known is None:
            raise HTTPException(status_code=404)
        try:
            data, ctype = immich.get_thumbnail(id, size)
        except ImmichNotFound as exc:
            raise HTTPException(status_code=404) from exc
        except ImmichError as exc:
            log.warning("thumbnail proxy failed: %s", exc)
            raise HTTPException(status_code=502) from exc
        ctype = ctype.split(";")[0].strip().lower()
        if not ctype.startswith("image/") or ctype == "image/svg+xml":
            ctype = "application/octet-stream"
        return Response(data, media_type=ctype, headers={"Cache-Control": "private, max-age=86400"})

    # -- actions (POST only, header + same-origin required) ----------------------------

    def chosen(conn: sqlite3.Connection, form: Form) -> tuple[list[int], bool]:
        """Suggestion ids named by the form, and whether they came from a bulk selection."""
        if form.get("one"):
            return _ints(form["one"][:1]), False
        if form.get("group"):
            gids = _ints(form["group"][:1])
            if not gids:
                return [], False
            rows = conn.execute(
                "SELECT id FROM suggestions WHERE group_id=? AND status='pending' ORDER BY id",
                (gids[0],),
            ).fetchall()
            return [r["id"] for r in rows], False
        return _ints(form.get("sel", []))[: actions.MAX_BULK], True

    @app.post("/keep", dependencies=guard)
    def keep(
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        form: Form = Depends(read_form),
    ) -> Response:
        ids, _ = chosen(conn, form)
        if not ids:
            return back(form, "msg_nothing")
        return back(form, actions.keep(conn, ids, user))

    @app.post("/trash", dependencies=guard)
    def trash(
        request: Request,
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        form: Form = Depends(read_form),
    ) -> Response:
        ids, bulk = chosen(conn, form)
        rows = [r for r in db.suggestions_by_ids(conn, ids) if r["status"] == "pending"]
        if not rows:
            return back(form, "msg_nothing" if not ids else "msg_stale")
        # A bulk selection is confirmed once, showing the count. Single photos and burst
        # groups go straight to the (recoverable) trash and offer Undo instead.
        if bulk and len(rows) > 1 and _first(form, "confirm") != "yes":
            return render(
                request,
                "confirm.html",
                user,
                category=_first(form, "category"),
                mode=_first(form, "mode"),
                i=_int(_first(form, "i")),
                page=max(1, _int(_first(form, "page"), 1)),
                rows=rows,
                count=len(rows),
            )
        try:
            out = actions.trash(conn, immich, settings, [r["id"] for r in rows], user)
        except ImmichError as exc:
            return back(form, immich_failure(exc))
        return back(form, out)

    @app.post("/undo", dependencies=guard)
    def undo(
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        form: Form = Depends(read_form),
    ) -> Response:
        try:
            out = actions.undo_trash(conn, immich, settings, _int(_first(form, "activity")), user)
        except ImmichError as exc:
            return back(form, immich_failure(exc))
        return back(form, out)

    @app.post("/stack", dependencies=guard)
    def stack(
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        form: Form = Depends(read_form),
    ) -> Response:
        gids = _ints(form.get("group", [])[:1])
        if not gids:
            return back(form, "msg_nothing")
        try:
            out = actions.stack(conn, immich, settings, gids[0], user)
        except ImmichError as exc:
            return back(form, immich_failure(exc))
        return back(form, out)

    @app.post("/keeper", dependencies=guard)
    def keeper(
        user: str = Depends(require_user),
        conn: sqlite3.Connection = Depends(get_conn),
        form: Form = Depends(read_form),
    ) -> Response:
        gid, _, asset_id = _first(form, "choose").partition(":")
        if not gid.isdigit() or not is_uuid(asset_id):
            return back(form, "msg_nothing")
        return back(form, actions.set_keeper(conn, int(gid), asset_id, user))

    return app
