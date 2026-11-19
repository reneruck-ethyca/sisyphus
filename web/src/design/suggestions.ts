// Pure helpers for the AI's suggestions on the design page.
import type { Suggestion, Suggestions, Thread } from "./api";

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
