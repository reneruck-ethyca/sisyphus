// Pure helpers for the AI's suggestions on the design page.
import { blocksInRange, buildComment } from "./anchor";
import type { Accept, DocFile, Suggestion, Suggestions, Thread } from "./api";
import { blocks } from "./render";

export type Filter = "ai" | "posted" | "all";

export const defaultFilter = (s: Suggestions): Filter => (s.items.some((x) => x.status === "open") ? "ai" : "posted");

export const isOutdated = (s: Suggestion, head: string) => s.head !== head;

// threadOf is the thread a reply answers: comment_id is a database id.
export const threadOf = (s: Suggestion, threads: Thread[]) =>
  threads.find((t) => t.comments.some((c) => String(s.comment_id) === c.databaseId));

// inDoc is the doc a suggestion shows on, or null when the page can't place it ("Outside the docs").
export function inDoc(s: Suggestion, docs: string[], threads: Thread[]): string | null {
  const path = s.kind === "reply" ? threadOf(s, threads)?.path : s.side !== "LEFT" ? s.path : undefined;
  return path && docs.includes(path) ? path : null;
}

// fromAI is the comments that accepted suggestions became.
export const fromAI = (items: Suggestion[]) =>
  new Set(items.filter((x) => (x.status === "review" || x.status === "posted") && x.github_id).map((x) => x.github_id!));

// draftOf is what accepting sends: a comment on the open doc placed as the composer would (doc is its
// rendered root), anything else as the AI wrote it. The server takes the side from the stored suggestion.
export function draftOf(s: Suggestion, body: string, docs: string[], threads: Thread[], file: DocFile | null, doc: ParentNode | null): Accept {
  if (s.kind === "reply") return { body };
  if (!s.line) return { body, subjectType: "FILE" };
  if (!file || !doc || inDoc(s, docs, threads) !== file.path) return { body, subjectType: "LINE", line: s.line, startLine: s.start_line };
  const range = { start: s.start_line ?? s.line, end: s.line };
  const quote = blocksInRange(blocks(doc), range.start, range.end).map((b) => b.el.textContent).join("\n");
  return buildComment(range, quote, body, file.hunks, file.headSha);
}
