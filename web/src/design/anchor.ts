import type { Hunk, Thread } from "./api";

export type Range = { start: number; end: number };
type Lines = { s: number; e: number };

export function rangeOf(el: Element | null): Range | null {
  const b = el?.closest<HTMLElement>("[data-line-start]");
  return b ? { start: Number(b.dataset.lineStart), end: Number(b.dataset.lineEnd) } : null;
}

// Source range covered by a selection: innermost block at each end, widened to both.
export function selectionRange(sel: Selection, root: HTMLElement): Range | null {
  if (!sel.rangeCount || sel.isCollapsed) return null;
  const r = sel.getRangeAt(0);
  const el = (n: Node) => (n.nodeType === Node.ELEMENT_NODE ? (n as Element) : n.parentElement);
  const a = el(r.startContainer), b = el(r.endContainer);
  if (!a || !b || !root.contains(a) || !root.contains(b)) return null;
  const x = rangeOf(a), y = rangeOf(b);
  if (!x || !y) return null;
  return { start: Math.min(x.start, y.start), end: Math.max(x.end, y.end) };
}

// GitHub needs startLine and line in the same hunk: keep the largest overlap.
export function clamp(r: Range, hunks: Hunk[]): Range | null {
  let best: Range | null = null;
  for (const h of hunks) {
    const s = Math.max(r.start, h.start), e = Math.min(r.end, h.end);
    if (s <= e && (!best || e - s > best.end - best.start)) best = { start: s, end: e };
  }
  return best;
}

export function quoteMd(text: string): string {
  const t = text.trim();
  const short = t.length > 300 ? t.slice(0, 300) + "…" : t;
  return short.split("\n").map((l) => "> " + l).join("\n");
}

export type Draft = { subjectType: "LINE" | "FILE"; body: string; line?: number; startLine?: number };

export function buildComment(range: Range, quote: string, text: string, hunks: Hunk[], headSha: string): Draft {
  const c = clamp(range, hunks);
  if (c) {
    const partial = c.start !== range.start || c.end !== range.end;
    return {
      subjectType: "LINE",
      line: c.end,
      startLine: c.start !== c.end ? c.start : undefined,
      body: partial && quote.trim() ? `${quoteMd(quote)}\n\n${text}` : text,
    };
  }
  const q = quote.trim() ? quoteMd(quote) + "\n\n" : "";
  return {
    subjectType: "FILE",
    body: `${q}${text}\n\n<!-- design-review:L${range.start}-L${range.end}@${headSha.slice(0, 7)} -->`,
  };
}

export type Placement =
  | { section: "general" | "outdated" }
  | { section: "doc"; start: number; line: number; approximate: boolean };

export function placeThread(t: Thread, headSha: string): Placement {
  if (t.subjectType === "FILE") {
    return t.anchor
      ? { section: "doc", start: t.anchor.startLine, line: t.anchor.line, approximate: !headSha.startsWith(t.anchor.sha) }
      : { section: "general" };
  }
  if (t.line == null) return { section: "outdated" };
  return { section: "doc", start: t.startLine ?? t.line, line: t.line, approximate: false };
}

// Innermost block containing line; else the next block below (blank line, front matter); else the last.
export function blockForLine<B extends Lines>(bs: B[], line: number): B | undefined {
  const size = (b: B) => b.e - b.s;
  const inside = bs.filter((b) => b.s <= line && line <= b.e);
  if (inside.length) return inside.reduce((a, b) => (size(b) <= size(a) ? b : a));
  const below = bs.filter((b) => b.s > line);
  if (below.length) return below.reduce((a, b) => (b.s < a.s || (b.s === a.s && size(b) < size(a)) ? b : a));
  return bs.at(-1);
}

// Innermost blocks intersecting [start, end], for hover highlighting.
export function blocksInRange<B extends Lines & { el: Element }>(bs: B[], start: number, end: number): B[] {
  const hit = bs.filter((b) => b.s <= end && b.e >= start);
  return hit.filter((b) => !hit.some((o) => o !== b && b.el.contains(o.el)));
}
