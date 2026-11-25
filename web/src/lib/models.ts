import type { ModelCatalog } from "../types/api";

/**
 * Resolves the exact reasoning effort options supported by the chosen model.
 * - When a model explicitly defines `efforts` as an array (including `[]`), returns it verbatim.
 *   An empty array `[]` indicates the model has no adjustable reasoning effort (e.g. Orchestrator
 *   or fixed-tier presets), allowing the UI to hide the effort selector cleanly.
 * - When `modelId` is empty ("default model") or `m.efforts` is null/undefined, falls back to
 *   `catalog.efforts` (or `loadingFallback` while catalog is still loading).
 */
export function getSupportedEfforts(
  catalog: ModelCatalog | null,
  modelId: string,
  loadingFallback: string[] = ["low", "medium", "high"]
): string[] {
  if (!catalog) {
    return modelId ? [] : loadingFallback;
  }
  if (modelId) {
    const m = catalog.models.find((item) => item.id === modelId);
    if (m && Array.isArray(m.efforts)) {
      return m.efforts;
    }
  }
  return catalog.efforts ?? [];
}
