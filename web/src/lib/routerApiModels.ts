export const TOKENHUB_BASE_URL = "https://tokenhub.tencentmaas.com/v1";

export interface RouterAPIModelSpec {
  id: string;
  label: string;
  efforts: readonly string[];
  defaultEffort: string;
}

export interface RouterAPIEffortOption {
  id: string;
  label: string;
}

const EFFORT_LABELS: Record<string, string> = {
  "": "不指定（模型默认）",
  no_think: "no_think（关闭思考）",
  low: "low（轻量思考）",
  medium: "medium（中等思考）",
  high: "high（深度思考）",
  max: "max（最高思考）",
};

export const TOKENHUB_MODELS: readonly RouterAPIModelSpec[] = [
  {
    id: "hy4",
    label: "hy4（混元 4 · 图文 1M）",
    efforts: ["high", "no_think"],
    defaultEffort: "high",
  },
  {
    id: "glm-5.3",
    label: "glm-5.3",
    efforts: ["low", "high", "max"],
    defaultEffort: "low",
  },
  {
    id: "glm-5.3-flash",
    label: "glm-5.3-flash（图文）",
    efforts: ["low", "high", "max"],
    defaultEffort: "low",
  },
  {
    id: "kimi-k3",
    label: "kimi-k3（图文 1M）",
    efforts: ["low", "high", "max"],
    defaultEffort: "low",
  },
];

const CUSTOM_OPENAI_EFFORTS = ["", "low", "medium", "high", "max", "no_think"] as const;

export function findTokenHubModel(modelId: string): RouterAPIModelSpec | undefined {
  const id = modelId.trim();
  return TOKENHUB_MODELS.find((m) => m.id === id);
}

export function getRouterAPIEffortOptions(provider: string, modelId: string): RouterAPIEffortOption[] {
  if (provider === "tokenhub") {
    const spec = findTokenHubModel(modelId);
    if (spec) {
      return spec.efforts.map((id) => ({ id, label: EFFORT_LABELS[id] ?? id }));
    }
  }
  return CUSTOM_OPENAI_EFFORTS.map((id) => ({ id, label: EFFORT_LABELS[id] ?? id }));
}

export function resolveRouterAPIEffort(provider: string, modelId: string, currentEffort?: string): string {
  const cur = (currentEffort ?? "").trim();
  if (provider === "tokenhub") {
    const spec = findTokenHubModel(modelId);
    if (spec) {
      return spec.efforts.includes(cur) ? cur : spec.defaultEffort;
    }
  }
  return cur;
}
