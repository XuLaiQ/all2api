"""Provider-specific watermark removal for generated Doubao images."""

from __future__ import annotations

import cv2
import numpy as np


def remove_doubao_watermark(data: bytes) -> bytes:
    """Repair the fixed lower-right Doubao watermark region.

    Current Doubao image variants all carry the rendered watermark pixels.
    OpenCV Telea inpainting reconstructs the lower-right scene region from
    surrounding pixels, preserving the image dimensions and encoding it as
    PNG.
    """

    source = np.frombuffer(bytes(data), dtype=np.uint8)
    image = cv2.imdecode(source, cv2.IMREAD_COLOR)
    if image is None:
        raise ValueError("image data cannot be decoded")
    height, width = image.shape[:2]
    if width < 512 or height < 512:
        return bytes(data)

    # The web watermark occupies the lower-right scene area. Keep a small
    # bottom margin outside the donor mask so the image edge remains stable.
    x1 = max(0, int(width * 0.655))
    x2 = width
    y2 = max(1, height - max(12, int(height * 0.01)))
    y1 = max(0, height - max(64, int(height * 0.165)))
    region_height = y2 - y1
    if region_height <= 0 or y1 - region_height < 0:
        return bytes(data)

    mask = np.zeros((height, width), dtype=np.uint8)
    mask[y1:y2, x1:x2] = 255
    repaired = cv2.inpaint(image, mask, 7, cv2.INPAINT_TELEA)
    success, encoded = cv2.imencode(".png", repaired)
    if not success:
        raise ValueError("image repair encoding failed")
    return bytes(encoded)


__all__ = ["remove_doubao_watermark"]
