import { useEffect, useRef, useState, type DragEvent, type FormEvent } from "react";
import { ArrowUp, ChevronDown, FolderGit2, Paperclip, Square, X } from "lucide-react";
import { useAutoResizeTextarea } from "../../hooks/useAutoResizeTextarea";
import { api, uploadFile } from "../../lib/api";
import { createIMETracker, classifyEnterKey } from "../../lib/composerInput";
import { formatDispatchSummary, resolveEffectiveDispatch } from "../../lib/dispatchDefaults";
import { getSupportedEfforts } from "../../lib/models";
import { capturePasteSnapshot, resolvePastedFiles } from "../../lib/pasteImage";
import type { Entry, Machine, MediaRef, ModelCatalog, WorkItem, WorkerDispatchDefaults } from "../../types/api";
import { DispatchOverridePopover } from "./DispatchOverridePopover";

const ALL_KINDS = ["agy", "codebuddy", "codex", "claude"];
const IMG_EXT_RE = /\.(png|jpe?g|gif|webp)$/i;

function createClientMsgId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `cm-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

interface Props {
  machines: Machine[];
  dispatchDefaults?: WorkerDispatchDefaults;
  replyTo: Entry | null;
  linkedWork?: WorkItem | null;
  draftSeed?: { seq: number; text: string } | null;
  working?: boolean;
  stopping?: boolean;
  disabled?: boolean;
  queuedCount?: number;
  embedded?: boolean;
  onStop?: () => void;
  onOpenSettings?: () => void;
  onClearReply: () => void;
  onClearLinkedWork?: () => void;
  onSend: (payload: {
    text: string;
    machine: string;
    kind: string;
    model: string;
    effort: string;
    projectId: string;
    media?: MediaRef[];
    clientMsgId: string;
  }) => Promise<void>;
  onToast: (msg: string) => void;
}

export function ComposerBar({
  machines,
  dispatchDefaults,
  replyTo,
  linkedWork = null,
  draftSeed = null,
  working = false,
  stopping = false,
  disabled = false,
  embedded = false,
  onStop,
  onOpenSettings,
  onClearReply,
  onClearLinkedWork,
  onSend,
  onToast,
}: Props) {
  const [text, setText] = useState("");
  const [machine, setMachine] = useState("");
  const [kind, setKind] = useState("");
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [projectId, setProjectId] = useState("");
  const [overrideOpen, setOverrideOpen] = useState(false);
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [attachments, setAttachments] = useState<{ name: string; path: string; machine?: string; mime?: string; size?: number }[]>([]);
  const [dragging, setDragging] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const imeRef = useRef(createIMETracker());
  const submittingRef = useRef(false);
  const clientMsgIdRef = useRef("");

  useAutoResizeTextarea(textareaRef, text, { minHeight: 24, maxHeight: 180 });

  useEffect(() => {
    if (replyTo) textareaRef.current?.focus({ preventScroll: true });
  }, [replyTo?.id]);

  useEffect(() => {
    if (!draftSeed) return;
    setText((cur) => (cur.trim() ? cur : draftSeed.text));
    requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    });
  }, [draftSeed?.seq]);

  useEffect(() => {
    if (!overrideOpen) return;
    const onDocClick = (e: MouseEvent) => {
      const t = e.target as Node | null;
      if (t && popoverRef.current && !popoverRef.current.contains(t) && triggerRef.current && !triggerRef.current.contains(t)) {
        setOverrideOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOverrideOpen(false);
    };
    document.addEventListener("mousedown", onDocClick);
    window.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocClick);
      window.removeEventListener("keydown", onKey);
    };
  }, [overrideOpen]);

  const resolved = resolveEffectiveDispatch(dispatchDefaults, machines, { machine, kind, model, effort });
  const effectiveBase = resolveEffectiveDispatch(dispatchDefaults, machines, { machine, kind });
  const effectiveMachineObj = machines.find((m) => (resolved.machine === "" ? m.local : m.id === resolved.machine));
  const availableKinds =
    effectiveMachineObj && effectiveMachineObj.agents && effectiveMachineObj.agents.length > 0 ? effectiveMachineObj.agents : ALL_KINDS;

  useEffect(() => {
    if (kind && !availableKinds.includes(kind)) {
      setKind("");
      setModel("");
      setEffort("");
    }
  }, [machine, kind, availableKinds]);

  useEffect(() => {
    if (!overrideOpen) return;
    const queryKind = resolved.kind;
    if (!queryKind) {
      setCatalog(null);
      return;
    }
    const qs = new URLSearchParams({ kind: queryKind });
    if (resolved.machine) qs.set("machine", resolved.machine);
    let cancelled = false;
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then((c) => {
        if (cancelled) return;
        setCatalog(c);
        setModel((cur) => (cur && c.models.some((m) => m.id === cur) ? cur : ""));
      })
      .catch(() => {
        if (!cancelled) setCatalog(null);
      });
    return () => {
      cancelled = true;
    };
  }, [overrideOpen, resolved.kind, resolved.machine]);

  const efforts = getSupportedEfforts(catalog, model || effectiveBase.model);
  const hideEffort = Boolean(catalog && efforts.length === 0);

  useEffect(() => {
    if (effort && (hideEffort || (efforts.length > 0 && !efforts.includes(effort)))) {
      setEffort("");
    }
  }, [effort, hideEffort, efforts.join(",")]);

  const clearOneShotOverride = () => {
    setMachine("");
    setKind("");
    setModel("");
    setEffort("");
  };

  const handleFiles = async (files: FileList | File[] | null) => {
    if (!files) return;
    const targetMachine = resolved.machine;
    for (const f of Array.from(files)) {
      try {
        const up = await uploadFile(f, targetMachine);
        setAttachments((prev) => [
          ...prev,
          { name: up.name, path: up.path, machine: targetMachine || "", mime: f.type || undefined, size: f.size },
        ]);
      } catch (err) {
        onToast(err instanceof Error ? err.message : "上传失败");
      }
    }
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (e.dataTransfer?.files?.length) void handleFiles(e.dataTransfer.files);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (disabled || submittingRef.current) return;
    let payload = text.trim();
    if (!payload && attachments.length === 0) return;
    if (attachments.length > 0) {
      payload = (payload ? payload + "\n\n" : "") + "附件：\n" + attachments.map((a) => a.path).join("\n");
    }
    if (!clientMsgIdRef.current) clientMsgIdRef.current = createClientMsgId();
    const msgId = clientMsgIdRef.current;
    const hadOverride = resolved.isOverride;
    submittingRef.current = true;
    setSubmitting(true);
    try {
      await onSend({
        text: payload,
        machine: machine === "local" ? "hub" : machine,
        kind,
        model,
        effort,
        projectId,
        media:
          attachments.length > 0
            ? attachments.map((a) => ({ path: a.path, machine: a.machine || undefined, name: a.name, mime: a.mime, size: a.size }))
            : undefined,
        clientMsgId: msgId,
      });
      setText("");
      setAttachments([]);
      clientMsgIdRef.current = "";
      setOverrideOpen(false);
      if (hadOverride) clearOneShotOverride();
    } catch {
      // Keep draft, attachments, replyTo, and the same clientMsgId so retrying is lossless and idempotent.
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  const projects = dispatchDefaults?.projects ?? [];
  const machineQuery = resolved.machine ? `&machine=${encodeURIComponent(resolved.machine)}` : "";
  const dispatchSummary = formatDispatchSummary(resolved);
  const baseModelDisplay = effectiveBase.model
    ? effectiveBase.effort
      ? `${effectiveBase.model} / ${effectiveBase.effort}`
      : effectiveBase.model
    : "本机默认";

  const form = (
    <div className="max-w-[740px] mx-auto relative">
      {overrideOpen && (
        <DispatchOverridePopover
          popoverRef={popoverRef}
          machines={machines}
          dispatchDefaults={dispatchDefaults}
          machine={machine}
          kind={kind}
          model={model}
          effort={effort}
          availableKinds={availableKinds}
          catalog={catalog}
          baseModelDisplay={baseModelDisplay}
          effectiveBaseModel={effectiveBase.model}
          effectiveBaseEffort={effectiveBase.effort}
          efforts={efforts}
          hideEffort={hideEffort}
          isOverride={resolved.isOverride}
          onChangeMachine={setMachine}
          onChangeKind={(v) => {
            setKind(v);
            setModel("");
            setEffort("");
          }}
          onChangeModel={setModel}
          onChangeEffort={setEffort}
          onClearOverride={clearOneShotOverride}
          onClose={() => setOverrideOpen(false)}
          onOpenSettings={onOpenSettings}
        />
      )}

      <form
        onSubmit={(e) => void submit(e)}
        onDragOver={(e) => {
          e.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
        className={`composer-box rounded-2xl px-4 pt-3 pb-2.5 space-y-2 transition-colors ${dragging ? "border-strong surface-rail" : ""}`}
      >
        {linkedWork && (
          <div className="flex items-center justify-between gap-2 text-xs px-2.5 py-1.5 rounded-lg border border-hairline surface-rail">
            <div className="flex items-center gap-1.5 min-w-0">
              <span className="text-muted shrink-0">关联</span>
              <span className="font-medium text-primary truncate">{linkedWork.title}</span>
            </div>
            {onClearLinkedWork && (
              <button type="button" onClick={onClearLinkedWork} className="text-muted hover:text-primary cursor-pointer shrink-0" title="取消关联">
                <X className="w-3.5 h-3.5" />
              </button>
            )}
          </div>
        )}

        {replyTo && (
          <div className="flex items-start justify-between gap-2 text-xs px-2.5 py-1.5 rounded-lg surface-rail text-secondary">
            <span className="line-clamp-2 break-words leading-relaxed">
              {replyTo.role === "user" && replyTo.action_id ? `补充说明（已选：${replyTo.text}）` : `引用：${replyTo.text}`}
            </span>
            <button type="button" onClick={onClearReply} className="cursor-pointer shrink-0 mt-0.5">
              <X className="w-3.5 h-3.5" />
            </button>
          </div>
        )}

        {attachments.length > 0 && (
          <div className="flex flex-wrap gap-2 text-xs">
            {attachments.map((att, idx) =>
              IMG_EXT_RE.test(att.path) ? (
                <div key={att.path} className="relative group">
                  <img
                    src={`/api/file?path=${encodeURIComponent(att.path)}&as=image${machineQuery}`}
                    alt={att.name}
                    className="h-14 w-14 object-cover rounded-lg border border-hairline"
                  />
                  <button
                    type="button"
                    onClick={() => setAttachments((p) => p.filter((_, i) => i !== idx))}
                    className="absolute -top-1.5 -right-1.5 w-4 h-4 rounded-full btn-accent flex items-center justify-center cursor-pointer"
                  >
                    <X className="w-2.5 h-2.5" />
                  </button>
                </div>
              ) : (
                <span key={att.path} className="inline-flex items-center gap-1 px-2 py-0.5 rounded surface-rail border border-hairline font-mono-code text-secondary">
                  {att.name}
                  <button type="button" onClick={() => setAttachments((p) => p.filter((_, i) => i !== idx))} className="text-muted cursor-pointer">
                    <X className="w-3 h-3" />
                  </button>
                </span>
              )
            )}
          </div>
        )}

        <textarea
          ref={textareaRef}
          rows={1}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onCompositionStart={() => {
            imeRef.current.composing = true;
          }}
          onCompositionEnd={(e) => {
            imeRef.current.composing = false;
            imeRef.current.lastCompositionEndAt = e.timeStamp;
          }}
          onPaste={(e) => {
            const snap = capturePasteSnapshot(e.clipboardData);
            if (!snap.hasMediaHint) return;
            e.preventDefault();
            void resolvePastedFiles(snap, navigator.clipboard).then((res) => {
              if (res.files.length > 0) void handleFiles(res.files);
              else if (res.error) onToast(res.error);
            });
          }}
          onKeyDown={(e) => {
            const action = classifyEnterKey(
              {
                key: e.key,
                shiftKey: e.shiftKey,
                ctrlKey: e.ctrlKey,
                metaKey: e.metaKey,
                altKey: e.altKey,
                keyCode: e.keyCode,
                isComposing: e.nativeEvent.isComposing,
                timeStamp: e.timeStamp,
              },
              imeRef.current
            );
            if (action === "swallow_safari_post_ime") {
              e.preventDefault();
              return;
            }
            if (action === "submit") {
              e.preventDefault();
              void submit(e);
            }
          }}
          placeholder="告诉 Corgi 你要做什么（支持粘贴、拖入图片或文件）…"
          className="block w-full resize-none bg-transparent text-[14px] leading-[1.5] min-h-[24px] max-h-[180px] overscroll-contain outline-none"
        />

        <div className="flex items-center justify-between pt-1.5 gap-2">
          <div className="flex items-center gap-1.5 text-xs text-muted flex-wrap min-w-0">
            <button
              type="button"
              onClick={() => fileRef.current?.click()}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer shrink-0"
              title="上传图片或文件"
            >
              <Paperclip className="w-4 h-4" strokeWidth={1.7} />
            </button>
            <input ref={fileRef} type="file" multiple hidden onChange={(e) => { void handleFiles(e.target.files); e.target.value = ""; }} />

            {projects.length > 0 && (
              <label className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full border border-hairline surface-rail text-[11.5px] text-secondary hover:text-primary cursor-pointer">
                <FolderGit2 className="w-3 h-3 text-muted shrink-0" />
                <select
                  value={projectId}
                  onChange={(e) => setProjectId(e.target.value)}
                  className="bg-transparent outline-none cursor-pointer max-w-[120px] truncate"
                  aria-label="绑定项目"
                >
                  <option value="">自动项目</option>
                  {projects.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </label>
            )}

            <button
              ref={triggerRef}
              type="button"
              onClick={() => setOverrideOpen((v) => !v)}
              className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full border text-[11.5px] cursor-pointer transition-colors max-w-full ${
                resolved.isOverride
                  ? "btn-accent border-transparent"
                  : "border-hairline surface-rail text-secondary hover:text-primary"
              }`}
              title={resolved.isOverride ? "点击修改或清除本次临时覆盖" : "当前遵循全局默认设置；点击可仅对本条消息临时覆盖"}
            >
              <span className="truncate">
                {resolved.isOverride ? "仅本次：" : "默认："}
                {dispatchSummary}
              </span>
              {resolved.isOverride ? (
                <span
                  role="button"
                  tabIndex={0}
                  onClick={(e) => {
                    e.stopPropagation();
                    clearOneShotOverride();
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.stopPropagation();
                      clearOneShotOverride();
                    }
                  }}
                  className="opacity-80 hover:opacity-100"
                  title="恢复全局默认"
                >
                  <X className="w-3 h-3" />
                </span>
              ) : (
                <ChevronDown className="w-3 h-3 opacity-60 shrink-0" />
              )}
            </button>
          </div>

          <div className="flex items-center gap-1.5 shrink-0">
            {working && onStop && (
              <button
                type="button"
                disabled={stopping || disabled}
                onClick={onStop}
                title="停止当前步骤 (Esc)"
                className="w-7 h-7 rounded-full border border-strong surface-rail text-primary surface-hover flex items-center justify-center cursor-pointer disabled:opacity-40"
              >
                <Square className="w-3 h-3 fill-current" />
              </button>
            )}
            <button
              type="submit"
              disabled={disabled || submitting || (!text.trim() && attachments.length === 0)}
              onPointerDown={(e) => {
                if (document.activeElement === textareaRef.current) e.preventDefault();
              }}
              title={disabled ? "正在同步实时状态…" : "发送"}
              className="w-7 h-7 rounded-full btn-accent flex items-center justify-center cursor-pointer disabled:opacity-30"
            >
              <ArrowUp className="w-4 h-4" strokeWidth={2} />
            </button>
          </div>
        </div>
      </form>
    </div>
  );

  if (embedded) return form;
  return <div className="px-5 pb-4 pt-2 shrink-0">{form}</div>;
}
