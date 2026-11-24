"""Configuration from CURATOR_* environment variables. See curator/CLAUDE.md for the table."""

from __future__ import annotations

import ipaddress
import os
from collections.abc import Mapping
from dataclasses import dataclass, field

DEFAULT_DB_PATH = "/var/lib/home-driver/curator/curator.db"
DEFAULT_NOTIFY_SCRIPT = "/opt/home-driver/scripts/notify.sh"


class ConfigError(Exception):
    """Invalid configuration; the message is safe to show to the owner."""


def _bool(env: Mapping[str, str], name: str, default: bool) -> bool:
    raw = env.get(name, "").strip().lower()
    if raw == "":
        return default
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    raise ConfigError(f"{name} must be on/off (got {raw!r})")


def _int(env: Mapping[str, str], name: str, default: int, minimum: int = 0) -> int:
    raw = env.get(name, "").strip()
    if raw == "":
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name} must be a whole number (got {raw!r})") from exc
    if value < minimum:
        raise ConfigError(f"{name} must be >= {minimum} (got {value})")
    return value


def _float(env: Mapping[str, str], name: str, default: float) -> float:
    raw = env.get(name, "").strip()
    if raw == "":
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise ConfigError(f"{name} must be a number (got {raw!r})") from exc


def parse_addr(addr: str) -> tuple[str, int]:
    """Split host:port (IPv6 as [::1]:8091) and refuse anything that is not loopback."""
    host, sep, port_s = addr.strip().rpartition(":")
    if not sep or not host:
        raise ConfigError(f"CURATOR_ADDR must look like 127.0.0.1:8091 (got {addr!r})")
    host = host.strip("[]")
    try:
        port = int(port_s)
    except ValueError as exc:
        raise ConfigError(f"CURATOR_ADDR has an invalid port: {addr!r}") from exc
    if not 0 < port < 65536:
        raise ConfigError(f"CURATOR_ADDR has an invalid port: {addr!r}")
    if not is_loopback(host):
        raise ConfigError(
            f"CURATOR_ADDR host {host!r} is not a loopback address. The curator has no "
            "authentication of its own and must only listen on 127.0.0.1 / ::1."
        )
    return host, port


def is_loopback(host: str) -> bool:
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


@dataclass(frozen=True)
class Settings:
    host: str = "127.0.0.1"
    port: int = 8091
    db_path: str = DEFAULT_DB_PATH
    immich_url: str = "http://127.0.0.1:2283"
    immich_api_key: str = field(default="", repr=False)
    immich_public_url: str = ""
    immich_timeout: float = 30.0
    immich_retries: int = 3
    dry_run: bool = False

    scan_batch: int = 5000
    scan_page_size: int = 250
    scan_concurrency: int = 2
    scan_sleep_ms: int = 0
    notify_script: str = DEFAULT_NOTIFY_SCRIPT

    detect_blurry: bool = True
    blur_threshold: float = 0.12
    detect_dark: bool = True
    dark_threshold: float = 12.0
    detect_screenshot: bool = True
    screenshot_threshold: float = 0.6
    detect_burst: bool = True
    burst_window_seconds: float = 3.0
    burst_hamming: int = 6
    detect_duplicates: bool = True

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        env = os.environ if env is None else env
        d = cls()
        host, port = parse_addr(env.get("CURATOR_ADDR", "").strip() or f"{d.host}:{d.port}")
        immich_url = (env.get("CURATOR_IMMICH_URL", "").strip() or d.immich_url).rstrip("/")
        if not immich_url.startswith(("http://", "https://")):
            raise ConfigError("CURATOR_IMMICH_URL must start with http:// or https://")
        public = env.get("CURATOR_IMMICH_PUBLIC_URL", "").strip().rstrip("/")
        if public and not public.startswith(("http://", "https://")):
            raise ConfigError("CURATOR_IMMICH_PUBLIC_URL must start with http:// or https://")
        page_size = _int(env, "CURATOR_SCAN_PAGE_SIZE", d.scan_page_size, 1)
        if page_size > 1000:
            raise ConfigError("CURATOR_SCAN_PAGE_SIZE must be <= 1000 (Immich API limit)")
        return cls(
            host=host,
            port=port,
            db_path=env.get("CURATOR_DB_PATH", "").strip() or d.db_path,
            immich_url=immich_url,
            immich_api_key=env.get("CURATOR_IMMICH_API_KEY", "").strip(),
            immich_public_url=public,
            immich_timeout=_float(env, "CURATOR_IMMICH_TIMEOUT", d.immich_timeout),
            immich_retries=_int(env, "CURATOR_IMMICH_RETRIES", d.immich_retries),
            dry_run=_bool(env, "CURATOR_DRY_RUN", d.dry_run),
            scan_batch=_int(env, "CURATOR_SCAN_BATCH", d.scan_batch),
            scan_page_size=page_size,
            scan_concurrency=_int(env, "CURATOR_SCAN_CONCURRENCY", d.scan_concurrency, 1),
            scan_sleep_ms=_int(env, "CURATOR_SCAN_SLEEP_MS", d.scan_sleep_ms),
            notify_script=env.get("CURATOR_NOTIFY_SCRIPT", "").strip() or d.notify_script,
            detect_blurry=_bool(env, "CURATOR_DETECT_BLURRY", d.detect_blurry),
            blur_threshold=_float(env, "CURATOR_BLUR_THRESHOLD", d.blur_threshold),
            detect_dark=_bool(env, "CURATOR_DETECT_DARK", d.detect_dark),
            dark_threshold=_float(env, "CURATOR_DARK_THRESHOLD", d.dark_threshold),
            detect_screenshot=_bool(env, "CURATOR_DETECT_SCREENSHOT", d.detect_screenshot),
            screenshot_threshold=_float(
                env, "CURATOR_SCREENSHOT_THRESHOLD", d.screenshot_threshold
            ),
            detect_burst=_bool(env, "CURATOR_DETECT_BURST", d.detect_burst),
            burst_window_seconds=_float(
                env, "CURATOR_BURST_WINDOW_SECONDS", d.burst_window_seconds
            ),
            burst_hamming=_int(env, "CURATOR_BURST_HAMMING", d.burst_hamming),
            detect_duplicates=_bool(env, "CURATOR_DETECT_DUPLICATES", d.detect_duplicates),
        )

    def require_api_key(self) -> None:
        if not self.immich_api_key:
            raise ConfigError(
                "CURATOR_IMMICH_API_KEY is not set. Create an API key in Immich "
                "(Account Settings > API Keys) and put it in /etc/home-driver/curator.env."
            )
