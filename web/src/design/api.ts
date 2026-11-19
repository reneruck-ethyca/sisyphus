// The design page's calls: /api/prs/{owner}/{repo}/{n}/design. Mirrors backend/design.go.
import { HTTPError, call as request, type PRView } from "../api";

export type DocStatus = "added" | "modified" | "renamed";
export type Hunk = { start: number; end: number };
export type Pending = { id: string; count: number } | null;

export type DesignPR = {
  title: string; url: string; headSha: string; baseSha: string; headRepo: string; viewer: string;
  docs: { path: string; status: DocStatus }[];
  pending: Pending;
};

export type DocFile = {
  path: string; headSha: string; content: string; status: DocStatus;
  hunks: Hunk[]; addedLines: number[]; patchMissing: boolean;
  sinceLines: number[] | null; // lines changed after ?since=; null without one, or when GitHub can't compare
};

export type Comment = {
  id: string; databaseId: string; author: string; avatarUrl: string;
  bodyHTML: string; createdAt: string; url: string; state: "PENDING" | "SUBMITTED";
};

export type Thread = {
  id: string; path: string; subjectType: "LINE" | "FILE";
  startLine: number | null; line: number | null;
  originalStartLine: number | null; originalLine: number | null;
  isResolved: boolean; isOutdated: boolean;
  anchor: { startLine: number; line: number; sha: string } | null;
  comments: Comment[];
};

export type Mode = "review" | "now";
export type CreateThread = {
  path: string; body: string; subjectType: "LINE" | "FILE";
  startLine?: number; line?: number; mode: Mode;
};
export type Verdict = "COMMENT" | "APPROVE" | "REQUEST_CHANGES";

export type SuggestionStatus = "open" | "posting" | "review" | "posted" | "dismissed";
export type Suggestion = {
  id: string; run: string; head: string; kind: "comment" | "reply";
  path?: string; line?: number; start_line?: number; side?: string; comment_id?: number;
  body: string; status: SuggestionStatus; decided_at?: string; edited_body?: string; github_id?: string;
};
// Accept is where an accepted suggestion goes: a reply sends only its body.
export type Accept = { body: string; subjectType?: "LINE" | "FILE"; startLine?: number; line?: number };
export type Suggestions = { runs: { run: string; head: string; event: string; body: string; at: string }[]; items: Suggestion[] };

// askLabel is the "Ask the AI" button: disabled while a review is queued or running.
export function askLabel(pr: PRView): { text: string; disabled: boolean } {
  const state = pr.review.state;
  if (state === "running") return { text: "Review running", disabled: true };
  if (state === "queued") return { text: "Review queued", disabled: true };
  const again = !!pr.review.runs?.length;
  return { text: pr.design ? (again ? "Ask the AI again" : "Ask the AI for a review") : again ? "Review again" : "Review now", disabled: false };
}

// isStale: the 409 a write gets once commits landed after the page rendered.
export const isStale = (e: unknown) => e instanceof HTTPError && e.status === 409;

// designAPI binds the calls to one PR. head is the commit the page rendered: writes
// send it, and the server refuses them once the PR moved on.
export function designAPI(repo: string, number: number, head: () => string) {
  const base = `/api/prs/${repo}/${number}/design`;
  const call = <T>(method: string, url: string, body?: unknown) => request<T>(method, base + url, body, { "X-Head": head() });
  const q = (path: string) => "?path=" + encodeURIComponent(path);
  return {
    // blob is a file a doc links to, at the commit the doc was rendered from.
    blob: (path: string, ref: string) => `${base}/blob?path=${encodeURIComponent(path)}&ref=${encodeURIComponent(ref)}`,
    pr: () => call<DesignPR>("GET", ""),
    file: (path: string, since = "") => call<DocFile>("GET", "/file" + q(path) + (since ? "&since=" + encodeURIComponent(since) : "")),
    threads: (path = "") => call<Thread[]>("GET", "/threads" + (path ? q(path) : "")),
    createThread: (t: CreateThread) => call("POST", "/threads", t),
    reply: (t: Thread, body: string, mode: Mode) =>
      call("POST", `/threads/${encodeURIComponent(t.id)}/replies`, { body, mode, commentId: t.comments[0]?.databaseId }),
    setResolved: (id: string, resolved: boolean) =>
      call("POST", `/threads/${encodeURIComponent(id)}/${resolved ? "resolve" : "unresolve"}`),
    submit: (event: Verdict, body: string) => call("POST", "/review", { event, body }),
    discard: () => call("DELETE", "/review"),
    suggestions: () => call<Suggestions>("GET", "/suggestions"),
    accept: (id: string, p: Accept & { mode: Mode }) =>
      call("POST", `/suggestions/${encodeURIComponent(id)}/accept`, p),
    setSuggestion: (id: string, status: SuggestionStatus) => call("POST", `/suggestions/${encodeURIComponent(id)}/status`, { status }),
  };
}
export type DesignAPI = ReturnType<typeof designAPI>;
