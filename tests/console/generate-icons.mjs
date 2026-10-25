// Crop the selected artwork and regenerate browser icons. Playwright is a test-only
// dependency; generated assets are embedded in the gateway binary as usual.
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { chromium } from "playwright";

const root = fileURLToPath(new URL("../..", import.meta.url));
const source = await readFile(path.join(root, "assets/icon-source.png"));
const output = path.join(root, "internal/gateway/consoleweb");
const browser = await chromium.launch({
  headless: true,
  ...(process.env.CONTROL_CONSOLE_TEST_CHROME
    ? { executablePath: process.env.CONTROL_CONSOLE_TEST_CHROME }
    : {}),
});
try {
  const page = await browser.newPage({ deviceScaleFactor: 1 });
  const cropped = await page.evaluate(async (base64) => {
    const image = new Image();
    image.src = "data:image/png;base64," + base64;
    await image.decode();
    const canvas = document.createElement("canvas");
    canvas.width = image.naturalWidth;
    canvas.height = image.naturalHeight;
    const context = canvas.getContext("2d");
    context.drawImage(image, 0, 0);
    const pixels = context.getImageData(0, 0, canvas.width, canvas.height);
    let left = canvas.width, top = canvas.height, right = -1, bottom = -1;
    for (let y = 0; y < canvas.height; y++) {
      for (let x = 0; x < canvas.width; x++) {
        const alpha = (y * canvas.width + x) * 4 + 3;
        // The source has stray alpha=1 pixels across otherwise empty margins.
        if (pixels.data[alpha] <= 1) {
          pixels.data[alpha] = 0;
          continue;
        }
        left = Math.min(left, x);
        top = Math.min(top, y);
        right = Math.max(right, x);
        bottom = Math.max(bottom, y);
      }
    }
    if (right < left) throw new Error("The source artwork is empty");
    context.putImageData(pixels, 0, 0);
    const width = right - left + 1, height = bottom - top + 1;
    const padding = Math.ceil(Math.max(width, height) * 0.01);
    const size = Math.max(width, height) + padding * 2;
    const crop = document.createElement("canvas");
    crop.width = crop.height = size;
    crop.getContext("2d").drawImage(canvas, left, top, width, height,
      Math.floor((size - width) / 2), Math.floor((size - height) / 2), width, height);
    return crop.toDataURL("image/png").split(",")[1];
  }, source.toString("base64"));
  const render = async (size) => Buffer.from(await page.evaluate(async ({ cropped, size }) => {
    const image = new Image();
    image.src = "data:image/png;base64," + cropped;
    await image.decode();
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = size;
    const context = canvas.getContext("2d");
    context.imageSmoothingQuality = "high";
    context.drawImage(image, 0, 0, size, size);
    return canvas.toDataURL("image/png").split(",")[1];
  }, { cropped, size }), "base64");
  await writeFile(path.join(root, "assets/icon-cropped.png"), Buffer.from(cropped, "base64"));
  await writeFile(path.join(root, "assets/icon.png"), await render(512));
  await writeFile(path.join(output, "brand-icon.png"), await render(64));
  await writeFile(path.join(output, "apple-touch-icon.png"), await render(180));
  await writeFile(path.join(output, "favicon-32.png"), await render(32));

  // ICO directory with PNG-encoded frames, supported by modern desktop browsers.
  const sizes = [16, 32, 48];
  const images = [];
  for (const size of sizes) images.push(await render(size));
  const header = Buffer.alloc(6 + sizes.length * 16);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(sizes.length, 4);
  let offset = header.length;
  images.forEach((image, index) => {
    const entry = 6 + index * 16;
    header[entry] = sizes[index];
    header[entry + 1] = sizes[index];
    header.writeUInt16LE(1, entry + 4);
    header.writeUInt16LE(32, entry + 6);
    header.writeUInt32LE(image.length, entry + 8);
    header.writeUInt32LE(offset, entry + 12);
    offset += image.length;
  });
  await writeFile(path.join(output, "favicon.ico"), Buffer.concat([header, ...images]));
  console.log("Generated cropped artwork, 64px brand icon, 16/32/48px ICO, 32px PNG, 180px touch icon, and 512px app icon.");
} finally {
  await browser.close();
}
