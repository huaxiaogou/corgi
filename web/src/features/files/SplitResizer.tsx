import { useEffect, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";
import { clampSplitWidth, stepSplitWidth, type SplitBounds } from "../../lib/previewLayout";

interface Props {
  width: number;
  bounds: SplitBounds;
  onWidthChange: (next: number) => void;
  onReset: () => void;
  onDraggingChange: (dragging: boolean) => void;
}

export function SplitResizer({ width, bounds, onWidthChange, onReset, onDraggingChange }: Props) {
  const [dragging, setDragging] = useState(false);
  const dragRef = useRef<{ pointerId: number; startX: number; startWidth: number } | null>(null);

  useEffect(() => {
    if (!dragging) return;
    const prevUserSelect = document.body.style.userSelect;
    const prevCursor = document.body.style.cursor;
    document.body.style.userSelect = "none";
    document.body.style.cursor = "col-resize";
    onDraggingChange(true);
    return () => {
      document.body.style.userSelect = prevUserSelect;
      document.body.style.cursor = prevCursor;
      onDraggingChange(false);
    };
  }, [dragging, onDraggingChange]);

  const finishDrag = (el: HTMLDivElement, pointerId: number) => {
    if (!dragRef.current || dragRef.current.pointerId !== pointerId) return;
    dragRef.current = null;
    setDragging(false);
    try {
      if (el.hasPointerCapture(pointerId)) {
        el.releasePointerCapture(pointerId);
      }
    } catch {
      // ignore if capture already released
    }
  };

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    dragRef.current = { pointerId: e.pointerId, startX: e.clientX, startWidth: width };
    setDragging(true);
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // fallback safely if capture is unavailable
    }
  };

  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== e.pointerId) return;
    const next = clampSplitWidth(drag.startWidth + (drag.startX - e.clientX), bounds.min, bounds.max);
    onWidthChange(next);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const next = stepSplitWidth(width, e.key, e.shiftKey, bounds);
    if (next === null) return;
    e.preventDefault();
    onWidthChange(next);
  };

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="调整预览栏宽度（左右方向键微调，Shift 加速，Home/End 最小/最大，Enter 重置）"
      aria-valuemin={bounds.min}
      aria-valuemax={bounds.max}
      aria-valuenow={width}
      tabIndex={0}
      title="拖拽调整宽度（双击或按 Enter 重置）"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={(e) => finishDrag(e.currentTarget, e.pointerId)}
      onPointerCancel={(e) => finishDrag(e.currentTarget, e.pointerId)}
      onDoubleClick={onReset}
      onKeyDown={onKeyDown}
      className={`hidden lg:flex relative w-2 -mx-1 z-10 shrink-0 cursor-col-resize items-center justify-center group select-none touch-none focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-[var(--fg)] ${
        dragging ? "z-20" : ""
      }`}
    >
      <span
        aria-hidden="true"
        className={`w-[2px] h-10 rounded-full transition-opacity ${
          dragging
            ? "opacity-100"
            : "opacity-0 group-hover:opacity-100 group-focus-visible:opacity-100"
        }`}
        style={{ background: dragging ? "var(--fg-2)" : "var(--line-2)" }}
      />
    </div>
  );
}
