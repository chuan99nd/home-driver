"""A fake Immich server implementing exactly the endpoints the curator uses.

Shapes follow the OpenAPI spec at release tag v3.4.2 (see curator/CLAUDE.md). Request bodies
are validated strictly (unknown fields are rejected), so a client that drifts from the spec
fails here first. Every request is recorded in ``state.requests`` for assertions.

Run it for manual testing (sample photos included):

    uv run python -m tests.fake_immich --port 2299 --demo
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import io
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any, Literal

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response
from PIL import Image
from pydantic import BaseModel, ConfigDict, Field

from . import images

API_KEY = "test-key"
OWNER = "11111111-1111-4111-8111-111111111111"
ALL_PERMISSIONS = ["asset.read", "asset.view", "asset.delete", "duplicate.read", "stack.create"]
Uuid = Field(pattern=r"^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$")


class Strict(BaseModel):
    model_config = ConfigDict(extra="forbid")


class EnumFilterAssetType(Strict):
    eq: Literal["IMAGE", "VIDEO", "AUDIO", "OTHER"] | None = None


class SearchFilter(Strict):
    type: EnumFilterAssetType | None = None


class SearchOrder(Strict):
    direction: Literal["asc", "desc"] = "desc"
    field: Literal["fileCreatedAt", "localDateTime", "fileSizeInBytes", "rating"] = "fileCreatedAt"


class MetadataSearchDto(Strict):
    cursor: str | None = Field(default=None, min_length=1)
    filter: SearchFilter | None = None
    orderBy: SearchOrder | None = None
    size: int = Field(default=250, ge=1, le=1000)
    withExif: bool | None = None


class AssetBulkDeleteDto(Strict):
    ids: list[str]
    force: bool | None = None


class BulkIdsDto(Strict):
    ids: list[str]


class StackCreateDto(Strict):
    assetIds: list[str] = Field(min_length=2)


@dataclass
class FakeAsset:
    id: str
    name: str
    image: bytes  # JPEG served for both thumbnail sizes
    taken: datetime
    type: str = "IMAGE"
    mime: str = "image/jpeg"
    make: str | None = "Apple"
    model: str | None = "iPhone 13"
    width: int = 4032
    height: int = 3024
    favorite: bool = False
    visibility: str = "timeline"
    trashed: bool = False
    deleted_forever: bool = False
    updated: str = "2024-01-01T00:00:00.000Z"
    duplicate_id: str | None = None

    @property
    def checksum(self) -> str:
        return base64.b64encode(hashlib.sha1(self.image, usedforsecurity=False).digest()).decode()

    def to_api(self, with_exif: bool = True) -> dict[str, Any]:
        stamp = self.taken.strftime("%Y-%m-%dT%H:%M:%S.000Z")
        data: dict[str, Any] = {
            "id": self.id,
            "ownerId": OWNER,
            "type": self.type,
            "originalFileName": self.name,
            "originalMimeType": self.mime,
            "originalPath": f"/data/upload/{self.name}",
            "checksum": self.checksum,
            "createdAt": stamp,
            "updatedAt": self.updated,
            "fileCreatedAt": stamp,
            "fileModifiedAt": stamp,
            "localDateTime": stamp,
            "duration": None,
            "hasMetadata": True,
            "width": self.width,
            "height": self.height,
            "isArchived": self.visibility == "archive",
            "isEdited": False,
            "isFavorite": self.favorite,
            "isOffline": False,
            "isTrashed": self.trashed,
            "thumbhash": None,
            "visibility": self.visibility,
            "duplicateId": self.duplicate_id,
            "livePhotoVideoId": None,
        }
        if with_exif:
            data["exifInfo"] = {
                "make": self.make,
                "model": self.model,
                "exifImageWidth": self.width,
                "exifImageHeight": self.height,
                "fileSizeInByte": len(self.image) * 20,
                "dateTimeOriginal": stamp,
            }
        return data


@dataclass
class FakeState:
    assets: dict[str, FakeAsset] = field(default_factory=dict)
    permissions: list[str] = field(default_factory=lambda: ["all"])
    requests: list[dict[str, Any]] = field(default_factory=list)
    stacks: list[list[str]] = field(default_factory=list)
    duplicates: list[dict[str, Any]] = field(default_factory=list)
    version: tuple[int, int, int] = (3, 4, 2)
    fail_next: int = 0  # answer this many requests with 503 (retry tests)
    fail_thumbnails_after: int | None = None  # 503 for every thumbnail after N served
    thumbnails_served: int = 0

    def add(
        self,
        name: str,
        image: Image.Image | bytes,
        *,
        taken: datetime | None = None,
        **kwargs: Any,
    ) -> FakeAsset:
        data = image if isinstance(image, bytes) else images.to_jpeg(image)
        asset_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"fake-immich/{name}"))
        # uuid5 sets version 5; the spec pattern wants v4-looking ids.
        asset_id = asset_id[:14] + "4" + asset_id[15:19] + "8" + asset_id[20:]
        taken = taken or datetime(2024, 5, 1, 10, 0, 0, tzinfo=UTC) + timedelta(
            hours=len(self.assets)
        )
        asset = FakeAsset(id=asset_id, name=name, image=data, taken=taken, **kwargs)
        self.assets[asset_id] = asset
        return asset

    def calls(self, method: str, path_prefix: str) -> list[dict[str, Any]]:
        return [
            r for r in self.requests if r["method"] == method and r["path"].startswith(path_prefix)
        ]


def _error(status: int, message: str) -> JSONResponse:
    names = {400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found"}
    return JSONResponse(
        {"message": message, "error": names.get(status, "Error"), "statusCode": status},
        status_code=status,
    )


def create_fake(state: FakeState | None = None) -> FastAPI:
    state = state or FakeState()
    app = FastAPI()
    app.state.fake = state

    @app.middleware("http")
    async def record(request: Request, call_next: Any) -> Response:
        body = await request.body()
        entry: dict[str, Any] = {
            "method": request.method,
            "path": request.url.path,
            "query": dict(request.query_params),
            "json": None,
        }
        if body:
            try:
                import json

                entry["json"] = json.loads(body)
            except ValueError:
                entry["json"] = "<not json>"
        state.requests.append(entry)
        if state.fail_next > 0:
            state.fail_next -= 1
            return _error(503, "temporarily unavailable")
        return await call_next(request)

    def deny(request: Request, permission: str | None) -> JSONResponse | None:
        if request.headers.get("x-api-key") != API_KEY:
            return _error(401, "Invalid API key")
        if permission and "all" not in state.permissions and permission not in state.permissions:
            return _error(403, f"Missing required permission: {permission}")
        return None

    def visible(asset_id: str) -> FakeAsset | None:
        asset = state.assets.get(asset_id)
        return None if asset is None or asset.deleted_forever else asset

    @app.get("/api/server/version")
    def version() -> dict[str, Any]:
        major, minor, patch = state.version
        return {
            "major": major,
            "minor": minor,
            "patch": patch,
            "prerelease": None,
            "featureLevel": 0,
        }

    @app.get("/api/api-keys/me")
    def key_me(request: Request) -> Any:
        if err := deny(request, None):
            return err
        return {
            "id": "22222222-2222-4222-8222-222222222222",
            "name": "curator",
            "permissions": state.permissions,
            "createdAt": "2024-01-01T00:00:00.000Z",
            "updatedAt": "2024-01-01T00:00:00.000Z",
        }

    @app.post("/api/search/metadata")
    def search(request: Request, dto: MetadataSearchDto) -> Any:
        if err := deny(request, "asset.read"):
            return err
        wanted = dto.filter.type.eq if dto.filter and dto.filter.type else None
        pool = [
            a
            for a in state.assets.values()
            if not a.trashed
            and not a.deleted_forever
            and a.visibility in ("timeline", "archive")
            and (wanted is None or a.type == wanted)
        ]
        reverse = (dto.orderBy.direction if dto.orderBy else "desc") == "desc"
        pool.sort(key=lambda a: (a.taken, a.id), reverse=reverse)
        start = 0
        if dto.cursor:
            try:
                start = int(base64.urlsafe_b64decode(dto.cursor.encode()).decode())
            except ValueError:
                return _error(400, "Invalid cursor")
        items = pool[start : start + dto.size]
        more = start + dto.size < len(pool)
        cursor = base64.urlsafe_b64encode(str(start + dto.size).encode()).decode() if more else None
        return {
            "albums": {"total": 0, "count": 0, "items": [], "facets": []},
            "assets": {
                "total": len(items),
                "count": len(items),
                "items": [a.to_api(bool(dto.withExif)) for a in items],
                "facets": [],
                "nextPage": None,
                "nextCursor": cursor,
            },
        }

    @app.get("/api/assets/{asset_id}")
    def get_asset(request: Request, asset_id: str) -> Any:
        if err := deny(request, "asset.read"):
            return err
        asset = visible(asset_id)
        if asset is None:
            return _error(400, "Not found or no asset.read access")
        return asset.to_api()

    @app.get("/api/assets/{asset_id}/thumbnail")
    def thumbnail(request: Request, asset_id: str, size: str = "thumbnail") -> Any:
        if err := deny(request, "asset.view"):
            return err
        if size not in ("original", "fullsize", "preview", "thumbnail"):
            return _error(400, "size must be one of original, fullsize, preview, thumbnail")
        asset = visible(asset_id)
        if asset is None:
            return _error(404, "Asset not found")
        if (
            state.fail_thumbnails_after is not None
            and state.thumbnails_served >= state.fail_thumbnails_after
        ):
            return _error(503, "thumbnail generation unavailable")
        state.thumbnails_served += 1
        if size == "thumbnail":
            with Image.open(io.BytesIO(asset.image)) as img:
                img.thumbnail((250, 250))
                buf = io.BytesIO()
                img.convert("RGB").save(buf, "WEBP", quality=75)
            return Response(buf.getvalue(), media_type="image/webp")
        return Response(asset.image, media_type="image/jpeg")

    @app.delete("/api/assets", status_code=204, response_model=None)
    def delete_assets(request: Request, dto: AssetBulkDeleteDto) -> Any:
        if err := deny(request, "asset.delete"):
            return err
        for asset_id in dto.ids:
            asset = visible(asset_id)
            if asset is None:
                return _error(400, "Not found or no asset.delete access")
            if dto.force:
                asset.deleted_forever = True
            else:
                asset.trashed = True
        return Response(status_code=204)

    @app.post("/api/trash/restore/assets")
    def restore(request: Request, dto: BulkIdsDto) -> Any:
        if err := deny(request, "asset.delete"):
            return err
        count = 0
        for asset_id in dto.ids:
            asset = visible(asset_id)
            if asset is not None and asset.trashed:
                asset.trashed = False
                count += 1
        return {"count": count}

    @app.get("/api/duplicates")
    def duplicates(request: Request) -> Any:
        if err := deny(request, "duplicate.read"):
            return err
        out = []
        for group in state.duplicates:
            members = [state.assets[i] for i in group["ids"] if not state.assets[i].trashed]
            if len(members) > 1:
                out.append(
                    {
                        "duplicateId": group["duplicateId"],
                        "assets": [a.to_api() for a in members],
                        "suggestedKeepAssetIds": group.get("keep", []),
                    }
                )
        return out

    @app.post("/api/stacks", status_code=201)
    def create_stack(request: Request, dto: StackCreateDto) -> Any:
        if err := deny(request, "stack.create"):
            return err
        if any(visible(i) is None for i in dto.assetIds):
            return _error(400, "Not found or no asset.update access")
        state.stacks.append(list(dto.assetIds))
        return {
            "id": "33333333-3333-4333-8333-333333333333",
            "primaryAssetId": dto.assetIds[0],
            "assets": [state.assets[i].to_api() for i in dto.assetIds],
        }

    @app.get("/__requests")
    def requests_log() -> list[dict[str, Any]]:
        """Test helper (not part of Immich): everything this fake has been asked."""
        return [r for r in state.requests if not r["path"].startswith("/__")]

    return app


def demo_state() -> FakeState:
    """A small library with something in every category, for manual UI checks."""
    state = FakeState()
    t0 = datetime(2024, 6, 1, 9, 0, 0, tzinfo=UTC)
    for n in range(6):
        state.add(f"IMG_10{n}.jpg", images.natural(10 + n), taken=t0 + timedelta(days=n))
    state.add("IMG_2001.jpg", images.blurred(21, 14), taken=t0 + timedelta(days=7))
    state.add("IMG_2002.jpg", images.blurred(22, 18), taken=t0 + timedelta(days=8))
    state.add("IMG_2003 Sinh nhật bà.jpg", images.blurred(23, 12), taken=t0 + timedelta(days=9))
    state.add("IMG_3001.jpg", images.black(1), taken=t0 + timedelta(days=10))
    state.add("IMG_3002.jpg", images.white(), taken=t0 + timedelta(days=11))
    state.add("IMG_3003.jpg", images.sky(1), taken=t0 + timedelta(days=12))
    state.add("IMG_3004.jpg", images.night(1), taken=t0 + timedelta(days=13))
    for n in range(9):
        state.add(
            f"Screenshot_2024-06-{10 + n}-08-15-0{n}.png",
            images.screenshot(),
            taken=t0 + timedelta(days=14, hours=n),
            mime="image/png",
            make=None,
            model=None,
            width=1080,
            height=2400,
        )
    burst = t0 + timedelta(days=20)
    for n, (shift, blur) in enumerate([(0, 1.2), (3, 0.0), (6, 2.5), (9, 0.8)]):
        state.add(
            f"IMG_400{n}.jpg",
            images.burst_frame(40, shift, blur),
            taken=burst + timedelta(seconds=n),
        )
    burst2 = t0 + timedelta(days=21)
    for n, (shift, blur) in enumerate([(0, 0.0), (4, 1.5), (8, 1.0)]):
        state.add(
            f"IMG_410{n}.jpg",
            images.burst_frame(41, shift, blur),
            taken=burst2 + timedelta(seconds=n),
            favorite=n == 1,
        )
    a = state.add("IMG_5001.jpg", images.natural(50), taken=t0 + timedelta(days=22))
    b = state.add("IMG_5001 (1).jpg", images.natural(50), taken=t0 + timedelta(days=23))
    state.duplicates.append(
        {"duplicateId": "44444444-4444-4444-8444-444444444444", "ids": [a.id, b.id], "keep": [a.id]}
    )
    state.add("VID_6001.mp4", images.natural(60), taken=t0 + timedelta(days=24), type="VIDEO")
    return state


def main() -> None:
    import uvicorn

    parser = argparse.ArgumentParser(description="Fake Immich for local curator testing")
    parser.add_argument("--port", type=int, default=2299)
    parser.add_argument("--demo", action="store_true", help="seed a small sample library")
    args = parser.parse_args()
    state = demo_state() if args.demo else FakeState()
    print(
        f"fake Immich on http://127.0.0.1:{args.port}  api key: {API_KEY}  "
        f"assets: {len(state.assets)}"
    )
    uvicorn.run(create_fake(state), host="127.0.0.1", port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
