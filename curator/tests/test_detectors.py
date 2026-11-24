from __future__ import annotations

import dataclasses

import pytest

from home_driver_curator import imaging
from home_driver_curator.config import Settings
from home_driver_curator.detectors import (
    DETECTORS,
    AssetMeta,
    detect_blurry,
    detect_dark,
    detect_screenshot,
)

from .conftest import jpeg

S = Settings()
BY_NAME = {d.name: d for d in DETECTORS}
CAMERA = AssetMeta("IMG_1.jpg", "image/jpeg", 4032, 3024, "Apple", "iPhone 13")
SHOT = AssetMeta("Screenshot_20240501-101500.png", "image/png", 1080, 2400)


def frame(kind: str, *args: object) -> imaging.Frame:
    return imaging.decode(jpeg(kind, *args))


def flagged(name: str, finding: object) -> bool:
    return BY_NAME[name].flags(finding.score, finding.veto, S)  # type: ignore[attr-defined]


# --- blurry -------------------------------------------------------------------------


@pytest.mark.parametrize("radius", [10.0, 14.0, 20.0])
def test_blurred_photo_is_flagged(radius: float) -> None:
    f = detect_blurry(frame("blurred", 1, radius), CAMERA)
    assert not f.veto
    assert flagged("blurry", f), f


@pytest.mark.parametrize("seed", [1, 2, 3])
def test_sharp_photo_is_not_flagged(seed: int) -> None:
    f = detect_blurry(frame("natural", seed), CAMERA)
    assert not flagged("blurry", f)
    assert f.score > 5 * S.blur_threshold  # comfortably clear of the threshold


def test_slightly_soft_photo_is_not_flagged() -> None:
    assert not flagged("blurry", detect_blurry(frame("blurred", 1, 2.0), CAMERA))


def test_bokeh_with_sharp_subject_is_not_flagged() -> None:
    assert not flagged("blurry", detect_blurry(frame("bokeh", 1), CAMERA))


def test_low_texture_sky_is_not_judged() -> None:
    f = detect_blurry(frame("sky", 1), CAMERA)
    assert f.veto and "texture" in f.reason
    assert not flagged("blurry", f)


def test_night_shot_is_not_judged() -> None:
    f = detect_blurry(frame("night", 1), CAMERA)
    assert f.veto
    assert not flagged("blurry", f)


# --- dark / blank ---------------------------------------------------------------------


@pytest.mark.parametrize("kind", ["black", "white", "uniform"])
def test_blank_frames_are_flagged(kind: str) -> None:
    args = (1,) if kind == "black" else ()
    f = detect_dark(frame(kind, *args), CAMERA)
    assert flagged("dark", f), f


@pytest.mark.parametrize("kind", ["natural", "night", "sky", "screenshot"])
def test_real_content_is_not_blank(kind: str) -> None:
    args = () if kind == "screenshot" else (1,)
    assert not flagged("dark", detect_dark(frame(kind, *args), CAMERA))


# --- screenshot -----------------------------------------------------------------------


def test_screenshot_is_flagged() -> None:
    f = detect_screenshot(frame("screenshot"), SHOT)
    assert flagged("screenshot", f)
    assert "file name" in f.reason


def test_png_screenshot_without_telling_name_is_flagged() -> None:
    meta = AssetMeta("IMG_0042.PNG", "image/png", 1170, 2532)
    assert flagged("screenshot", detect_screenshot(frame("screenshot"), meta))


def test_camera_photo_is_never_a_screenshot() -> None:
    meta = dataclasses.replace(SHOT, make="Apple", model="iPhone 13")
    f = detect_screenshot(frame("screenshot"), meta)
    assert f.veto and not flagged("screenshot", f)


def test_messenger_photo_without_exif_is_not_a_screenshot() -> None:
    meta = AssetMeta("IMG-20240101-WA0001.jpg", "image/jpeg", 1600, 1200)
    assert not flagged("screenshot", detect_screenshot(frame("natural", 1), meta))


def test_jpeg_photo_with_phone_screen_shape_is_not_a_screenshot() -> None:
    meta = AssetMeta("received_123.jpeg", "image/jpeg", 1080, 2400)
    assert not flagged("screenshot", detect_screenshot(frame("natural", 2), meta))


# --- switches, hashing ----------------------------------------------------------------


def test_each_detector_can_be_switched_off() -> None:
    off = dataclasses.replace(S, detect_blurry=False, detect_dark=False, detect_screenshot=False)
    assert not BY_NAME["blurry"].flags(0.0, False, off)
    assert not BY_NAME["dark"].flags(0.0, False, off)
    assert not BY_NAME["screenshot"].flags(1.0, False, off)


def test_thresholds_are_applied_from_settings() -> None:
    f = detect_blurry(frame("blurred", 1, 14.0), CAMERA)
    strict = dataclasses.replace(S, blur_threshold=0.0001)
    assert not BY_NAME["blurry"].flags(f.score, f.veto, strict)


def test_dhash_groups_near_identical_frames_only() -> None:
    a = imaging.dhash(frame("burst_frame", 7, 0, 2.0).gray)
    b = imaging.dhash(frame("burst_frame", 7, 3, 0.0).gray)
    c = imaging.dhash(frame("burst_frame", 7, 6, 1.0).gray)
    other = imaging.dhash(frame("natural", 1).gray)
    assert imaging.hamming(a, b) <= S.burst_hamming
    assert imaging.hamming(a, c) <= S.burst_hamming
    assert imaging.hamming(a, other) > 3 * S.burst_hamming


def test_sharpness_ranks_burst_frames() -> None:
    soft = imaging.sharpness(frame("burst_frame", 7, 0, 2.0).gray).absolute
    crisp = imaging.sharpness(frame("burst_frame", 7, 3, 0.0).gray).absolute
    assert crisp > soft
