// The design page's calls: /api/prs/{owner}/{repo}/{n}/design. Mirrors backend/design.go.
import { HTTPError, session } from "../api";

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
export type ReviewEvent = "COMMENT" | "APPROVE" | "REQUEST_CHANGES";

// isStale: the 409 a write gets once commits landed after the page rendered.
export const isStale = (e: unknown) => e instanceof HTTPError && e.status === 409;

// designAPI binds the calls to one PR. head is the commit the page rendered: writes
// send it, and the server refuses them once the PR moved on.
export function designAPI(repo: string, number: number, head: () => string) {
  const base = `/api/prs/${repo}/${number}/design`;
  const call = async <T>(method: string, url: string, body?: unknown): Promise<T> => {
    await session;
    const res = await fetch(base + url, {
      method,
      headers: { "Content-Type": "application/json", "X-Head": head() },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) throw new HTTPError(res.status, (await res.text()).trim() || res.statusText);
    const text = await res.text();
    return (text ? JSON.parse(text) : undefined) as T;
  };
  const q = (path: string) => "?path=" + encodeURIComponent(path);
  return {
    blob: base + "/blob",
    pr: () => call<DesignPR>("GET", ""),
    file: (path: string, since = "") => call<DocFile>("GET", "/file" + q(path) + (since ? "&since=" + encodeURIComponent(since) : "")),
    threads: (path = "") => call<Thread[]>("GET", "/threads" + (path ? q(path) : "")),
    createThread: (t: CreateThread) => call("POST", "/threads", t),
    reply: (t: Thread, body: string, mode: Mode) =>
      call("POST", `/threads/${encodeURIComponent(t.id)}/replies`, { body, mode, commentId: t.comments[0]?.databaseId }),
    setResolved: (id: string, resolved: boolean) =>
      call("POST", `/threads/${encodeURIComponent(id)}/${resolved ? "resolve" : "unresolve"}`),
    submit: (event: ReviewEvent, body: string) => call("POST", "/review", { event, body }),
    discard: () => call("DELETE", "/review"),
  };
}
export type DesignAPI = ReturnType<typeof designAPI>;
