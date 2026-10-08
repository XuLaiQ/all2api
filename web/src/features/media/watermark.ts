const ALPHA_THRESHOLD = 2e-3;
const MAX_ALPHA = 0.99;
const LOGO_VALUE = 255;

type WatermarkConfig = {
  logoSize: 48 | 96;
  marginRight: number;
  marginBottom: number;
};

type WatermarkPosition = {
  x: number;
  y: number;
  width: number;
  height: number;
};

export type WatermarkResult = {
  dataUrl: string;
  width: number;
  height: number;
  position: WatermarkPosition;
};

const alphaMapCache = new Map<number, Promise<Float32Array>>();

function detectWatermarkConfig(width: number, height: number): WatermarkConfig {
  return width > 1024 && height > 1024
    ? { logoSize: 96, marginRight: 64, marginBottom: 64 }
    : { logoSize: 48, marginRight: 32, marginBottom: 32 };
}

function watermarkPosition(width: number, height: number, config: WatermarkConfig): WatermarkPosition {
  return {
    x: width - config.marginRight - config.logoSize,
    y: height - config.marginBottom - config.logoSize,
    width: config.logoSize,
    height: config.logoSize,
  };
}

function loadImage(blob: Blob | string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const image = new Image();
    const objectUrl = blob instanceof Blob ? URL.createObjectURL(blob) : blob;
    image.onload = () => {
      if (blob instanceof Blob) URL.revokeObjectURL(objectUrl);
      resolve(image);
    };
    image.onerror = () => {
      if (blob instanceof Blob) URL.revokeObjectURL(objectUrl);
      reject(new Error("无法读取图片内容"));
    };
    image.src = objectUrl;
  });
}

function canvasToBlob(canvas: HTMLCanvasElement): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob);
      else reject(new Error("图片编码失败"));
    }, "image/png");
  });
}

function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ""));
    reader.onerror = () => reject(new Error("图片结果读取失败"));
    reader.readAsDataURL(blob);
  });
}

async function getAlphaMap(size: 48 | 96): Promise<Float32Array> {
  const cached = alphaMapCache.get(size);
  if (cached) return cached;
  const promise = (async () => {
    const image = await loadImage(`/watermark/bg_${size}.png`);
    const canvas = document.createElement("canvas");
    canvas.width = size;
    canvas.height = size;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    if (!context) throw new Error("浏览器不支持 Canvas");
    context.drawImage(image, 0, 0, size, size);
    const pixels = context.getImageData(0, 0, size, size).data;
    const alphaMap = new Float32Array(size * size);
    for (let index = 0; index < alphaMap.length; index += 1) {
      const pixel = index * 4;
      alphaMap[index] = Math.max(pixels[pixel], pixels[pixel + 1], pixels[pixel + 2]) / 255;
    }
    return alphaMap;
  })();
  alphaMapCache.set(size, promise);
  return promise;
}

function restoreWatermarkRegion(
  pixels: ImageData,
  alphaMap: Float32Array,
  position: WatermarkPosition,
): void {
  for (let row = 0; row < position.height; row += 1) {
    for (let column = 0; column < position.width; column += 1) {
      const imageIndex = ((position.y + row) * pixels.width + position.x + column) * 4;
      const alpha = alphaMap[row * position.width + column];
      if (alpha < ALPHA_THRESHOLD) continue;
      const clampedAlpha = Math.min(alpha, MAX_ALPHA);
      const inverseAlpha = 1 - clampedAlpha;
      for (let channel = 0; channel < 3; channel += 1) {
        const watermarked = pixels.data[imageIndex + channel];
        const original = (watermarked - clampedAlpha * LOGO_VALUE) / inverseAlpha;
        pixels.data[imageIndex + channel] = Math.max(0, Math.min(255, Math.round(original)));
      }
    }
  }
}

export async function removeGeneratedWatermark(sourceUrl: string): Promise<WatermarkResult> {
  const response = await fetch(sourceUrl, { credentials: "include" });
  if (!response.ok) throw new Error(`图片读取失败（HTTP ${response.status}）`);
  const image = await loadImage(await response.blob());
  const canvas = document.createElement("canvas");
  canvas.width = image.naturalWidth || image.width;
  canvas.height = image.naturalHeight || image.height;
  const config = detectWatermarkConfig(canvas.width, canvas.height);
  const position = watermarkPosition(canvas.width, canvas.height, config);
  if (position.x < 0 || position.y < 0) {
    throw new Error("图片尺寸过小，无法定位生成水印区域");
  }
  const context = canvas.getContext("2d", { willReadFrequently: true });
  if (!context) throw new Error("浏览器不支持 Canvas");
  context.drawImage(image, 0, 0, canvas.width, canvas.height);
  const pixels = context.getImageData(0, 0, canvas.width, canvas.height);
  restoreWatermarkRegion(pixels, await getAlphaMap(config.logoSize), position);
  context.putImageData(pixels, 0, 0);
  const output = await canvasToBlob(canvas);
  return {
    dataUrl: await blobToDataUrl(output),
    width: canvas.width,
    height: canvas.height,
    position,
  };
}
