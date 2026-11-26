function isASCIIAlpha(code: number): boolean {
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
}

function isASCIIAlphaNum(code: number): boolean {
  return isASCIIAlpha(code) || (code >= 48 && code <= 57);
}

function hasClosingBackticks(s: string, from: number, run: number): boolean {
  let i = from;
  while (i < s.length) {
    if (s[i] !== "`") {
      i++;
      continue;
    }
    let j = i;
    while (j < s.length && s[j] === "`") j++;
    if (j - i === run) return true;
    i = j;
  }
  return false;
}

function isWindowsDriveEscape(s: string, idx: number): boolean {
  if (idx < 2 || s[idx - 1] !== ":" || !isASCIIAlpha(s.charCodeAt(idx - 2))) return false;
  if (idx >= 3 && isASCIIAlphaNum(s.charCodeAt(idx - 3))) return false;
  const isCRLF = idx + 3 < s.length && s[idx + 1] === "r" && s[idx + 2] === "\\" && s[idx + 3] === "n";
  const next = isCRLF ? idx + 4 : idx + 2;
  if (next >= s.length) return false;
  const code = s.charCodeAt(next);
  return isASCIIAlphaNum(code) || s[next] === "_";
}

/**
 * Expands unescaped literal `\r\n` and `\n` into real LF when the text has no internal
 * real newlines (such as legacy replies sent via POSIX double-quoted CLI args).
 * Preserves real multiline content, inline backtick code spans, `\\n`, and Windows drive paths.
 */
export function normalizeEscapedNewlines(text: string): string {
  const trimmed = text.replace(/[\r\n]+$/, "");
  if (/[\r\n]/.test(trimmed) || !text.includes("\\n")) return text;

  let out = "";
  let inlineCodeTicks = 0;
  let i = 0;

  while (i < text.length) {
    if (text[i] === "`") {
      let j = i;
      while (j < text.length && text[j] === "`") j++;
      const run = j - i;
      if (inlineCodeTicks > 0) {
        if (run === inlineCodeTicks) inlineCodeTicks = 0;
      } else if (run < 3 && hasClosingBackticks(text, j, run)) {
        inlineCodeTicks = run;
      }
      out += text.slice(i, j);
      i = j;
      continue;
    }

    if (inlineCodeTicks === 0 && text[i] === "\\") {
      let j = i;
      while (j < text.length && text[j] === "\\") j++;
      if (j - i > 1) {
        out += text.slice(i, j);
        i = j;
        continue;
      }
      if (i + 3 < text.length && text[i + 1] === "r" && text[i + 2] === "\\" && text[i + 3] === "n" && !isWindowsDriveEscape(text, i)) {
        out += "\n";
        i += 4;
        continue;
      }
      if (i + 1 < text.length && text[i + 1] === "n" && !isWindowsDriveEscape(text, i)) {
        out += "\n";
        i += 2;
        continue;
      }
    }

    out += text[i];
    i++;
  }

  return out;
}
