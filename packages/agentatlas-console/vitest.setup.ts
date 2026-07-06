import "@testing-library/jest-dom/vitest";

// jsdom lacks layout observers the canvas libraries expect.
class ResizeObserverShim {
  observe() {}
  unobserve() {}
  disconnect() {}
}
if (typeof globalThis.ResizeObserver === "undefined") {
  globalThis.ResizeObserver = ResizeObserverShim as unknown as typeof ResizeObserver;
}
if (typeof (globalThis as Record<string, unknown>).DOMMatrixReadOnly === "undefined") {
  (globalThis as Record<string, unknown>).DOMMatrixReadOnly = class {
    m22 = 1;
  };
}
if (!HTMLElement.prototype.getBoundingClientRect.toString().includes("native code")) {
  // already patched
} else {
  const original = HTMLElement.prototype.getBoundingClientRect;
  HTMLElement.prototype.getBoundingClientRect = function () {
    const rect = original.call(this);
    if (rect.width === 0 && rect.height === 0) {
      return { ...rect, width: 800, height: 600, top: 0, left: 0, right: 800, bottom: 600 } as DOMRect;
    }
    return rect;
  };
}
