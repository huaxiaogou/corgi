import { useEffect, useRef, useState, type DragEvent, type FormEvent } from "react";
import { ArrowUp, Paperclip, Square, X } from "lucide-react";
import { useAutoResizeTextarea } from "../../hooks/useAutoResizeTextarea";
import { api, uploadFile } from "../../lib/api";
import { createIMETracker, classifyEnterKey } from "../../lib/composerInput";
import { getSupportedEfforts } from "../../lib/models";
import type { Entry, Machine, ModelCatalog } from "../../types/api";

const ALL_KINDS = ["claude", "codex", "agy", "codebuddy"];
const IMG_EXT_RE = /\.(png|jpe?g|gif|webp)$/i;

function createClientMsgId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `cm-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

interface Props {
  machines: Machine[];
  replyTo: Entry | null;
  working?: boolean;
  stopping?: boolean;
  queuedCount?: number;
  embedded?: boolean;
  onStop?: () => void;
  onClearReply: () => void;
  onSend: (payload: {
    text: string;
    machine: string;
    kind: string;
    model: string;
    effort: string;
    clientMsgId: string;
  }) => Promise<void>;
  onToast: (msg: string) => void;
}

export function ComposerBar({
  machines,
  replyTo,
  working = false,
  stopping = false,
  queuedCount = 0,
  embedded = false,
  onStop,
  onClearReply,
  onSend,
  onToast,
}: Props) {
  const [text, setText] = useState("");
  const [machine, setMachine] = useState("");
  const [kind, setKind] = useState("");
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [attachments, setAttachments] = useState<{ name: string; path: string }[]>([]);
  const [dragging, setDragging] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const imeRef = useRef(createIMETracker());
  const submittingRef = useRef(false);
  const clientMsgIdRef = useRef("");

  useAutoResizeTextarea(textareaRef, text, { minHeight: 24, maxHeight: 180 });

  useEffect(() => {
    if (replyTo) {
      textareaRef.current?.focus();
    }
  }, [replyTo?.id]);

  const selectedMachine = machines.find((m) => (m.id || "local") === machine || (machine === "" && m.local));
  const availableKinds =
    selectedMachine && selectedMachine.agents && selectedMachine.agents.length > 0 ? selectedMachine.agents : ALL_KINDS;

  useEffect(() => {
    if (kind && !availableKinds.includes(kind)) {
      setKind("");
      setModel("");
      setEffort("");
    }
  }, [machine, kind, availableKinds]);

  useEffect(() => {
    if (!kind) {
      setCatalog(null);
      setModel("");
      return;
    }
    const qs = new URLSearchParams({ kind });
    if (machine && machine !== "local") qs.set("machine", machine);
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then((c) => {
        setCatalog(c);
        setModel((cur) => (c.models.some((m) => m.id === cur) ? cur : ""));
      })
      .catch(() => setCatalog(null));
  }, [kind, machine]);

  const efforts = getSupportedEfforts(catalog, model);
  const hideEffort = Boolean(kind && catalog && efforts.length === 0);

  useEffect(() => {
    if (effort && (hideEffort || (efforts.length > 0 && !efforts.includes(effort)))) {
      setEffort("");
    }
  }, [effort, hideEffort, efforts.join(",")]);

  const handleFiles = async (files: FileList | File[] | null) => {
    if (!files) return;
    const targetMachine = machine === "local" ? "" : machine;
    for (const f of Array.from(files)) {
      try {
        const up = await uploadFile(f, targetMachine);
        setAttachments((prev) => [...prev, { name: up.name, path: up.path }]);
      } catch (err) {
        onToast(err instanceof Error ? err.message : "上传失败");
      }
    }
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (e.dataTransfer?.files?.length) {
      void handleFiles(e.dataTransfer.files);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (submittingRef.current) return;
    let payload = text.trim();
    if (!payload && attachments.length === 0) return;
    if (attachments.length > 0) {
      payload = (payload ? payload + "\n\n" : "") + "附件：\n" + attachments.map((a) => a.path).join("\n");
    }
    if (!clientMsgIdRef.current) {
      clientMsgIdRef.current = createClientMsgId();
    }
    const msgId = clientMsgIdRef.current;
    submittingRef.current = true;
    setSubmitting(true);
    try {
      await onSend({ text: payload, machine: machine === "local" ? "" : machine, kind, model, effort, clientMsgId: msgId });
      setText("");
      setAttachments([]);
      clientMsgIdRef.current = "";
    } catch {
      // Keep draft, attachments, replyTo, and the same clientMsgId so retrying is lossless and idempotent.
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  const machineQuery = machine && machine !== "local" ? `&machine=${encodeURIComponent(machine)}` : "";

  const form = (
      <form
        onSubmit={(e) => void submit(e)}
        onDragOver={(e) => {
          e.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
        className={`max-w-[740px] mx-auto composer-box rounded-2xl px-4 pt-3 pb-2.5 space-y-2 transition-colors ${dragging ? "border-strong surface-rail" : ""}`}
      >
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
            if (e.clipboardData?.files?.length) {
              e.preventDefault();
              void handleFiles(e.clipboardData.files);
            }
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
          placeholder={working || queuedCount > 0 ? "告诉 Corgi 你要做什么（运行中自动排队）…" : "告诉 Corgi 你要做什么（支持粘贴、拖入图片或文件）…"}
          className="block w-full resize-none bg-transparent text-[14px] leading-[1.5] min-h-[24px] max-h-[180px] overscroll-contain outline-none"
        />

        <div className="flex items-center justify-between pt-1.5 gap-2">
          <div className="flex items-center gap-2 text-xs text-muted flex-wrap">
            <button type="button" onClick={() => fileRef.current?.click()} className="text-secondary hover:opacity-80 cursor-pointer" title="上传图片或文件">
              <Paperclip className="w-4 h-4" strokeWidth={1.7} />
            </button>
            <input ref={fileRef} type="file" multiple hidden onChange={(e) => { void handleFiles(e.target.files); e.target.value = ""; }} />
            <span>派活</span>
            <select value={machine} onChange={(e) => setMachine(e.target.value)} className="bg-transparent text-secondary outline-none cursor-pointer">
              <option value="">默认电脑</option>
              {machines.map((m) => (
                <option key={m.id || "local"} value={m.id || "local"}>
                  {m.name}
                </option>
              ))}
            </select>
            <select value={kind} onChange={(e) => setKind(e.target.value)} className="bg-transparent text-secondary outline-none cursor-pointer">
              <option value="">自动 Agent</option>
              {availableKinds.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
            <select
              value={model}
              onChange={(e) => {
                const nextModel = e.target.value;
                const nextEfforts = getSupportedEfforts(catalog, nextModel);
                setModel(nextModel);
                if (effort && !nextEfforts.includes(effort)) {
                  setEffort("");
                }
              }}
              disabled={!kind}
              className="bg-transparent text-secondary outline-none cursor-pointer disabled:opacity-50 max-w-[140px] truncate"
            >
              <option value="">{kind ? (catalog?.default ? `默认 (${catalog.default})` : "默认模型") : "默认模型"}</option>
              {(catalog?.models ?? []).map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label || m.id}
                </option>
              ))}
            </select>
            {!hideEffort && (
              <select value={effort} onChange={(e) => setEffort(e.target.value)} className="bg-transparent text-secondary outline-none cursor-pointer">
                <option value="">默认思考</option>
                {efforts.map((ef) => (
                  <option key={ef} value={ef}>
                    思考 · {ef}
                  </option>
                ))}
              </select>
            )}
          </div>
          <div className="flex items-center gap-2 shrink-0">
            {working && onStop && (
              <button
                type="button"
                disabled={stopping}
                onClick={onStop}
                title="停止当前步骤 (Esc)"
                className="w-8 h-8 rounded-full border border-strong surface-rail text-primary flex items-center justify-center surface-hover cursor-pointer disabled:opacity-50"
              >
                <Square className="w-3 h-3 fill-current" />
              </button>
            )}
            <button
              type="submit"
              disabled={submitting}
              className="w-8 h-8 rounded-full btn-accent flex items-center justify-center cursor-pointer hover:opacity-85 shrink-0 disabled:opacity-50"
              title={working || queuedCount > 0 ? "加入待发送队列" : "发送"}
            >
              <ArrowUp className="w-4 h-4" strokeWidth={2} />
            </button>
          </div>
        </div>
      </form>
  );

  if (embedded) return form;
  return <div className="px-5 pb-4 pt-2 shrink-0">{form}</div>;
}
