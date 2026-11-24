"""Synthetic test images (Pillow + numpy only, deterministic)."""

from __future__ import annotations

import io

import numpy as np
from PIL import Image, ImageDraw, ImageFilter

SIZE = (1440, 1080)


def _rng(seed: int) -> np.random.Generator:
    return np.random.default_rng(seed)


def natural(seed: int = 1, size: tuple[int, int] = SIZE) -> Image.Image:
    """A photo-like scene: smooth 1/f background, crisp shapes and fine texture."""
    rng = _rng(seed)
    w, h = size
    spectrum = rng.normal(size=(h, w)) + 1j * rng.normal(size=(h, w))
    fy = np.fft.fftfreq(h)[:, None]
    fx = np.fft.fftfreq(w)[None, :]
    spectrum /= np.maximum(np.hypot(fx, fy), 1.0 / max(w, h)) ** 1.2
    field = np.real(np.fft.ifft2(spectrum))
    field = (field - field.min()) / (np.ptp(field) + 1e-9)
    base = np.stack([field * 200 + 30, field * 170 + 50, (1 - field) * 150 + 60], axis=-1)
    img = Image.fromarray(base.clip(0, 255).astype(np.uint8), "RGB")
    draw = ImageDraw.Draw(img)
    for _ in range(40):
        x, y = int(rng.integers(0, w)), int(rng.integers(0, h))
        rw, rh = int(rng.integers(20, 220)), int(rng.integers(20, 220))
        colour = tuple(int(c) for c in rng.integers(0, 255, 3))
        if rng.random() < 0.5:
            draw.rectangle([x, y, x + rw, y + rh], fill=colour)
        else:
            draw.ellipse([x, y, x + rw, y + rh], fill=colour)
    for _ in range(60):
        pts = [int(v) for v in rng.integers(0, max(w, h), 4)]
        draw.line(pts, fill=tuple(int(c) for c in rng.integers(0, 255, 3)), width=2)
    return img


def blurred(seed: int = 1, radius: float = 8.0) -> Image.Image:
    return natural(seed).filter(ImageFilter.GaussianBlur(radius))


def bokeh(seed: int = 1) -> Image.Image:
    """Sharp subject in the centre, heavily blurred background."""
    sharp = natural(seed)
    out = sharp.filter(ImageFilter.GaussianBlur(14))
    w, h = sharp.size
    box = (w // 2 - 220, h // 2 - 220, w // 2 + 220, h // 2 + 220)
    out.paste(sharp.crop(box), box)
    return out


def sky(seed: int = 1) -> Image.Image:
    """Sharp but nearly textureless: a smooth sky gradient with a faint haze."""
    rng = _rng(seed)
    w, h = SIZE
    ramp = np.linspace(0.0, 1.0, h)[:, None] * np.ones((1, w))
    rgb = np.stack([90 + ramp * 110, 140 + ramp * 80, 225 - ramp * 15], axis=-1)
    rgb += rng.normal(0, 0.8, rgb.shape)
    return Image.fromarray(rgb.clip(0, 255).astype(np.uint8), "RGB")


def night(seed: int = 1) -> Image.Image:
    """Dark scene with a few lights and sensor noise: must not be called blurry or blank."""
    rng = _rng(seed)
    w, h = SIZE
    arr = rng.normal(14, 5, (h, w, 3))
    img = Image.fromarray(arr.clip(0, 255).astype(np.uint8), "RGB")
    draw = ImageDraw.Draw(img)
    for _ in range(25):
        x, y = int(rng.integers(0, w)), int(rng.integers(0, h))
        r = int(rng.integers(6, 30))
        draw.ellipse([x - r, y - r, x + r, y + r], fill=(250, 230, 160))
    return img.filter(ImageFilter.GaussianBlur(2))


def black(seed: int = 1) -> Image.Image:
    """Pocket shot / lens cap: black with a little sensor noise."""
    rng = _rng(seed)
    w, h = SIZE
    arr = rng.normal(3, 1.5, (h, w, 3))
    return Image.fromarray(arr.clip(0, 255).astype(np.uint8), "RGB")


def white() -> Image.Image:
    return Image.new("RGB", SIZE, (252, 252, 252))


def uniform(colour: tuple[int, int, int] = (120, 96, 80)) -> Image.Image:
    return Image.new("RGB", SIZE, colour)


def screenshot(size: tuple[int, int] = (1080, 2400)) -> Image.Image:
    """Phone UI: flat panels, a few colours, rows of 'text'."""
    w, h = size
    img = Image.new("RGB", size, (250, 250, 250))
    draw = ImageDraw.Draw(img)
    draw.rectangle([0, 0, w, 160], fill=(25, 118, 210))
    for row in range(12):
        y = 220 + row * 170
        draw.rectangle([40, y, 150, y + 110], fill=(224, 224, 224))
        draw.rectangle([190, y + 10, 190 + 500 + (row % 3) * 90, y + 40], fill=(33, 33, 33))
        draw.rectangle([190, y + 65, 190 + 320 + (row % 4) * 60, y + 85], fill=(117, 117, 117))
    draw.rectangle([0, h - 140, w, h], fill=(245, 245, 245))
    return img


def burst_frame(seed: int = 1, shift: int = 0, blur: float = 0.0) -> Image.Image:
    """Same scene, shifted a few pixels (hand shake) and optionally softer."""
    base = natural(seed)
    if shift:
        arr = np.roll(np.asarray(base), shift, axis=1)
        base = Image.fromarray(arr, "RGB")
    if blur:
        base = base.filter(ImageFilter.GaussianBlur(blur))
    return base


def to_jpeg(img: Image.Image, quality: int = 85) -> bytes:
    buf = io.BytesIO()
    img.save(buf, "JPEG", quality=quality)
    return buf.getvalue()


def to_webp(img: Image.Image) -> bytes:
    buf = io.BytesIO()
    img.save(buf, "WEBP", quality=80)
    return buf.getvalue()
