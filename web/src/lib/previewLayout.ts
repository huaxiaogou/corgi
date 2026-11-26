export const MIN_CHAT_WIDTH = 360;
export const MIN_HTML_PANE_WIDTH = 360;
export const MIN_FILE_PANE_WIDTH = 320;
export const MAX_PANE_WIDTH = 1080;
export const DEFAULT_FILE_PANE_WIDTH = 400;
export const DEFAULT_HTML_RATIO = 0.48;
export const WIDE_HTML_RATIO = 0.62;
export const KEYBOARD_STEP = 32;
export const KEYBOARD_SHIFT_STEP = 64;

export interface SplitBounds {
  min: number;
  max: number;
  defaultWidth: number;
  wideWidth: number;
}

export function clampSplitWidth(width: number, min: number, max: number): number {
  const safeMax = Math.max(min, max);
  return Math.round(Math.min(safeMax, Math.max(min, width)));
}

export function computeSplitBounds(containerWidth: number, isHTML: boolean): SplitBounds {
  const min = isHTML ? MIN_HTML_PANE_WIDTH : MIN_FILE_PANE_WIDTH;
  const available = containerWidth > 0 ? containerWidth - MIN_CHAT_WIDTH : 760;
  const max = Math.max(min, Math.min(MAX_PANE_WIDTH, available));
  const defaultRaw = isHTML ? containerWidth * DEFAULT_HTML_RATIO : DEFAULT_FILE_PANE_WIDTH;
  const wideRaw = isHTML ? containerWidth * WIDE_HTML_RATIO : Math.max(540, containerWidth * 0.5);
  return {
    min,
    max,
    defaultWidth: clampSplitWidth(defaultRaw || min, min, max),
    wideWidth: clampSplitWidth(wideRaw || max, min, max),
  };
}

export function stepSplitWidth(
  current: number,
  key: string,
  shiftKey: boolean,
  bounds: SplitBounds
): number | null {
  const delta = shiftKey ? KEYBOARD_SHIFT_STEP : KEYBOARD_STEP;
  switch (key) {
    case "ArrowLeft":
      return clampSplitWidth(current + delta, bounds.min, bounds.max);
    case "ArrowRight":
      return clampSplitWidth(current - delta, bounds.min, bounds.max);
    case "Home":
      return bounds.min;
    case "End":
      return bounds.max;
    case "Enter":
    case "0":
      return bounds.defaultWidth;
    default:
      return null;
  }
}
