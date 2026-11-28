import type { ReactNode } from "react";
import { normalizeEscapedNewlines } from "../lib/text";
import type { ArtifactSummary } from "../types/api";
import { CodeBlock } from "./markdown/CodeBlock";
import { DataTable } from "./markdown/DataTable";
import {
  renderInlineText,
  type InlineContext,
  type MarkdownImagePayload,
} from "./markdown/InlineText";
import { MermaidBlock } from "./markdown/MermaidBlock";
import { parseMarkdownBlocks, type MarkdownBlock } from "./markdown/parseBlocks";
import { CalloutBlock, MetricsStrip, StepsTimeline, TaskListItem } from "./markdown/RichBlocks";

export type { MarkdownImagePayload };

export interface MarkdownTextProps {
  text: string;
  defaultMachine?: string;
  entryArtifacts?: ArtifactSummary[];
  onOpenFile?: (path: string, machine?: string) => void;
  onOpenArtifact?: (art: ArtifactSummary) => void;
  onOpenImage?: (img: MarkdownImagePayload) => void;
}

function renderBlocks(blocks: MarkdownBlock[], ctx: InlineContext): ReactNode[] {
  return blocks.map((b, idx) => {
    switch (b.type) {
      case "code":
        if (b.lang.trim().toLowerCase() === "mermaid") {
          return <MermaidBlock key={idx} code={b.code} />;
        }
        return <CodeBlock key={idx} lang={b.lang} code={b.code} />;
      case "metrics":
        return <MetricsStrip key={idx} items={b.items} />;
      case "steps":
        return <StepsTimeline key={idx} items={b.items} renderText={(t) => renderInlineText(t, ctx)} />;
      case "callout":
        return (
          <CalloutBlock key={idx} kind={b.kind} title={b.title}>
            {b.blocks.length > 0 ? renderBlocks(b.blocks, ctx) : null}
          </CalloutBlock>
        );
      case "blockquote":
        return (
          <blockquote key={idx} className="my-2 pl-3 border-l-2 border-strong text-secondary space-y-1">
            {renderBlocks(b.blocks, ctx)}
          </blockquote>
        );
      case "details":
        return (
          <details key={idx} className="my-2 border-y border-hairline py-1.5">
            <summary className="cursor-pointer text-[13px] font-medium text-primary select-none hover:opacity-80">
              {renderInlineText(b.summary, ctx)}
            </summary>
            {b.blocks.length > 0 && (
              <div className="pt-2 pl-2 space-y-1.5 text-[13.5px] text-secondary">
                {renderBlocks(b.blocks, ctx)}
              </div>
            )}
          </details>
        );
      case "table":
        return <DataTable key={idx} headers={b.headers} rows={b.rows} numericCols={b.numericCols} inlineCtx={ctx} />;
      case "hr":
        return <hr key={idx} className="my-3 border-0 border-t border-hairline" />;
      case "heading": {
        const cls =
          b.level === 1
            ? "text-[16px] font-semibold mt-3 mb-1"
            : b.level === 2
              ? "text-[15px] font-semibold mt-2.5 mb-1"
              : "text-[14px] font-semibold mt-2";
        return (
          <div key={idx} className={cls}>
            {renderInlineText(b.text, ctx)}
          </div>
        );
      }
      case "list":
        if (b.ordered) {
          return (
            <ol key={idx} start={b.start} className="my-1.5 pl-5 space-y-1 list-decimal">
              {b.items.map((it, i) => (
                <li key={i} className="leading-[1.65]">
                  {renderInlineText(it.text, ctx)}
                </li>
              ))}
            </ol>
          );
        }
        return (
          <ul key={idx} className="my-1.5 pl-5 space-y-1 list-disc">
            {b.items.map((it, i) =>
              it.checked !== undefined ? (
                <TaskListItem key={i} checked={it.checked}>
                  {renderInlineText(it.text, ctx)}
                </TaskListItem>
              ) : (
                <li key={i} className="leading-[1.65]">
                  {renderInlineText(it.text, ctx)}
                </li>
              )
            )}
          </ul>
        );
      case "paragraph":
        return (
          <p key={idx} className="leading-[1.7] whitespace-pre-wrap">
            {renderInlineText(b.text, ctx)}
          </p>
        );
    }
  });
}

export function MarkdownText({
  text,
  defaultMachine,
  entryArtifacts,
  onOpenFile,
  onOpenArtifact,
  onOpenImage,
}: MarkdownTextProps) {
  const ctx: InlineContext = { defaultMachine, entryArtifacts, onOpenFile, onOpenArtifact, onOpenImage };
  const normalized = normalizeEscapedNewlines(text);
  const blocks = parseMarkdownBlocks(normalized);
  return <div className="space-y-1.5">{renderBlocks(blocks, ctx)}</div>;
}
