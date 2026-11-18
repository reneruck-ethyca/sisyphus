// Presentation helpers: how times, runs and handoff states read on the pages.
import type { HandoffView, Run } from "./api";

export const isZero = (t?: string) => !t || t.startsWith("0001-01-01");

// rel says how long ago (or from now) t is: "just now", "5 min ago", "in 2 h".
export function rel(t: string, now = Date.now()): string {
  const s = Math.round((now - new Date(t).getTime()) / 1000), a = Math.abs(s);
  const v = a < 60 ? "" : a < 3600 ? `${Math.round(a / 60)} min` : a < 86400 ? `${Math.round(a / 3600)} h` : `${Math.round(a / 86400)} d`;
  return !v ? (s < 0 ? "in a moment" : "just now") : s < 0 ? `in ${v}` : `${v} ago`;
}

const pad = (n: number) => String(n).padStart(2, "0");
const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// stamp is "2 Jan, 15:04"; when is "2006-01-02 15:04:05", or "never"; full is "2 Jan 2006, 15:04:05".
export function stamp(t: string): string {
  const d = new Date(t);
  return `${d.getDate()} ${months[d.getMonth()]}, ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
export function when(t?: string): string {
  if (isZero(t)) return "never";
  const d = new Date(t!);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
export function full(t: string): string {
  const d = new Date(t);
  return `${d.getDate()} ${months[d.getMonth()]} ${d.getFullYear()}, ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
export const clock = (t: string) => { const d = new Date(t); return `${pad(d.getHours())}:${pad(d.getMinutes())}`; };

export const short = (sha?: string) => (sha ?? "").slice(0, 8);
export const pct = (f: number) => `${Math.round(f * 100)}%`;
export const base = (p: string) => p.slice(p.lastIndexOf("/") + 1);
export const plural = (n: number, word: string, many = word + "s") => `${n} ${n === 1 ? word : many}`;

// gh passes a link to a GitHub page and drops any other: URLs come from GitHub, but the page doesn't trust them.
export const gh = (u?: string) => (u?.startsWith("https://github.com/") ? u : "");

// secs says a duration in its largest unit: "45 s", "12 min", "5.0 h", "2.5 d".
export function secs(s: number): string {
  if (s < 60) return `${Math.trunc(s)} s`;
  if (s < 3600) return `${Math.trunc(s / 60)} min`;
  if (s < 48 * 3600) return `${(s / 3600).toFixed(1)} h`;
  return `${(s / 86400).toFixed(1)} d`;
}

// outcome says in a word or two what a run came to: what was posted, not how the container exited.
export function outcome(r: Run): { label: string; tone: string } {
  switch (r.status) {
    case "running": return { label: "Reviewing", tone: "live" };
    case "cancelled": return { label: "Cancelled", tone: "neutral" };
    case "lost": return { label: "Interrupted", tone: "neutral" };
    case "succeeded": break;
    default: return { label: "Failed", tone: "bad" };
  }
  const p = r.posted;
  if (!p) return { label: "Not posted", tone: "neutral" };
  if (p.error) return { label: "Posting failed", tone: "bad" };
  if (p.event === "APPROVE") return { label: "Approved", tone: "ok" };
  if (p.event === "REQUEST_CHANGES") return { label: "Changes requested", tone: "changes" };
  if (p.event === "COMMENT") return { label: "Commented", tone: "neutral" };
  return { label: "Replied", tone: "neutral" };
}

const reasons: Record<string, string> = { new: "PR first seen", commits: "new commits", replies: "new replies", manual: "run by you" };
export const reason = (r: string) => reasons[r] || r;

// dur is how long a run took, or has been running.
export function dur(r: Run, now = Date.now()): string {
  const end = r.status === "running" || isZero(r.finished) ? now : new Date(r.finished).getTime();
  const s = Math.round((end - new Date(r.started).getTime()) / 1000);
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`;
  return `${Math.floor(s / 3600)} h ${Math.floor(s / 60) % 60} min`;
}

// handoffTone is the chip colour of a handoff state, handoffIcon a one-glyph cue matching it.
export function handoffTone(state: string): string {
  if (["ci-red", "conflict", "needs-human"].includes(state)) return "bad";
  if (["ci-green", "approved"].includes(state)) return "ok";
  if (state === "merged") return "merged";
  if (state === "queued") return "live";
  return state === "pr-open" ? "open" : "neutral";
}
export function handoffIcon(state: string): string {
  if (state === "merged") return "✓✓";
  if (["ci-green", "approved", "mergeable"].includes(state)) return "✓";
  if (["ci-red", "conflict"].includes(state)) return "✗";
  if (state === "needs-human") return "!";
  if (["closed", "cancelled"].includes(state)) return "⊘";
  if (["ci-running", "awaiting-review"].includes(state)) return "…";
  return "→";
}

// splitState splits a "state" timeline event, written as "state" or "state: reason".
export function splitState(text: string): { state: string; reason: string } {
  const i = text.indexOf(": ");
  return i < 0 ? { state: text, reason: "" } : { state: text.slice(0, i), reason: text.slice(i + 2) };
}

// reviewerChips tells who is asked to review: those TLM requested plain, ones a human added "human",
// and those a human removed "gone" (TLM won't re-add them).
export function reviewerChips(v: HandoffView): { login: string; cls: string }[] {
  const out = (v.reviewing ?? []).map((login) => ({
    login, cls: !(v.requested ?? []).includes(login) && v.snapshot ? "human" : "",
  }));
  for (const login of v.removed ?? []) if (!(v.reviewing ?? []).includes(login)) out.push({ login, cls: "gone" });
  return out;
}

// lmCount counts the handoffs in any of the states; none = every open one.
export const lmCount = (hs: HandoffView[], ...states: string[]) =>
  hs.filter((v) => (states.length ? states.includes(v.state) : !v.over)).length;

// The vendors' model families and reasoning efforts, for the agent pickers.
export const profiles: Record<string, { models: string[]; efforts: string[] }> = {
  claude: { models: ["opus", "sonnet", "haiku"], efforts: ["low", "medium", "high"] },
  codex: { models: ["gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5-codex"], efforts: ["low", "medium", "high", "xhigh", "max", "ultra"] },
};
// withCurrent keeps a saved value the list doesn't know, so an existing config is never silently changed.
export const withCurrent = (values: string[], current: string) => (current && !values.includes(current) ? [current, ...values] : values);

// fuzzy says every word of the query appears in the title, its letters in order ("rbc dflt" finds "RBAC the default").
export function fuzzy(query: string, title: string): boolean {
  const t = title.toLowerCase();
  return query.toLowerCase().split(/\s+/).filter(Boolean).every((w) => {
    let i = 0;
    for (const c of t) if (c === w[i]) i++;
    return i === w.length;
  });
}

// runPath links a review run's page from a PR id ("owner/repo#n").
export function runPath(prID: string, run: string, archive = false): string {
  const [repo, n] = prID.split("#");
  return `/${archive ? "history" : "prs"}/${repo}/${n}/runs/${run}`;
}
