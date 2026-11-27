import type { Action, ActionChoice } from "../types/api";

export const DECISION_FOLD_LIMIT = 4;

export function isMultiAction(action: Action | undefined | null): boolean {
  if (!action || action.kind === "worker_dialog" || action.kind === "router_check") return false;
  return action.mode === "multi";
}

export function isFastSingleAction(action: Action | undefined | null): boolean {
  if (!action) return false;
  if (isMultiAction(action)) return false;
  return (action.choices?.length ?? 0) <= 2;
}

export function choiceKeyOf(choice: ActionChoice, index: number): string {
  const key = (choice.key ?? "").trim();
  return key || String(index + 1);
}

export function toggleChoiceSelection(multi: boolean, selected: string[], key: string): string[] {
  if (!key) return selected;
  if (!multi) return [key];
  return selected.includes(key) ? selected.filter((k) => k !== key) : [...selected, key];
}

export interface DecisionFoldState {
  visible: { choice: ActionChoice; index: number; key: string }[];
  folded: boolean;
  hiddenCount: number;
  hiddenSelectedCount: number;
}

export function computeDecisionFold(
  choices: ActionChoice[],
  expanded: boolean,
  selected: string[]
): DecisionFoldState {
  const indexed = choices.map((choice, index) => ({
    choice,
    index,
    key: choiceKeyOf(choice, index),
  }));
  const folded = !expanded && indexed.length > DECISION_FOLD_LIMIT;
  if (!folded) {
    return {
      visible: indexed,
      folded: false,
      hiddenCount: 0,
      hiddenSelectedCount: 0,
    };
  }
  const hidden = indexed.slice(DECISION_FOLD_LIMIT);
  const hiddenSelectedCount = hidden.filter((item) => selected.includes(item.key)).length;
  return {
    visible: indexed.slice(0, DECISION_FOLD_LIMIT),
    folded: true,
    hiddenCount: hidden.length,
    hiddenSelectedCount,
  };
}

export function formatDecisionConfirmLabel(action: Action, selected: string[]): string {
  const base = (action.confirm_label ?? "").trim() || "确认提交";
  if (selected.length === 0) return "请先选择上方选项";
  if (isMultiAction(action)) return `${base}（已选 ${selected.length} 项）`;
  const choices = action.choices ?? [];
  const matched = choices.find((ch, idx) => choiceKeyOf(ch, idx) === selected[0]);
  if (matched && !action.confirm_label) {
    const label = matched.label.trim();
    const clipped = label.length > 14 ? `${label.slice(0, 14)}…` : label;
    return `确认：${clipped}`;
  }
  return base;
}
