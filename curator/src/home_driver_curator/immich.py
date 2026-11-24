"""The ONLY module that talks to Immich. A version bump should touch this file alone.

Checked against the Immich OpenAPI spec at release tag v3.4.2
(open-api/immich-openapi-specs.json). Endpoints used, with the API-key permission each
needs (from the spec's ``x-immich-permission``):

    GET    /api/server/version                    (none)
    GET    /api/api-keys/me                       (none)            -> key's permission list
    POST   /api/search/metadata                   asset.read        cursor pagination (v3.2.0+)
    GET    /api/assets/{id}                       asset.read
    GET    /api/assets/{id}/thumbnail?size=...    asset.view
    DELETE /api/assets   {"ids": [...], "force": false}   asset.delete   -> moves to trash
    POST   /api/trash/restore/assets {"ids": [...]}       asset.delete   -> undo of the above
    GET    /api/duplicates                        duplicate.read
    POST   /api/stacks   {"assetIds": [primary, ...]}     stack.create   first id = primary

Hard rule: this client can only ever *trash* assets. ``trash_assets`` always sends
``"force": false`` and there is deliberately no way to pass ``true``.
"""

from __future__ import annotations

import logging
import re
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import httpx

log = logging.getLogger(__name__)

CHECKED_IMMICH_VERSION = "3.4.2"
# Cursor pagination and the nested search `filter` were added in v3.2.0.
MIN_IMMICH_VERSION = (3, 2, 0)

REQUIRED_PERMISSIONS: dict[str, str] = {
    "asset.read": "list assets and read their metadata",
    "asset.view": "download thumbnails/previews for analysis and the review page",
    "asset.delete": "move approved assets to the Immich trash (and restore them on Undo)",
    "duplicate.read": "show Immich's own duplicate groups",
    "stack.create": "stack a burst instead of trashing it",
}

_UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$")
_RETRY_STATUS = {429, 502, 503, 504}
THUMBNAIL_SIZES = ("thumbnail", "preview")


class ImmichError(Exception):
    """Base class for every failure talking to Immich."""


class ImmichUnavailable(ImmichError):
    """Network failure or 5xx after all retries."""


class ImmichAuthError(ImmichError):
    """401: the API key is missing, wrong or revoked."""


class ImmichPermissionError(ImmichError):
    """403: the API key exists but lacks a permission."""

    def __init__(self, permission: str, purpose: str, server_message: str = "") -> None:
        self.permission = permission
        msg = (
            f"The Immich API key lacks the permission '{permission}', needed to {purpose}. "
            "Edit the key in Immich (Account Settings > API Keys) and enable it."
        )
        if server_message:
            msg += f" (Immich said: {server_message})"
        super().__init__(msg)


class ImmichNotFound(ImmichError):
    """404 for a specific asset."""


class ImmichBadRequest(ImmichError):
    """Other 4xx: we sent something this Immich version does not accept."""


def is_uuid(value: str) -> bool:
    return bool(_UUID_RE.match(value))


def _parse_time(value: str | None) -> float | None:
    if not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


@dataclass(frozen=True)
class Asset:
    """The subset of AssetResponseDto (+ ExifResponseDto) the curator uses."""

    id: str
    owner_id: str
    type: str  # IMAGE | VIDEO | AUDIO | OTHER
    file_name: str
    mime: str
    checksum: str
    updated_at: str
    taken_at: float | None  # epoch seconds, from fileCreatedAt
    taken_at_iso: str
    width: int | None
    height: int | None
    is_favorite: bool
    is_trashed: bool
    visibility: str  # timeline | archive | hidden | locked
    make: str
    model: str
    file_size: int | None
    duplicate_id: str | None

    @property
    def is_archived(self) -> bool:
        return self.visibility != "timeline"

    @property
    def camera(self) -> str:
        return f"{self.make} {self.model}".strip()

    @classmethod
    def from_api(cls, data: dict[str, Any]) -> Asset:
        exif = data.get("exifInfo") or {}
        visibility = data.get("visibility") or ("archive" if data.get("isArchived") else "timeline")
        if data.get("isArchived") and visibility == "timeline":
            visibility = "archive"
        return cls(
            id=str(data["id"]),
            owner_id=str(data.get("ownerId") or ""),
            type=str(data.get("type") or "OTHER"),
            file_name=str(data.get("originalFileName") or ""),
            mime=str(data.get("originalMimeType") or ""),
            checksum=str(data.get("checksum") or ""),
            updated_at=str(data.get("updatedAt") or ""),
            taken_at=_parse_time(data.get("fileCreatedAt")),
            taken_at_iso=str(data.get("localDateTime") or data.get("fileCreatedAt") or ""),
            width=data.get("width") or exif.get("exifImageWidth"),
            height=data.get("height") or exif.get("exifImageHeight"),
            is_favorite=bool(data.get("isFavorite")),
            is_trashed=bool(data.get("isTrashed")),
            visibility=str(visibility),
            make=str(exif.get("make") or "").strip(),
            model=str(exif.get("model") or "").strip(),
            file_size=exif.get("fileSizeInByte"),
            duplicate_id=data.get("duplicateId"),
        )


@dataclass(frozen=True)
class DuplicateGroup:
    duplicate_id: str
    assets: list[Asset]
    suggested_keep_ids: list[str]


@dataclass(frozen=True)
class ServerVersion:
    major: int
    minor: int
    patch: int

    def __str__(self) -> str:
        return f"{self.major}.{self.minor}.{self.patch}"

    @property
    def supported(self) -> bool:
        return (self.major, self.minor, self.patch) >= MIN_IMMICH_VERSION


class ImmichClient:
    def __init__(
        self,
        base_url: str,
        api_key: str,
        *,
        timeout: float = 30.0,
        retries: int = 3,
        backoff: float = 0.5,
        http: httpx.Client | None = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._base = base_url.rstrip("/") + "/api"
        self._headers = {"x-api-key": api_key, "Accept": "application/json"}
        self._retries = max(0, retries)
        self._backoff = backoff
        self._sleep = sleep
        self._owns_http = http is None
        self._http = http or httpx.Client(
            timeout=httpx.Timeout(timeout, connect=min(timeout, 10.0)),
            limits=httpx.Limits(max_connections=8, max_keepalive_connections=4),
        )

    def close(self) -> None:
        if self._owns_http:
            self._http.close()

    def __enter__(self) -> ImmichClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # -- transport -----------------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        *,
        permission: str | None,
        params: dict[str, Any] | None = None,
        json: Any = None,
        retry: bool = True,
        accept: str | None = None,
    ) -> httpx.Response:
        url = self._base + path
        headers = dict(self._headers)
        if accept:
            headers["Accept"] = accept
        attempts = (self._retries if retry else 0) + 1
        last: str = ""
        for attempt in range(attempts):
            if attempt:
                self._sleep(self._backoff * (2 ** (attempt - 1)))
            try:
                resp = self._http.request(method, url, params=params, json=json, headers=headers)
            except httpx.HTTPError as exc:
                last = f"{type(exc).__name__}: {exc}"
                log.warning("immich %s %s failed (%s), attempt %d", method, path, last, attempt + 1)
                continue
            if resp.status_code in _RETRY_STATUS or resp.status_code >= 500:
                last = f"HTTP {resp.status_code}"
                log.warning("immich %s %s -> %s, attempt %d", method, path, last, attempt + 1)
                continue
            self._raise_for_status(resp, method, path, permission)
            return resp
        raise ImmichUnavailable(
            f"Immich did not answer {method} {path} after {attempts} attempt(s): {last}"
        )

    @staticmethod
    def _server_message(resp: httpx.Response) -> str:
        try:
            body = resp.json()
        except ValueError:
            return resp.text[:200]
        if isinstance(body, dict):
            msg = body.get("message")
            if isinstance(msg, list):
                return "; ".join(str(m) for m in msg)[:300]
            if msg:
                return str(msg)[:300]
        return ""

    def _raise_for_status(
        self, resp: httpx.Response, method: str, path: str, permission: str | None
    ) -> None:
        code = resp.status_code
        if code < 400:
            return
        message = self._server_message(resp)
        if code == 401:
            raise ImmichAuthError(
                "Immich rejected the API key (401). Check CURATOR_IMMICH_API_KEY. " + message
            )
        if code == 403:
            perm = permission or "unknown"
            purpose = REQUIRED_PERMISSIONS.get(perm, f"call {method} {path}")
            raise ImmichPermissionError(perm, purpose, message)
        # Immich answers an unknown/inaccessible id with 400 "Not found or no <perm> access".
        if code == 404 or (code == 400 and message.lower().startswith("not found")):
            raise ImmichNotFound(f"{method} {path}: not found. {message}")
        raise ImmichBadRequest(
            f"Immich refused {method} {path} with HTTP {code}: {message}. The Immich API may "
            f"have changed; this client was checked against v{CHECKED_IMMICH_VERSION}."
        )

    # -- calls ---------------------------------------------------------------------

    def server_version(self) -> ServerVersion:
        data = self._request("GET", "/server/version", permission=None).json()
        return ServerVersion(int(data["major"]), int(data["minor"]), int(data["patch"]))

    def api_key_permissions(self) -> list[str]:
        data = self._request("GET", "/api-keys/me", permission=None).json()
        return [str(p) for p in data.get("permissions", [])]

    def missing_permissions(self) -> list[str]:
        granted = set(self.api_key_permissions())
        if "all" in granted:
            return []
        return [p for p in REQUIRED_PERMISSIONS if p not in granted]

    def iter_image_pages(self, page_size: int = 250) -> Iterator[list[Asset]]:
        """Yield pages of IMAGE assets, oldest first, following ``nextCursor`` to the end."""
        cursor: str | None = None
        seen_cursors: set[str] = set()
        while True:
            body: dict[str, Any] = {
                "size": page_size,
                "withExif": True,
                "filter": {"type": {"eq": "IMAGE"}},
                "orderBy": {"field": "fileCreatedAt", "direction": "asc"},
            }
            if cursor:
                body["cursor"] = cursor
            data = self._request(
                "POST", "/search/metadata", permission="asset.read", json=body
            ).json()
            assets = data["assets"]
            yield [Asset.from_api(item) for item in assets.get("items", [])]
            cursor = assets.get("nextCursor")
            if not cursor:
                return
            if cursor in seen_cursors:
                raise ImmichError("Immich returned the same search cursor twice; aborting.")
            seen_cursors.add(cursor)

    def get_asset(self, asset_id: str) -> Asset:
        self._check_id(asset_id)
        data = self._request("GET", f"/assets/{asset_id}", permission="asset.read").json()
        return Asset.from_api(data)

    def get_thumbnail(self, asset_id: str, size: str = "preview") -> tuple[bytes, str]:
        """Return (bytes, content-type) of the server-generated thumbnail or preview."""
        self._check_id(asset_id)
        if size not in THUMBNAIL_SIZES:
            raise ValueError(f"unsupported thumbnail size {size!r}")
        resp = self._request(
            "GET",
            f"/assets/{asset_id}/thumbnail",
            permission="asset.view",
            params={"size": size},
            accept="*/*",
        )
        return resp.content, resp.headers.get("content-type", "application/octet-stream")

    def trash_assets(self, asset_ids: list[str]) -> None:
        """Move assets to the Immich trash (recoverable). Never force-deletes."""
        if not asset_ids:
            return
        for asset_id in asset_ids:
            self._check_id(asset_id)
        self._request(
            "DELETE",
            "/assets",
            permission="asset.delete",
            json={"ids": list(asset_ids), "force": False},
        )

    def restore_assets(self, asset_ids: list[str]) -> int:
        """Bring assets back from the Immich trash (the Undo of ``trash_assets``)."""
        if not asset_ids:
            return 0
        for asset_id in asset_ids:
            self._check_id(asset_id)
        resp = self._request(
            "POST",
            "/trash/restore/assets",
            permission="asset.delete",
            json={"ids": list(asset_ids)},
        )
        return int(resp.json().get("count", 0))

    def get_duplicates(self) -> list[DuplicateGroup]:
        data = self._request("GET", "/duplicates", permission="duplicate.read").json()
        return [
            DuplicateGroup(
                duplicate_id=str(group["duplicateId"]),
                assets=[Asset.from_api(a) for a in group.get("assets", [])],
                suggested_keep_ids=[str(i) for i in group.get("suggestedKeepAssetIds", [])],
            )
            for group in data
        ]

    def create_stack(self, primary_id: str, other_ids: list[str]) -> str:
        """Create a stack; the first id in ``assetIds`` becomes the primary. Returns stack id."""
        ids = [primary_id, *[i for i in other_ids if i != primary_id]]
        if len(ids) < 2:
            raise ValueError("a stack needs at least two assets")
        for asset_id in ids:
            self._check_id(asset_id)
        # Not retried: POST /stacks is not idempotent.
        resp = self._request(
            "POST", "/stacks", permission="stack.create", json={"assetIds": ids}, retry=False
        )
        return str(resp.json().get("id", ""))

    @staticmethod
    def _check_id(asset_id: str) -> None:
        if not is_uuid(asset_id):
            raise ValueError(f"not an Immich asset id: {asset_id!r}")
