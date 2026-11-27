import type { AgentView, WeComFocusView } from "../types/api";

export type IdentityPane = Pick<AgentView, "pane_id" | "name" | "topic" | "workspace_label" | "kind" | "is_agent">;

export type IdentityRoute =
  | { view: "corgi"; thread: string }
  | { view: "agent"; paneId: string }
  | { view: "auto" };

export interface PaneHandleDisplay {
  title: string;
  handle: string;
  showHandle: boolean;
}

export interface MobileAgentBarModel {
  title: string;
  badge: string;
  isCorgiBrain: boolean;
  ended: boolean;
}

export interface AgentComposerContextModel {
  stripDesktop: string;
  stripMobile: string;
  backLabel: string;
  placeholder: string;
  badge: string;
  isCorgiBrain: boolean;
}

export function isCorgiRouterPane(
  pane: Pick<AgentView, "pane_id" | "name" | "workspace_label"> | null | undefined,
  routerPaneId?: string
): boolean {
  if (!pane) return false;
  if (routerPaneId && pane.pane_id === routerPaneId) return true;
  return pane.name === "corgi" || pane.workspace_label === "corgi · router";
}

export function isInternalCorgiPane(
  pane: Pick<AgentView, "name" | "workspace_label"> | null | undefined
): boolean {
  if (!pane) return false;
  return (
    pane.name === "corgi" ||
    pane.name === "corgi-dream" ||
    pane.workspace_label === "corgi · router" ||
    pane.workspace_label === "corgi · dream" ||
    Boolean(pane.workspace_label?.startsWith("corgi · dream · "))
  );
}

export function formatPaneHandle(
  pane: Pick<AgentView, "name" | "topic" | "kind" | "is_agent">
): PaneHandleDisplay {
  const rawName = (pane.name ?? "").trim();
  const rawTopic = (pane.topic ?? "").trim();
  const handle = rawName ? (rawName.startsWith("#") ? rawName : `#${rawName}`) : "";
  const title = rawTopic || rawName || (pane.is_agent ? pane.kind || "会话" : "终端");
  const showHandle = Boolean(
    rawTopic &&
      rawName &&
      rawTopic.toLowerCase() !== rawName.toLowerCase() &&
      !rawTopic.includes(handle)
  );
  return { title, handle, showHandle };
}

export function formatDocumentTitle(
  route: IdentityRoute,
  selectedPane: IdentityPane | null | undefined,
  routerPaneId?: string
): string {
  if (route.view !== "agent") {
    return "Corgi";
  }
  if (!selectedPane) {
    return "会话已结束 · Corgi";
  }
  if (isCorgiRouterPane(selectedPane, routerPaneId)) {
    return "Corgi 底层终端 · Corgi";
  }
  const rawName = (selectedPane.name ?? "").trim();
  const handle = rawName ? (rawName.startsWith("#") ? rawName : `#${rawName}`) : "";
  const label = handle || (selectedPane.topic ?? "").trim() || selectedPane.kind || "会话";
  return `直聊 · ${label} · Corgi`;
}

export function formatMobileAgentBar(
  pane: IdentityPane | null | undefined,
  routerPaneId?: string
): MobileAgentBarModel {
  if (!pane) {
    return {
      title: "会话已结束",
      badge: "",
      isCorgiBrain: false,
      ended: true,
    };
  }
  if (isCorgiRouterPane(pane, routerPaneId)) {
    return {
      title: "Corgi 底层终端",
      badge: "调试",
      isCorgiBrain: true,
      ended: false,
    };
  }
  const { title, handle } = formatPaneHandle(pane);
  const rawTopic = (pane.topic ?? "").trim();
  return {
    title: rawTopic || handle || title,
    badge: pane.is_agent ? "直聊" : "终端",
    isCorgiBrain: false,
    ended: false,
  };
}

export function formatAgentComposerContext(
  pane: IdentityPane,
  opts: { routerPaneId?: string; mode: "chat" | "screen" | "log"; busy?: boolean }
): AgentComposerContextModel {
  if (isCorgiRouterPane(pane, opts.routerPaneId)) {
    return {
      stripDesktop: "Corgi 底层终端 · 调试视图，日常对话请回 Corgi",
      stripMobile: "Corgi 底层终端 · 调试",
      backLabel: "回 Corgi",
      placeholder: opts.mode === "chat" ? "向 Corgi 底层终端发送原始输入…" : "输入底层终端命令…",
      badge: "调试",
      isCorgiBrain: true,
    };
  }
  const { title, handle, showHandle } = formatPaneHandle(pane);
  const desktopTarget = showHandle ? `${title} (${handle})` : title;
  const placeholder =
    pane.is_agent && opts.mode === "chat"
      ? opts.busy
        ? `发给 ${title}（运行中自动排队）…`
        : `发给 ${title}（支持粘贴或拖入文件）…`
      : "输入终端命令…";
  return {
    stripDesktop: pane.is_agent ? `正在直聊 · ${desktopTarget}` : `正在操作终端 · ${desktopTarget}`,
    stripMobile: pane.is_agent ? `直聊 · ${title}` : `终端 · ${title}`,
    backLabel: "回 Corgi",
    placeholder,
    badge: pane.is_agent ? "直聊" : "终端",
    isCorgiBrain: false,
  };
}

export function formatAgentSpeakerLabel(opts: {
  name?: string;
  kind?: string;
  machine?: string;
  isAgent?: boolean;
  isCorgiBrain?: boolean;
}): string {
  const name = (opts.name ?? "").trim();
  const kind = (opts.kind ?? "").trim();
  const machine = (opts.machine ?? "").trim();
  if (opts.isCorgiBrain) {
    return ["Corgi 底层终端", kind].filter(Boolean).join(" · ");
  }
  const parts: string[] = [];
  if (name) parts.push(name);
  if (kind && kind.toLowerCase() !== name.toLowerCase()) parts.push(kind);
  if (machine) parts.push(machine);
  if (parts.length > 0) return parts.join(" · ");
  return opts.isAgent === false ? "终端" : "Agent";
}

export function formatWeComFocusBanner(
  focus: Pick<WeComFocusView, "worker" | "title"> | null | undefined
): string {
  if (!focus || !focus.worker.trim()) return "";
  const worker = focus.worker.trim();
  const handle = worker.startsWith("#") ? worker : `#${worker}`;
  const title = (focus.title ?? "").trim();
  if (title && title.toLowerCase() !== worker.toLowerCase() && !title.includes(handle)) {
    return `企微正在直聊 · ${title} (${handle})`;
  }
  return `企微正在直聊 · ${handle}`;
}
