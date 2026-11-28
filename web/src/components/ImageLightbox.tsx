import { useCallback, useEffect, useRef, useState, type PointerEvent } from "react";
import { createPortal } from "react-dom";
import { ChevronLeft, ChevronRight, Download, Minus, Plus, RotateCcw, X } from "lucide-react";
import { computeSwipeOffset, type GalleryImageItem } from "../lib/imageGallery";
import { formatByteSize } from "../lib/previewLink";

interface Props {
  items: GalleryImageItem[];
  initialIndex: number;
  onClose: () => void;
}

const SWIPE_COMMIT_PX = 56;
const BTN_CLS = "w-8 h-8 rounded-lg flex items-center justify-center hover:bg-white/10 cursor-pointer disabled:opacity-35";
const NAV_CLS = "absolute z-10 w-9 h-9 rounded-full flex items-center justify-center text-white cursor-pointer disabled:opacity-25 disabled:cursor-not-allowed";

export function ImageLightbox({ items, initialIndex, onClose }: Props) {
  const [idx, setIdx] = useState(() => Math.max(0, Math.min(initialIndex, Math.max(0, items.length - 1))));
  const [scale, setScale] = useState(1);
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const [dragging, setDragging] = useState(false);
  const [naturalSize, setNaturalSize] = useState<{ w: number; h: number } | null>(null);
  const closeBtnRef = useRef<HTMLButtonElement>(null);
  const dragRef = useRef<{ id: number; sx: number; sy: number; bx: number; by: number } | null>(null);

  const current = items[idx] ?? items[0];
  const atStart = idx <= 0;
  const atEnd = idx >= items.length - 1;

  const resetView = useCallback(() => {
    setScale(1);
    setOffset({ x: 0, y: 0 });
    setDragging(false);
  }, []);

  const stepTo = useCallback(
    (next: number) => {
      const clamped = Math.max(0, Math.min(next, items.length - 1));
      if (clamped === idx) return;
      setIdx(clamped);
      setNaturalSize(null);
      resetView();
    },
    [idx, items.length, resetView]
  );

  const adjustScale = useCallback((delta: number) => {
    setScale((s) => {
      const next = Math.max(1, Math.min(3, Number((s + delta).toFixed(2))));
      if (next === 1) setOffset({ x: 0, y: 0 });
      return next;
    });
  }, []);

  useEffect(() => {
    const triggerEl = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    closeBtnRef.current?.focus({ preventScroll: true });
    return () => {
      document.body.style.overflow = prevOverflow;
      triggerEl?.focus({ preventScroll: true });
    };
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.preventDefault(); e.stopImmediatePropagation(); onClose(); }
      else if (e.key === "ArrowLeft") { e.preventDefault(); if (!atStart) stepTo(idx - 1); }
      else if (e.key === "ArrowRight") { e.preventDefault(); if (!atEnd) stepTo(idx + 1); }
      else if (e.key === "+" || e.key === "=") { e.preventDefault(); adjustScale(0.5); }
      else if (e.key === "-") { e.preventDefault(); adjustScale(-0.5); }
      else if (e.key === "0") { e.preventDefault(); resetView(); }
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [atStart, atEnd, idx, onClose, adjustScale, resetView, stepTo]);

  if (!current || typeof document === "undefined") return null;

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.currentTarget.setPointerCapture(e.pointerId);
    dragRef.current = { id: e.pointerId, sx: e.clientX, sy: e.clientY, bx: offset.x, by: offset.y };
    setDragging(true);
  };

  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    const d = dragRef.current;
    if (!d || d.id !== e.pointerId) return;
    const dx = e.clientX - d.sx;
    setOffset(scale > 1 ? { x: d.bx + dx, y: d.by + (e.clientY - d.sy) } : { x: computeSwipeOffset(dx, idx, items.length), y: 0 });
  };

  const finishDrag = (e: PointerEvent<HTMLDivElement>) => {
    const d = dragRef.current;
    if (!d || d.id !== e.pointerId) return;
    const dx = e.clientX - d.sx;
    dragRef.current = null;
    setDragging(false);
    if (scale === 1) {
      if (dx <= -SWIPE_COMMIT_PX && !atEnd) { stepTo(idx + 1); return; }
      if (dx >= SWIPE_COMMIT_PX && !atStart) { stepTo(idx - 1); return; }
      setOffset({ x: 0, y: 0 });
    }
  };

  return createPortal(
    <div
      role="dialog"
      aria-modal="true"
      aria-label={current.name}
      className="fixed inset-0 z-50 flex flex-col select-none text-white"
      style={{ background: "rgba(0,0,0,0.88)" }}
      onClick={onClose}
    >
      <div className="h-12 px-4 flex items-center justify-between gap-2 shrink-0" onClick={(e) => e.stopPropagation()}>
        <span className="font-mono-code text-xs opacity-80">{idx + 1} / {items.length}</span>
        <div className="flex items-center gap-1">
          <button type="button" onClick={() => adjustScale(-0.5)} disabled={scale <= 1} title="缩小 (-)" className={BTN_CLS}><Minus className="w-4 h-4" /></button>
          <button type="button" onClick={resetView} title="重置缩放 (0)" className="px-2 h-8 rounded-lg font-mono-code text-xs hover:bg-white/10 cursor-pointer inline-flex items-center gap-1">
            <RotateCcw className="w-3 h-3" /><span>{Math.round(scale * 100)}%</span>
          </button>
          <button type="button" onClick={() => adjustScale(0.5)} disabled={scale >= 3} title="放大 (+)" className={BTN_CLS}><Plus className="w-4 h-4" /></button>
          <button ref={closeBtnRef} type="button" onClick={onClose} title="关闭 (Esc)" className={`${BTN_CLS} ml-1`}><X className="w-4 h-4" /></button>
        </div>
      </div>

      <div
        className="relative flex-1 min-h-0 flex items-center justify-center overflow-hidden px-12"
        style={{ touchAction: "none" }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={finishDrag}
        onPointerCancel={finishDrag}
        onDoubleClick={(e) => { e.stopPropagation(); if (scale > 1) resetView(); else { setScale(2); setOffset({ x: 0, y: 0 }); } }}
      >
        {items.length > 1 && (
          <>
            <button type="button" disabled={atStart} onPointerDown={(e) => e.stopPropagation()} onClick={(e) => { e.stopPropagation(); stepTo(idx - 1); }} title="上一张" className={`${NAV_CLS} left-3`} style={{ background: "rgba(0,0,0,0.55)" }}>
              <ChevronLeft className="w-5 h-5" />
            </button>
            <button type="button" disabled={atEnd} onPointerDown={(e) => e.stopPropagation()} onClick={(e) => { e.stopPropagation(); stepTo(idx + 1); }} title="下一张" className={`${NAV_CLS} right-3`} style={{ background: "rgba(0,0,0,0.55)" }}>
              <ChevronRight className="w-5 h-5" />
            </button>
          </>
        )}
        <img
          key={current.id}
          src={current.src}
          alt={current.name}
          draggable={false}
          onClick={(e) => e.stopPropagation()}
          onLoad={(e) => setNaturalSize({ w: e.currentTarget.naturalWidth, h: e.currentTarget.naturalHeight })}
          style={{ transform: `translate3d(${offset.x}px, ${offset.y}px, 0) scale(${scale})`, transition: dragging ? "none" : "transform 180ms ease-out" }}
          className="max-w-full max-h-full object-contain rounded-lg"
        />
      </div>

      <div className="px-4 py-2.5 flex items-center justify-between gap-3 text-xs shrink-0" style={{ background: "rgba(0,0,0,0.72)" }} onClick={(e) => e.stopPropagation()}>
        <div className="min-w-0 flex items-center gap-2 font-mono-code">
          <span className="truncate font-medium">{current.name}</span>
          {naturalSize && naturalSize.w > 0 && <span className="opacity-70 shrink-0">{naturalSize.w}×{naturalSize.h}</span>}
          {current.size ? <span className="opacity-70 shrink-0">{formatByteSize(current.size)}</span> : null}
        </div>
        {current.downloadUrl && (
          <a href={current.downloadUrl} className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-white hover:bg-white/15 shrink-0" style={{ border: "1px solid rgba(255,255,255,0.22)" }}>
            <Download className="w-3.5 h-3.5" /><span>下载原件</span>
          </a>
        )}
      </div>
    </div>,
    document.body
  );
}
