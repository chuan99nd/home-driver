from __future__ import annotations

import inspect

import pytest

from home_driver_curator.immich import (
    ImmichAuthError,
    ImmichClient,
    ImmichNotFound,
    ImmichPermissionError,
    ImmichUnavailable,
)

from .conftest import jpeg
from .fake_immich import FakeState


def add_many(fake: FakeState, n: int) -> list[str]:
    return [fake.add(f"img_{i}.jpg", jpeg("uniform")).id for i in range(n)]


def test_listing_follows_cursors_to_the_end(fake: FakeState, immich: ImmichClient) -> None:
    ids = add_many(fake, 5)
    fake.add("clip.mp4", jpeg("uniform"), type="VIDEO")
    pages = list(immich.iter_image_pages(page_size=2))
    assert [len(p) for p in pages] == [2, 2, 1]
    assert [a.id for p in pages for a in p] == ids  # oldest first, videos excluded
    bodies = [r["json"] for r in fake.calls("POST", "/api/search/metadata")]
    assert len(bodies) == 3
    assert "cursor" not in bodies[0] and bodies[1]["cursor"] and bodies[2]["cursor"]
    assert bodies[0]["filter"] == {"type": {"eq": "IMAGE"}}
    assert bodies[0]["withExif"] is True


def test_asset_fields_are_parsed(fake: FakeState, immich: ImmichClient) -> None:
    a = fake.add("x.jpg", jpeg("uniform"), favorite=True, visibility="archive")
    got = immich.get_asset(a.id)
    assert got.is_favorite and got.is_archived and not got.is_trashed
    assert got.camera == "Apple iPhone 13" and got.file_name == "x.jpg"
    assert got.taken_at is not None and got.width == 4032


def test_retries_with_backoff_then_succeeds(
    fake: FakeState, immich: ImmichClient, sleeps: list[float]
) -> None:
    add_many(fake, 1)
    fake.fail_next = 2
    assert len(next(immich.iter_image_pages())) == 1
    assert sleeps == [0.5, 1.0]


def test_gives_up_with_a_clear_error(fake: FakeState, immich: ImmichClient) -> None:
    fake.fail_next = 99
    with pytest.raises(ImmichUnavailable, match="after 4 attempt"):
        immich.server_version()


def test_missing_permission_is_explained(fake: FakeState, immich: ImmichClient) -> None:
    ids = add_many(fake, 1)
    fake.permissions = ["asset.read", "asset.view"]
    with pytest.raises(ImmichPermissionError) as err:
        immich.trash_assets(ids)
    assert err.value.permission == "asset.delete"
    assert "asset.delete" in str(err.value) and "API Keys" in str(err.value)
    assert immich.missing_permissions() == ["asset.delete", "duplicate.read", "stack.create"]
    assert not fake.assets[ids[0]].trashed


def test_wrong_api_key(fake: FakeState, immich: ImmichClient) -> None:
    immich._headers["x-api-key"] = "wrong"
    with pytest.raises(ImmichAuthError):
        immich.api_key_permissions()


def test_unknown_asset_is_not_found(immich: ImmichClient) -> None:
    with pytest.raises(ImmichNotFound):
        immich.get_asset("99999999-9999-4999-8999-999999999999")


def test_trash_always_sends_force_false(fake: FakeState, immich: ImmichClient) -> None:
    ids = add_many(fake, 2)
    immich.trash_assets(ids)
    (call,) = fake.calls("DELETE", "/api/assets")
    assert call["json"] == {"ids": ids, "force": False}
    assert all(fake.assets[i].trashed and not fake.assets[i].deleted_forever for i in ids)
    # There is no way to ask the client for a permanent delete.
    assert list(inspect.signature(ImmichClient.trash_assets).parameters) == ["self", "asset_ids"]


def test_restore_brings_assets_back(fake: FakeState, immich: ImmichClient) -> None:
    ids = add_many(fake, 2)
    immich.trash_assets(ids)
    assert immich.restore_assets(ids) == 2
    (call,) = fake.calls("POST", "/api/trash/restore/assets")
    assert call["json"] == {"ids": ids}
    assert not any(fake.assets[i].trashed for i in ids)


def test_stack_puts_primary_first_and_is_not_retried(fake: FakeState, immich: ImmichClient) -> None:
    a, b, c = add_many(fake, 3)
    immich.create_stack(b, [a, b, c])
    assert fake.stacks == [[b, a, c]]
    fake.fail_next = 1
    with pytest.raises(ImmichUnavailable):
        immich.create_stack(a, [b])
    assert len(fake.calls("POST", "/api/stacks")) == 2


def test_thumbnail_sizes(fake: FakeState, immich: ImmichClient) -> None:
    (asset_id,) = add_many(fake, 1)
    data, ctype = immich.get_thumbnail(asset_id, "preview")
    assert ctype == "image/jpeg" and data[:2] == b"\xff\xd8"
    _, ctype = immich.get_thumbnail(asset_id, "thumbnail")
    assert ctype == "image/webp"
    assert fake.calls("GET", "/api/assets/")[-1]["query"] == {"size": "thumbnail"}
    with pytest.raises(ValueError):
        immich.get_thumbnail(asset_id, "original")
    with pytest.raises(ValueError):
        immich.get_thumbnail("../../etc/passwd")


def test_duplicates_and_version(fake: FakeState, immich: ImmichClient) -> None:
    a, b = add_many(fake, 2)
    fake.duplicates.append(
        {"duplicateId": "44444444-4444-4444-8444-444444444444", "ids": [a, b], "keep": [a]}
    )
    (group,) = immich.get_duplicates()
    assert [x.id for x in group.assets] == [a, b] and group.suggested_keep_ids == [a]
    assert str(immich.server_version()) == "3.4.2" and immich.server_version().supported
    fake.version = (3, 1, 0)
    assert not immich.server_version().supported
