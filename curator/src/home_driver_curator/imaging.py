"""Pixel helpers shared by the detectors. numpy + Pillow only (no OpenCV)."""

from __future__ import annotations

import io
from dataclasses import dataclass

import numpy as np
from PIL import Image, ImageOps

GRAY_LONG_SIDE = 512
RGB_LONG_SIDE = 256
TILE_GRID = 8


@dataclass(frozen=True)
class Frame:
    """A decoded preview, normalised so detector scores are comparable between assets.

    gray: float32, HxW, 0..255, long side GRAY_LONG_SIDE.
    rgb:  uint8, HxWx3, long side RGB_LONG_SIDE.
    """

    gray: np.ndarray
    rgb: np.ndarray


def _fit(img: Image.Image, long_side: int) -> Image.Image:
    w, h = img.size
    scale = long_side / max(w, h)
    size = (max(8, round(w * scale)), max(8, round(h * scale)))
    return img.resize(size, Image.Resampling.BILINEAR, reducing_gap=2.0)


def frame_from_image(img: Image.Image) -> Frame:
    img = ImageOps.exif_transpose(img)
    if img.mode != "RGB":
        img = img.convert("RGB")
    gray = np.asarray(_fit(img, GRAY_LONG_SIDE).convert("L"), dtype=np.float32)
    rgb = np.asarray(_fit(img, RGB_LONG_SIDE), dtype=np.uint8)
    return Frame(gray=gray, rgb=rgb)


def decode(data: bytes) -> Frame:
    """Decode image bytes. Raises PIL.UnidentifiedImageError / OSError on bad data."""
    with Image.open(io.BytesIO(data)) as img:
        if img.format == "JPEG":
            # Let libjpeg downscale while decoding: far less CPU and memory on big previews.
            img.draft("RGB", (GRAY_LONG_SIDE * 2, GRAY_LONG_SIDE * 2))
        img.load()
        return frame_from_image(img)


def laplacian(gray: np.ndarray) -> np.ndarray:
    """4-neighbour Laplacian of the interior pixels."""
    c = gray[1:-1, 1:-1]
    return 4.0 * c - gray[:-2, 1:-1] - gray[2:, 1:-1] - gray[1:-1, :-2] - gray[1:-1, 2:]


def tiles(arr: np.ndarray, grid: int = TILE_GRID) -> np.ndarray:
    """Cut a 2-D array into grid*grid equal tiles -> shape (grid*grid, th, tw)."""
    h, w = arr.shape
    th, tw = h // grid, w // grid
    cut = arr[: th * grid, : tw * grid]
    return cut.reshape(grid, th, grid, tw).swapaxes(1, 2).reshape(grid * grid, th, tw)


@dataclass(frozen=True)
class Sharpness:
    """Tile-based sharpness.

    relative: Laplacian energy divided by local detail energy, averaged over the sharpest
              textured tiles. Contrast-independent: ~0.8+ for crisp detail, <0.12 when
              badly blurred.
              None when the image has too little texture to judge.
    absolute: mean Laplacian variance of the sharpest tiles; only meaningful when comparing
              near-identical frames (burst keeper choice).
    textured_fraction: share of tiles that contained enough detail to be judged.
    """

    relative: float | None
    absolute: float
    textured_fraction: float


# A tile needs at least this much detail (std-dev in grey levels, after removing smooth
# gradients) before its sharpness is trusted. Below it the tile is sky, wall, fog...
MIN_TILE_DETAIL_STD = 5.0
MIN_TEXTURED_FRACTION = 0.25
TOP_TILES = 4


def sharpness(gray: np.ndarray) -> Sharpness:
    lap_t = tiles(laplacian(gray))
    img_t = tiles(gray[1:-1, 1:-1])
    lap_var = lap_t.var(axis=(1, 2))
    # Remove each tile's smooth gradient (row and column trends) so a sky ramp or vignette
    # does not count as "detail that should have been sharp".
    row = img_t.mean(axis=2, keepdims=True)
    col = img_t.mean(axis=1, keepdims=True)
    mean = img_t.mean(axis=(1, 2), keepdims=True)
    detail_var = (img_t - row - col + mean).var(axis=(1, 2))

    absolute = float(np.sort(lap_var)[-TOP_TILES:].mean())
    textured = detail_var >= MIN_TILE_DETAIL_STD**2
    fraction = float(textured.mean())
    if fraction < MIN_TEXTURED_FRACTION:
        return Sharpness(relative=None, absolute=absolute, textured_fraction=fraction)
    rel = lap_var[textured] / detail_var[textured]
    top = np.sort(rel)[-TOP_TILES:]
    return Sharpness(relative=float(top.mean()), absolute=absolute, textured_fraction=fraction)


def dhash(gray: np.ndarray) -> int:
    """64-bit difference hash (9x8 area-averaged grid, horizontal gradient sign)."""
    h, w = gray.shape
    ys = np.linspace(0, h, 9).astype(int)
    xs = np.linspace(0, w, 10).astype(int)
    small = np.empty((8, 9), dtype=np.float64)
    for i in range(8):
        for j in range(9):
            small[i, j] = gray[ys[i] : ys[i + 1], xs[j] : xs[j + 1]].mean()
    bits = (small[:, 1:] > small[:, :-1]).flatten()
    value = 0
    for bit in bits:
        value = (value << 1) | int(bit)
    return value


def hamming(a: int, b: int) -> int:
    return (a ^ b).bit_count()


def flat_fraction(rgb: np.ndarray) -> float:
    """Share of pixels whose 3x3 neighbourhood is (almost) one flat colour."""
    g = rgb.astype(np.int16)
    c = g[1:-1, 1:-1]
    diff = np.zeros(c.shape[:2], dtype=np.int16)
    for dy, dx in ((0, 1), (2, 1), (1, 0), (1, 2)):
        n = g[dy : dy + c.shape[0], dx : dx + c.shape[1]]
        diff = np.maximum(diff, np.abs(n - c).max(axis=2))
    return float((diff <= 2).mean())


def distinct_colours(rgb: np.ndarray) -> int:
    """Number of distinct colours after quantising to 4 bits per channel (max 4096)."""
    q = (rgb >> 4).astype(np.uint16)
    packed = (q[..., 0] << 8) | (q[..., 1] << 4) | q[..., 2]
    return int(np.unique(packed).size)


def is_grayscale(rgb: np.ndarray) -> bool:
    c = rgb.astype(np.int16)
    spread = np.abs(c - c.mean(axis=2, keepdims=True)).max(axis=2)
    return bool(np.percentile(spread, 99) <= 6)
