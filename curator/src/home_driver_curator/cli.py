"""Console entry point: ``home-driver-curator serve | scan | doctor``."""

from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import sys
import threading
from collections.abc import Sequence

from . import db
from .config import ConfigError, Settings
from .detectors import DETECTORS
from .immich import CHECKED_IMMICH_VERSION, REQUIRED_PERMISSIONS, ImmichClient, ImmichError
from .scan import ScanAlreadyRunning, Scanner

log = logging.getLogger("home_driver_curator")


def _client(settings: Settings) -> ImmichClient:
    return ImmichClient(
        settings.immich_url,
        settings.immich_api_key,
        timeout=settings.immich_timeout,
        retries=settings.immich_retries,
    )


def cmd_serve(settings: Settings, args: argparse.Namespace) -> int:
    import uvicorn

    from .web import create_app

    settings.require_api_key()
    app = create_app(settings)
    log.info(
        "curator listening on %s:%d (dry-run: %s)", settings.host, settings.port, settings.dry_run
    )
    uvicorn.run(
        app,
        host=settings.host,
        port=settings.port,
        log_level="info",
        proxy_headers=False,
        server_header=False,
    )
    return 0


def cmd_scan(settings: Settings, args: argparse.Namespace) -> int:
    settings.require_api_key()
    stop = threading.Event()

    def on_signal(signum: int, frame: object) -> None:
        log.info("signal %d received: finishing the current page, then stopping", signum)
        stop.set()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)
    with _client(settings) as client:
        try:
            result = Scanner(settings, client, stop).run(
                suggest=not args.no_suggest, limit=args.limit
            )
        except ScanAlreadyRunning:
            log.info("another scan is already running; nothing to do")
            return 0
    print(
        f"scan {result.status}: listed={result.listed} analysed={result.analysed} "
        f"unchanged={result.skipped} errors={result.errors} remaining={result.remaining} "
        f"new_suggestions={result.new_suggestions}"
    )
    if result.message or result.warnings:
        print("; ".join([m for m in [result.message, *result.warnings] if m]))
    return 0 if result.status in ("completed", "interrupted") else 1


def _percentiles(values: list[float]) -> str:
    values = sorted(values)
    if not values:
        return "(no data)"

    def at(p: float) -> float:
        return values[min(len(values) - 1, int(p / 100 * len(values)))]

    return "  ".join(f"p{p}={at(p):.3g}" for p in (1, 5, 25, 50, 75, 95, 99))


def report(settings: Settings) -> None:
    """Print score distributions from the analysis cache. Creates no suggestions."""
    with db.open_db(settings.db_path) as conn:
        rows = conn.execute(
            "SELECT asset_id, file_name, scores FROM assets WHERE scores IS NOT NULL AND present=1"
        ).fetchall()
        print(f"\nScore report over {len(rows)} analysed photos (nothing is changed)")
        for det in DETECTORS:
            scored: list[tuple[float, str]] = []
            vetoed = 0
            for row in rows:
                finding = json.loads(row["scores"]).get(det.name)
                if not finding:
                    continue
                if finding.get("veto"):
                    vetoed += 1
                else:
                    scored.append((float(finding["score"]), row["file_name"]))
            limit = det.threshold(settings)
            flagged = [s for s in scored if det.flags(s[0], False, settings)]
            env = "CURATOR_" + det.threshold_attr.upper()
            print(f"\n[{det.name}] flags when score is {det.flag_when} {limit:g}  ({env})")
            print(
                f"  judged={len(scored)}  not judged (guards)={vetoed}  would flag={len(flagged)}"
            )
            print("  " + _percentiles([s[0] for s in scored]))
            worst = sorted(scored, reverse=det.flag_when == "above")[:8]
            for score, name in worst:
                mark = "FLAG" if det.flags(score, False, settings) else "    "
                print(f"    {mark} {score:8.3g}  {name}")
        pending = db.pending_counts(conn)
        print(f"\nCurrently pending suggestions: {dict(pending) or 'none'}")


def cmd_doctor(settings: Settings, args: argparse.Namespace) -> int:
    ok = True

    def check(label: str, passed: bool, detail: str = "") -> None:
        nonlocal ok
        ok = ok and passed
        print(f"[{'ok' if passed else 'FAIL'}] {label}{': ' + detail if detail else ''}")

    check("listen address is loopback", True, f"{settings.host}:{settings.port}")
    try:
        with db.open_db(settings.db_path):
            pass
        check("database writable", True, settings.db_path)
    except OSError as exc:
        check("database writable", False, f"{settings.db_path}: {exc}")
    check("CURATOR_IMMICH_API_KEY set", bool(settings.immich_api_key))
    print(f"[info] dry-run: {'ON (nothing is changed in Immich)' if settings.dry_run else 'off'}")
    script = settings.notify_script
    print(
        f"[info] notify script: {script} "
        f"({'found' if os.path.isfile(script) else 'not found, skipped'})"
    )
    if settings.immich_api_key:
        with _client(settings) as client:
            try:
                version = client.server_version()
                check(
                    "Immich reachable",
                    version.supported,
                    f"v{version} at {settings.immich_url} (client checked against "
                    f"v{CHECKED_IMMICH_VERSION}; needs >= 3.2.0)",
                )
                missing = client.missing_permissions()
                check(
                    "API key permissions",
                    not missing,
                    "missing: " + ", ".join(missing)
                    if missing
                    else ", ".join(REQUIRED_PERMISSIONS),
                )
                if args.sample:
                    result = Scanner(settings, client).run(suggest=False, limit=args.sample)
                    print(
                        f"[info] sampled: analysed={result.analysed} errors={result.errors} "
                        f"status={result.status} {result.message}"
                    )
            except ScanAlreadyRunning:
                print("[info] a scan is running; sample skipped")
            except ImmichError as exc:
                check("Immich reachable", False, str(exc))
    if args.report or args.sample:
        report(settings)
    return 0 if ok else 1


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="home-driver-curator",
        description="Suggests junk photos in Immich for human review. Never deletes anything.",
    )
    parser.add_argument("-v", "--verbose", action="store_true")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("serve", help="run the review UI (loopback only)")
    p_scan = sub.add_parser("scan", help="analyse new/changed photos and refresh suggestions")
    p_scan.add_argument("--limit", type=int, default=None, help="max photos to analyse (0 = all)")
    p_scan.add_argument(
        "--no-suggest", action="store_true", help="analyse only; do not touch the review queue"
    )
    p_doc = sub.add_parser("doctor", help="check configuration, Immich access and permissions")
    p_doc.add_argument(
        "--report", action="store_true", help="print detector score distributions (read-only)"
    )
    p_doc.add_argument(
        "--sample",
        type=int,
        default=0,
        metavar="N",
        help="first analyse up to N not-yet-analysed photos (no suggestions), then report",
    )
    args = parser.parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)
    try:
        settings = Settings.from_env()
        handler = {"serve": cmd_serve, "scan": cmd_scan, "doctor": cmd_doctor}[args.command]
        return handler(settings, args)
    except ConfigError as exc:
        print(f"configuration error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
