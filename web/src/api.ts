// The backend's JSON, as the pages read it. Mirrors the Go types; Go's zero time
// arrives as "0001-01-01T00:00:00Z" (see isZero) and nil slices as null.

export type Agent = { vendor: string; model: string; effort: string };

export type Tracked = {
  repo: string; number: number; url: string; title: string; author: string;
  head_ref: string; head_sha: string; synced_sha: string; worktree: string;
  created_worktree: boolean; created_branch: boolean;
  tracked_at: string; last_seen_at: string;
  conflict_head_sha?: string; last_error?: string; design_review?: boolean;
  design?: DesignState; // set for design reviews
};

// DesignState is where your own review of a design PR stands (tracker.DesignState).
export type DesignState = {
  reviewed_at: string; reviewed_sha?: string; verdict?: string; new_commits?: boolean; replies?: number;
};

export type Posted = { url?: string; event?: string; comments?: number; replies?: number; fallback?: boolean; error?: string };
export type ClaudeResult = { subtype: string; is_error: boolean; num_turns: number; cost_usd: number; duration_ms: number; error?: string };
export type StackRun = { project: string; dir: string; offset?: number };

export type Run = {
  id: string;
  job: { pr: Tracked; agent: Agent; reason: string; attempt?: number };
  image: string; container?: string; container_name?: string;
  started: string; finished: string; status: string; exit_code: number;
  error?: string; claude?: ClaudeResult; posted?: Posted; stack?: StackRun;
  post_note?: string; overdue?: boolean;
};

export type PRStatus = { state: "" | "queued" | "running"; runs: Run[] | null };

export type PRView = Tracked & {
  id: string; health: string; review: PRStatus; review_note: string; up_to_date: boolean;
  verdict: string; tone: string; group: string; verdict_at: string;
  agent: Agent; overrides_agent: boolean;
};

export type Activity = { pr: string; run: string; title: string; label: string; tone: string; detail: string; at: string };
export type ActiveReview = { pr: string; run: string; reason: string; container: string; container_name: string; started: string; overdue: boolean };
export type Budget = { share: number; used: number; account: number; resets_at: string };
export type Ignored = { repo: string; number: number; title: string; at: string };

export type Check = { Name: string; Workflow: string; Status: string; Conclusion: string; URL: string };
export type Approval = { user: string; commit: string; stale: boolean; bot?: boolean };
export type Failure = { name: string; url?: string; fingerprint: string; flake?: boolean };
export type Snapshot = {
  head: string; author: string; draft: boolean; mergeable: string; merge_state: string; review_decision: string;
  approvals?: Approval[]; auto_merge_allowed?: boolean; in_queue?: boolean; merge_queue?: boolean; auto_merge?: boolean; node_id?: string; unanswered?: number; requested?: string[]; checks?: Check[]; ci: string; base: string; behind: number;
  additions: number; deletions: number; files?: string[]; failures?: Failure[];
  unresolved: number; new_feedback?: { id: string; kind: string; author: string; bot: boolean }[];
};
export type Signals = {
  additions: number; deletions: number; files: number; risky_paths: string[] | null; fix_rounds: Record<string, number> | null;
  approvers: { user: string; bot: boolean }[] | null; time_in_review_sec: number; flake_reruns: number;
  unresolved: number; behind: number; approvals_stale: boolean;
};
export type Verdict = { at: string; head: string; would_merge: boolean; blockers: string[] | null; signals: Signals };
export type DirtyFile = { path: string; code: string; add: number; del: number; binary?: boolean };
export type Dirty = { status: string; files: DirtyFile[]; summary?: string; message?: string; err?: string; dismissed?: boolean };
export type HandoffEvent = { time: string; kind: string; text: string };

export type Handoff = {
  id: string; repo: string; branch: string; worktree: string; base: string; title: string;
  reviewers?: string[]; reviewers_set?: boolean; reviewers_pending?: boolean; requested?: string[]; removed?: string[];
  autonomy: { fix_ci: boolean; address_reviews: boolean; merge: string };
  max_fix_rounds: number; fix_rounds?: Record<string, number>;
  merge_requested?: boolean; auto_merge_set?: boolean; queue_kicks?: number; enqueued_at?: string;
  state: string; prev_state?: string; reason?: string; error?: string; warning?: string;
  pr_number?: number; pr_url?: string;
  events?: HandoffEvent[]; snapshot?: Snapshot; dirty?: Dirty; shadow?: Verdict;
  created: string; updated: string;
};

export type HandoffView = Handoff & {
  over: boolean; can_merge?: boolean; reviewing: string[] | null; depth: number; waiting_on?: string; dirty_hash?: string;
  agent_running?: string; agent_started?: string;
};

export type PRGroup = { label: string; prs: PRView[] };

// Status is GET /api/status (and each /api/status/stream event): the app's state and what the board derives from it.
export type Status = {
  user: string; org: string; interval: string; running: boolean; last_run: string; next_run: string; last_error?: string;
  prs: PRView[] | null; reviews: boolean; runtime_error?: string; budget: Budget; budget_note?: string; budget_wait?: string;
  reviewing: ActiveReview[] | null; ignored: Ignored[] | null; activity: Activity[] | null; handoffs: HandoffView[] | null;
  slots_used: number; slots_max: number;
  groups: PRGroup[] | null; needs_human: HandoffView[] | null; failing: PRView[] | null; awaiting_you: PRView[] | null;
  needs_count: number; agents_running: HandoffView[] | null; merged: HandoffView[] | null; queued: number;
};

export type ReviewEvent = { line: number; kind: string; tool?: string; text: string; is_error?: boolean };
export type RunLog = { events: ReviewEvent[]; next: number };
export type ReviewFile = { event: string; body: string };
export type StackStatus = {
  project: string; state: string; volumes: string[] | null; networks: string[] | null;
  containers: { name: string; service: string; state: string; status: string; image: string }[] | null;
};

export type HistoryRow = {
  repo: string; number: number; url: string; title: string; author: string;
  tracked_at: string; dropped_at: string; reason: string; id: string; runs: Run[] | null;
};

export type ShadowPR = {
  handoff_id: string; repo: string; pr: number; head: string; verdict_at: string; would_merge: boolean;
  blockers: string[] | null; signals: Signals; outcome: string; outcome_at?: string; merged_by?: string;
};
export type ShadowLine = {
  time: string; handoff_id: string; repo: string; pr: number; head?: string; would_merge?: boolean;
  blockers?: string[]; signals?: Signals; outcome?: string; merged_by?: string;
};
export type Agreement = { merged: number; closed: number; open: number };
export type ShadowReport = {
  totals: { observed: number; merged: number; closed: number; open: number };
  agreement: { would_merge_yes: Agreement; would_merge_no: Agreement };
  mean_time_in_review_sec: number; prs: ShadowPR[]; history?: ShadowLine[];
};

export type Field = {
  key: string; type: "text" | "number" | "bool"; help: string; value: unknown; group: string; label: string;
  options?: string[]; suggest?: boolean; invert?: boolean;
};
export type Settings = { path: string; groups: string[]; fields: Field[] };
export type SkillStatus = { agents: { id: string; name: string; state: "absent" | "current" | "stale" }[] | null; busy?: string; error?: string };
export type LogEntry = { seq: number; time: string; level: string; msg: string; attrs?: string };

// session trades the one-time code the menu-bar app opens the page with (?code=) for an
// HttpOnly cookie, then drops it from the address bar. Every API call waits for it.
export const session: Promise<void> = (async () => {
  const url = new URL(location.href), code = url.searchParams.get("code");
  if (!code) return;
  await fetch("/api/session?code=" + encodeURIComponent(code)).catch(() => {});
  url.searchParams.delete("code");
  history.replaceState(null, "", url.pathname + url.search + url.hash);
})();

export class HTTPError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export async function call<T>(method: string, url: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  await session;
  const res = await fetch(url, {
    method,
    headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new HTTPError(res.status, (await res.text()).trim() || res.statusText);
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export const get = <T>(url: string) => call<T>("GET", url);
export const post = <T = void>(url: string, body?: unknown) => call<T>("POST", url, body);
export const put = <T = void>(url: string, body?: unknown) => call<T>("PUT", url, body);

// prAPI is a PR's API base: "/api/prs/owner/repo/n".
export const prAPI = (p: { repo: string; number: number }) => `/api/prs/${p.repo}/${p.number}`;
