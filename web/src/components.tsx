// Pieces several pages share: the header, chips, ages, notices and the runs table.
import type { ComponentChildren } from "preact";
import { useEffect, useState } from "preact/hooks";
import { post, type Run, type SkillStatus, type Status, get } from "./api";
import { dur, full, gh, isZero, outcome, pct, reason, rel, short, stamp } from "./format";
import { useTick } from "./live";

export function Ago({ t }: { t?: string }) {
  useTick();
  if (isZero(t)) return <>never</>;
  return <time dateTime={t} title={full(t!)}>{rel(t!)}</time>;
}

export const Chip = ({ tone, children }: { tone?: string; children: ComponentChildren }) =>
  <span class={"chip" + (tone ? " " + tone : "")}>{children}</span>;

export function RunChip({ run }: { run: Run }) {
  const o = outcome(run);
  return <Chip tone={o.tone}>{o.label}</Chip>;
}

// Ext links a GitHub page in a new tab; anything that isn't GitHub renders as plain text.
export function Ext({ href, children, class: cls }: { href?: string; children: ComponentChildren; class?: string }) {
  const u = gh(href);
  if (!u) return <>{children}</>;
  return <a class={cls} href={u} target="_blank" rel="noopener">{children}</a>;
}

const mdPart = /`([^`\n]+)`|\*\*([^*\n]+)\*\*/g;

// Md renders a review's `code` and **bold**; the rest stays as written (the page keeps its line breaks).
export function Md({ text }: { text: string }) {
  const out: ComponentChildren[] = [];
  let last = 0;
  for (const m of text.matchAll(mdPart)) {
    out.push(text.slice(last, m.index));
    out.push(m[1] !== undefined ? <code>{m[1]}</code> : <b>{m[2]}</b>);
    last = m.index! + m[0].length;
  }
  out.push(text.slice(last));
  return <>{out}</>;
}

// BudgetMeter is the 5-hour budget meter; it turns red past three quarters of the share.
export const BudgetMeter = ({ b, label }: { b: Status["budget"]; label?: string }) =>
  <meter min={0} max={b.share} low={b.share * 0.75} high={b.share * 0.75} optimum={0} value={b.used} aria-label={label} />;

let skills: SkillStatus | undefined;
export function useSkills(): [SkillStatus | undefined, (s: SkillStatus) => void] {
  const [s, set] = useState(skills);
  useEffect(() => { if (!skills) get<SkillStatus>("/api/skill").then((v) => set((skills = v))).catch(() => {}); }, []); // no badge then
  return [s, (v) => set((skills = v))];
}

export function Top({ status, page }: { status?: Status; page?: string }) {
  const [sk] = useSkills();
  const [checking, setChecking] = useState(false);
  const needs = status?.needs_human?.length ?? 0;
  const tab = (href: string, name: string, label: string, extra?: ComponentChildren) =>
    <a href={href} aria-current={page === name ? "page" : undefined}>{label}{extra}</a>;
  const run = async () => {
    setChecking(true);
    await post("/api/run").catch(() => {});
    setTimeout(() => setChecking(false), 1000);
  };
  return <header class="top">
    <a class="brand" href="/">sisyphus</a>
    <nav>
      {tab("/reviews", "reviews", "Reviews")}
      {tab("/lastmile", "lastmile", "Last mile", needs > 0 && <span class="badge" aria-label={`${needs} need you`}>{needs}</span>)}
      {tab("/history", "history", "History")}
      {tab("/logs", "logs", "Logs")}
      {tab("/settings", "settings", "Settings", sk?.agents?.some((a) => a.state === "stale") &&
        <span class="badge dot" title="Skill update available" aria-label="skill update available" />)}
    </nav>
    {status && <div class="top-status">
      {status.running ? <span class="checked running">Checking now…</span>
        : isZero(status.last_run) ? <span class="checked running">Starting…</span>
        : status.last_error ? <span class="checked failed">Last check failed <Ago t={status.last_run} /></span>
        : <span class="checked">Checked <Ago t={status.last_run} /> · next <Ago t={status.next_run} /></span>}
      {status.budget_note && <span class="budget" title={status.budget_note}>5-hour window <BudgetMeter b={status.budget} />
        {" "}{pct(status.budget.used)} of {pct(status.budget.share)}</span>}
      <button class="btn quiet small" disabled={status.running || checking} onClick={run}>Check now</button>
    </div>}
  </header>;
}

// Notices are the app-wide reasons reviews aren't happening.
export function Notices({ status }: { status: Status }) {
  return <>
    {status.runtime_error && <p class="notice">Reviews are waiting for the container runtime: {status.runtime_error}</p>}
    {status.budget_wait && <p class="notice">{status.budget_wait}</p>}
    {!status.reviews && <p class="notice quiet">Reviews are off. Turn them on in <a href="/settings">Settings</a>.</p>}
  </>;
}

// RunTable lists runs, each linked to base/<run id>.
export function RunTable({ runs, base }: { runs: Run[]; base: string }) {
  useTick();
  return <div class="card scrollx"><table class="runs">
    <thead><tr><th>Outcome</th><th>Started</th><th>Trigger</th><th class="num">Took</th><th class="num">Turns</th><th class="num">Cost</th><th>Head</th></tr></thead>
    {runs.map((r) => <tbody key={r.id}>
      <tr><td><RunChip run={r} /></td><td><a href={`${base}/${r.id}`}>{stamp(r.started)}</a></td><td>{reason(r.job.reason)}</td>
        <td class="num">{dur(r)}</td><td class="num">{r.claude ? r.claude.num_turns : "—"}</td>
        <td class="num">{r.claude ? `$${r.claude.cost_usd.toFixed(2)}` : "—"}</td><td><code>{short(r.job.pr.head_sha)}</code></td></tr>
      {(r.error || r.overdue) && <tr class="note"><td /><td colSpan={6}><div class="err">{r.overdue && "Running longer than the review timeout. "}{r.error}</div></td></tr>}
    </tbody>)}
  </table></div>;
}

// Page sets the document title and wraps a page in the header.
export function Page({ title, status, page, children }: { title: string; status?: Status; page?: string; children: ComponentChildren }) {
  useEffect(() => { document.title = `${title} · sisyphus`; }, [title]);
  return <><Top status={status} page={page} />{children}</>;
}

export const Loading = () => <p class="muted" style="padding: 20px">Loading…</p>;
