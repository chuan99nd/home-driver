"""Per-asset detectors.

A detector is a pure function ``(Frame, AssetMeta) -> Finding``. It never does I/O and never
sees thresholds: it reports a raw ``score`` and a human-readable ``reason``. The scan stores
every finding; whether a finding becomes a suggestion is decided later by comparing the
stored score with the current threshold (see ``DetectorSpec.flags``), so thresholds can be
tuned without re-downloading anything.

``veto=True`` means "this detector refuses to judge this image" (too little texture, night
shot, has camera EXIF, ...). A vetoed finding never becomes a suggestion, whatever the score.
"""

from __future__ import annotations

import re
from collections.abc import Callable
from dataclasses import dataclass
from typing import Literal

import numpy as np

from . import imaging
from .config import Settings
from .imaging import Frame


@dataclass(frozen=True)
class AssetMeta:
    file_name: str = ""
    mime: str = ""
    width: int | None = None
    height: int | None = None
    make: str = ""
    model: str = ""


@dataclass(frozen=True)
class Finding:
    score: float
    reason: str
    veto: bool = False


@dataclass(frozen=True)
class DetectorSpec:
    name: str  # also the suggestion category
    label: str
    fn: Callable[[Frame, AssetMeta], Finding]
    enabled_attr: str  # Settings field (bool)
    threshold_attr: str  # Settings field (float)
    flag_when: Literal["below", "above"]

    def enabled(self, settings: Settings) -> bool:
        return bool(getattr(settings, self.enabled_attr))

    def threshold(self, settings: Settings) -> float:
        return float(getattr(settings, self.threshold_attr))

    def flags(self, score: float, veto: bool, settings: Settings) -> bool:
        if veto or not self.enabled(settings):
            return False
        limit = self.threshold(settings)
        return score < limit if self.flag_when == "below" else score >= limit


# --- blurry ---------------------------------------------------------------------------

# Below this mean brightness (0..255) a photo is treated as a night shot: noise and long
# exposures make sharpness unreliable, so we do not judge it.
NIGHT_MEAN_LUMA = 40.0
NOT_JUDGED_SCORE = 9.0  # reported when vetoed, so it sorts as "sharp" in reports


def detect_blurry(frame: Frame, meta: AssetMeta) -> Finding:
    """Score = relative sharpness of the sharpest textured tiles (lower = blurrier).

    Using only the sharpest tiles keeps intentional bokeh (sharp subject, soft background)
    from being flagged; the texture requirement keeps sky/wall/fog shots from being flagged.
    """
    gray = frame.gray
    mean = float(gray.mean())
    if mean < NIGHT_MEAN_LUMA:
        return Finding(NOT_JUDGED_SCORE, "not judged: dark / night shot", veto=True)
    s = imaging.sharpness(gray)
    if s.relative is None:
        return Finding(
            NOT_JUDGED_SCORE,
            f"not judged: too little texture ({s.textured_fraction:.0%} of the frame has detail)",
            veto=True,
        )
    return Finding(
        round(s.relative, 4),
        f"Looks out of focus: even the sharpest areas are soft (sharpness {s.relative:.3f})",
    )


# --- dark / blank ---------------------------------------------------------------------


def detect_dark(frame: Frame, meta: AssetMeta) -> Finding:
    """Score = how much tonal information the frame holds (lower = blanker), 0..255.

    min(tonal range, half the brightest level, half the headroom above the darkest level),
    using the 0.1 / 99.9 percentiles so a few hot pixels do not rescue a lens-cap shot.
    """
    lo, hi = (float(v) for v in np.percentile(frame.gray, [0.1, 99.9]))
    tonal_range = hi - lo
    black = hi / 2.0
    white = (255.0 - lo) / 2.0
    score = min(tonal_range, black, white)
    if score == black:
        reason = f"Almost completely black (brightest area is {hi:.0f}/255)"
    elif score == white:
        reason = f"Almost completely white (darkest area is {lo:.0f}/255)"
    else:
        reason = f"Blank, uniform frame (tones span only {tonal_range:.0f}/255)"
    return Finding(round(score, 2), reason)


# --- screenshot / document ------------------------------------------------------------

_SCREENSHOT_NAME = re.compile(
    r"screen[ _-]?shot|screen[ _-]?record|screen[ _-]?capture|screen[ _-]?recording"
    r"|^capture[ _-]|captura de pantalla|capture d.[eé]cran|bildschirmfoto"
    r"|ảnh chụp màn hình|anh chup man hinh",
    re.IGNORECASE,
)

# Common screen aspect ratios (long side / short side): phones, tablets, laptops.
_SCREEN_RATIOS = (
    16 / 9,
    18 / 9,
    18.5 / 9,
    19 / 9,
    19.3 / 9,
    19.5 / 9,
    20 / 9,
    20.5 / 9,
    21 / 9,
    22 / 9,
    4 / 3,
    16 / 10,
    3 / 2,
    2360 / 1640,
    2388 / 1668,
)
# 4:3 and 3:2 are also the commonest *camera* ratios, so they earn no evidence.
_CAMERA_RATIOS = (4 / 3, 3 / 2)


def _screen_ratio(width: int | None, height: int | None) -> bool:
    if not width or not height:
        return False
    ratio = max(width, height) / min(width, height)
    if any(abs(ratio - r) < 0.01 for r in _CAMERA_RATIOS):
        return False
    return any(abs(ratio - r) / r < 0.012 for r in _SCREEN_RATIOS)


def detect_screenshot(frame: Frame, meta: AssetMeta) -> Finding:
    """Score = accumulated evidence 0..1 that this is a screenshot / digital document.

    Anything with a camera make or model in EXIF is vetoed: a real photo is never suggested
    by this detector, even a photo of a receipt.
    """
    if meta.make or meta.model:
        return Finding(0.0, "not judged: taken with a camera", veto=True)
    score = 0.15
    why = ["no camera information"]
    if _SCREENSHOT_NAME.search(meta.file_name):
        score += 0.45
        why.append("file name says screenshot")
    is_png = meta.mime.lower() == "image/png" or meta.file_name.lower().endswith(".png")
    if is_png:
        score += 0.2
        why.append("PNG file")
    if _screen_ratio(meta.width, meta.height):
        score += 0.1
        why.append(f"screen-shaped ({meta.width}x{meta.height})")
    flat = imaging.flat_fraction(frame.rgb)
    if flat >= 0.5:
        score += 0.2
        why.append(f"{flat:.0%} flat colour")
    elif flat >= 0.3:
        score += 0.1
        why.append(f"{flat:.0%} flat colour")
    colours = imaging.distinct_colours(frame.rgb)
    # A black-and-white photo also has few colours, so grey images earn nothing here.
    if colours <= 64 and not imaging.is_grayscale(frame.rgb):
        score += 0.1
        why.append(f"only {colours} colours")
    return Finding(round(min(score, 1.0), 2), "Looks like a screenshot: " + ", ".join(why))


DETECTORS: tuple[DetectorSpec, ...] = (
    DetectorSpec("blurry", "Blurry", detect_blurry, "detect_blurry", "blur_threshold", "below"),
    DetectorSpec("dark", "Dark / blank", detect_dark, "detect_dark", "dark_threshold", "below"),
    DetectorSpec(
        "screenshot",
        "Screenshots",
        detect_screenshot,
        "detect_screenshot",
        "screenshot_threshold",
        "above",
    ),
)

# Categories produced by grouping rather than by a per-asset detector.
GROUP_CATEGORIES: dict[str, str] = {"burst": "Bursts", "duplicate": "Duplicates"}

CATEGORY_LABELS: dict[str, str] = {**{d.name: d.label for d in DETECTORS}, **GROUP_CATEGORIES}
