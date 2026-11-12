// Reviews and History: every PR on the left; the overview, a PR, or one of its runs on the right.
import { Fragment } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { HTTPError, post, prAPI, put, type HistoryRow, type PRView, type ReviewEvent, type ReviewFile, type Run, type RunLog, type StackStatus, type Status } from "../api";
import { Ago, Chip, Ext, Loading, Md, Notices, Page, RunChip, RunTable } from "../components";
import { base, dur, fuzzy, isZero, plural, profiles, reason, runPath, short, stamp, when, withCurrent } from "../format";
import { loadSession, navigate, saveSession, stream, unstream, useFetch, useTick } from "../live";

type Ref = { repo: string; number: number };
type Props = { status?: Status; error: string; pr?: Ref; run?: string; archive?: boolean };

// runHref is where a PR in the "reviewing" list leads: its running run, else the PR.
export function runHref(p: PRView): string {
  const last = p.review.runs?.[0];
  return last?.status === "running" ? runPath(p.id, last.id) : `/prs/${p.repo}/${p.number}`;
}

export function Reviews({ status: s, error, pr, run, archive }: Props) {
  const history = useFetch<HistoryRow[]>(archive ? "/api/history" : "", archive);
  const id = pr && `${pr.repo}#${pr.number}`;
  const title = run ? `Run ${run}` : id ?? (archive ? "History" : "Status");
  if (!s || (archive && !history.data)) return <Page title={title} status={s} page={archive ? "history" : "reviews"}><Loading />
    {(error || history.error) && <p class="notice">{error || history.error}</p>}</Page>;

  let main;
  if (archive) {
    const row = id ? history.data!.find((h) => h.id === id) : undefined;
    if (id && !row) main = <NotFound what="PR" />;
    else if (row && run) main = <RunPage key={row.id + run} status={s} pr={row} runs={row.runs ?? []} runID={run} archive />;
    else if (row) main = <Archived key={row.id} row={row} />;
    else main = <div class="pagehead"><h1>History</h1>
      <div class="muted">Pick a PR on the left to see whether it was reviewed and what the reviews came to. PRs dropped before history was kept show as dropped "unknown".</div></div>;
  } else if (id) {
    const p = s.prs?.find((x) => x.id === id);
    if (!p) main = <NotFound what="PR" />;
    else if (run) main = <RunPage key={p.id + run} status={s} pr={p} runs={p.review.runs ?? []} runID={run} />;
    else main = <PRPage key={p.id} status={s} pr={p} />;
  } else main = <Overview status={s} />;

  return <Page title={title} status={s} page={archive ? "history" : "reviews"}>
    {archive && <div class="archive-banner" role="note"><span><b>History.</b> PRs sisyphus no longer tracks. Nothing here updates live.</span>
      <a href="/reviews">Back to current PRs →</a></div>}
    <div class={"cols" + (archive ? " archive" : "")}>
      <aside aria-label="Pull requests">{archive ? <HistoryList rows={history.data!} sel={id} /> : <PRList status={s} sel={id} />}</aside>
      <main>{error && <p class="notice">{error}</p>}{main}</main>
    </div>
  </Page>;
}

const NotFound = ({ what }: { what: string }) => <div class="pagehead"><h1>No such {what}</h1><p class="muted">It may have been untracked or reset.</p></div>;

function PRList({ status: s, sel }: { status: Status; sel?: string }) {
  const [busy, setBusy] = useState("");
  const track = async (key: string, url: string) => {
    setBusy(key);
    try { await post(url); } catch (e) { alert((e as Error).message); }
    setBusy("");
  };
  return <>
    {s.groups?.length ? s.groups.map((g) => <Fragment key={g.label}>
      <h2><span>{g.label}</span><span>{g.prs.length}</span></h2>
      {g.prs.map((p) => <a key={p.id} class="pr" href={`/prs/${p.repo}/${p.number}`} aria-current={p.id === sel ? "page" : undefined}>
        <span class="row"><Chip tone={p.tone}>{p.verdict}</Chip>{p.design_review && <span class="chip" title="Reviewed only when you start one">Design</span>}
          {!isZero(p.verdict_at) && <Ago t={p.verdict_at} />}</span>
        <span class="title">{p.title}</span>
        <span class="meta">{base(p.repo)} #{p.number}{p.author && ` · ${p.author}`}</span>
      </a>)}
    </Fragment>) : <><h2>Pull requests</h2><p class="empty">None tracked yet. PRs you're asked to review show up after the next check.</p></>}
    {s.ignored?.length ? <><h2><span>Untracked</span><span>{s.ignored.length}</span></h2>
      {s.ignored.map((i) => { const key = `${i.repo}#${i.number}`; return <div class="untracked" key={key}><span title={i.title}>{key}</span>
        <button class="linkbtn" disabled={busy === key} onClick={() => track(key, `${prAPI(i)}/track`)}>Track again</button></div>; })}</> : null}
  </>;
}

const searchKey = "sisyphus-history-search";

function HistoryList({ rows, sel }: { rows: HistoryRow[]; sel?: string }) {
  const [q, setQ] = useState(() => loadSession(searchKey, ""));
  const shown = rows.filter((r) => fuzzy(q, r.title));
  return <>
    <div class="search"><input type="search" placeholder="Search titles" aria-label="Search PR titles" autocomplete="off" value={q}
      onInput={(e) => { const v = e.currentTarget.value; setQ(v); saveSession(searchKey, v); }} /></div>
    <h2><span>No longer tracked</span><span>{shown.length}</span></h2>
    {shown.map((r) => <a key={r.id} class="pr" href={`/history/${r.repo}/${r.number}`} aria-current={r.id === sel ? "page" : undefined}>
      <span class="row">{r.runs?.length ? <RunChip run={r.runs[0]} /> : <Chip>Not reviewed</Chip>}</span>
      <span class="title">{r.title}</span>
      <span class="meta">{base(r.repo)} #{r.number} · picked up {stamp(r.tracked_at)}</span>
    </a>)}
    {!rows.length && <p class="empty">No PRs dropped yet.</p>}
    {rows.length > 0 && !shown.length && <p class="empty">No title matches.</p>}
  </>;
}

function Overview({ status: s }: { status: Status }) {
  const attention = (s.prs ?? []).filter((p) => p.group === "attention"), reviewing = (s.prs ?? []).filter((p) => p.group === "reviewing");
  const needs = s.needs_human ?? [];
  return <>
    <h1>Overview</h1>
    <section aria-labelledby="needs"><h2 id="needs">Needs you</h2>
      {s.last_error && <p class="notice">The last check failed: {s.last_error}</p>}
      <Notices status={s} />
      {needs.length > 0 && <p class="notice">{needs.length} handoff{needs.length === 1 ? " needs" : "s need"} you → <a href="/lastmile">Open Last mile</a></p>}
      {attention.length ? <ol class="list card">{attention.map((p) => <li key={p.id}>
        <Chip tone={p.tone}>{p.verdict}</Chip>
        <span class="grow"><a href={`/prs/${p.repo}/${p.number}`}>{p.title}</a><span class="facts">{base(p.repo)} #{p.number} · {p.last_error || p.review_note}</span></span>
        {!isZero(p.verdict_at) && <Ago t={p.verdict_at} />}</li>)}</ol>
        : !s.last_error && !s.runtime_error && s.reviews && !needs.length && <div class="card empty"><div><b>Nothing needs you right now</b>
          <div class="muted">Failed reviews, merge conflicts and container problems show up here.</div></div></div>}
    </section>
    <section aria-labelledby="now"><h2 id="now">Reviewing now</h2>
      {reviewing.length ? <ol class="list card">{reviewing.map((p) => <li key={p.id}>
        <Chip tone={p.tone}>{p.verdict}</Chip>
        <span class="grow"><a href={runHref(p)}>{p.title}</a>
          <span class="facts">{base(p.repo)} #{p.number} · {p.review_note}{p.review.runs?.[0]?.overdue && <> · <span class="err">running longer than the review timeout</span></>}</span></span>
        {!isZero(p.verdict_at) && <Ago t={p.verdict_at} />}</li>)}</ol>
        : <p class="muted">No review running.</p>}
      {s.budget_note && <p class="muted">{s.budget_note}</p>}
    </section>
    <section aria-labelledby="recent"><h2 id="recent">Recent activity</h2>
      {s.activity?.length ? <ol class="list card">{s.activity.map((a) => <li key={a.pr + a.run}>
        <Chip tone={a.tone}>{a.label}</Chip>
        <span class="grow"><a href={runPath(a.pr, a.run)}>{a.title}</a><span class="facts">{a.pr}{a.detail && ` · ${a.detail}`}</span></span>
        <Ago t={a.at} /></li>)}</ol>
        : <p class="muted">No reviews yet.</p>}
    </section>
  </>;
}

// reviewFile loads what a run's agent wrote; again when the run's status changes.
const useReviewFile = (prID: string, run?: Run) => {
  const [repo, n] = prID.split("#");
  return useFetch<ReviewFile | null>(run ? `/api/prs/${repo}/${n}/runs/${run.id}/review` : "", run?.status);
};

function LatestReview({ pr, runs, runBase, foot }: { pr: { id: string }; runs: Run[]; runBase: string; foot?: preact.ComponentChildren }) {
  const last = runs[0];
  const rf = useReviewFile(pr.id, last);
  return <div class="card">
    <div class="card-h"><RunChip run={last} />
      {!!last.posted?.comments && <span class="muted">{plural(last.posted.comments, "inline comment")}</span>}
      <span class="muted">{last.status === "running" ? <>started <Ago t={last.started} /></> : <Ago t={last.finished} />}</span>
      {last.posted?.url && <Ext class="end" href={last.posted.url}>View on GitHub ↗</Ext>}</div>
    {last.error && <div class="card-b err">{last.error}</div>}
    {rf.data?.body && <div class="card-b"><div class="review-body clamp"><Md text={rf.data.body} /></div>
      <a href={`${runBase}/${last.id}`}>Read the full review →</a></div>}
    {foot && <div class="card-f">{foot}</div>}
  </div>;
}

function PRPage({ status: s, pr }: { status: Status; pr: PRView }) {
  const api = prAPI(pr);
  const [msg, setMsg] = useState<{ text: string; err?: boolean }>({ text: "" });
  const [busy, setBusy] = useState("");
  const menu = useRef<HTMLDetailsElement>(null);
  const runs = pr.review.runs ?? [];
  const stack = useFetch<StackStatus | null>(`${api}/stack`, pr.head_sha + pr.review.state);

  const act = async (action: string, ask?: string, then?: string) => {
    if (ask && !confirm(`${pr.id}: ${ask}`)) return;
    menu.current?.removeAttribute("open");
    setBusy(action);
    setMsg({ text: `${action}…` });
    try {
      await post(`${api}/${action}`);
      setMsg({ text: "" });
      if (then) navigate(then);
    } catch (e) {
      setMsg({ text: (e as Error).message, err: true });
    }
    setBusy("");
  };

  return <>
    <Notices status={s} />
    <div class="pagehead">
      <h1>{pr.title}</h1>
      <div class="meta-line"><span>{pr.repo} #{pr.number}</span>{pr.author && <span>by <Ext href={`https://github.com/${pr.author}`}>{pr.author}</Ext></span>}
        <code>{pr.head_ref}</code><span>tracked <Ago t={pr.tracked_at} /></span></div>
      <div class="actions">
        <button class="btn" disabled={busy === "review"} onClick={() => act("review")}>{runs.length ? "Review again" : "Review now"}</button>
        <Ext class="btn quiet" href={pr.url}>Open on GitHub ↗</Ext>
        <details class="menu" ref={menu}><summary class="btn quiet">More ▾</summary><div>
          <button disabled={!!busy} onClick={() => act("reset", "Reset the review status? Stops a running review and forgets the review history; a fresh review is queued. The stack stays.")}>
            Reset review status<small>forget history, review again</small></button>
          <button disabled={!!busy} onClick={() => act("wipe", "Wipe the review data? Stops a running review, forgets the review history, and removes the stack: containers, network, DB volume, and its images unless something else uses them. The next review starts fresh.")}>
            Wipe review data<small>+ stack containers, volumes, images</small></button>
          <button class="danger" disabled={!!busy} onClick={() => act("untrack", "Untrack this PR? Wipes its review data and stack, removes the worktree and branch sisyphus created, and stops tracking it. It isn't picked up again until you track it again.", "/reviews")}>
            Untrack PR<small>+ worktree; ignored until tracked again</small></button>
        </div></details>
        <span class={msg.err ? "err" : "muted"} role="status">{msg.text}</span>
      </div>
    </div>
    {pr.health === "conflict" ? <p class="notice">Merge conflict at head <code>{short(pr.head_sha)}</code>: the on_conflict hook couldn't resolve it. {pr.last_error}</p>
      : pr.health === "error" && <p class="notice">Syncing the worktree failed: {pr.last_error}</p>}

    <AgentFold pr={pr} />

    <section aria-labelledby="latest"><h2 id="latest">Latest review</h2>
      {runs.length ? <LatestReview pr={pr} runs={runs} runBase={`/prs/${pr.repo}/${pr.number}/runs`}
        foot={pr.up_to_date && !pr.design_review ? <>Up to date with <code>{short(pr.head_sha)}</code>. The next review starts on new commits or replies.</> : pr.review_note} />
        : <div class="card"><div class="card-b muted">{pr.review_note}</div></div>}
    </section>

    {runs.length > 0 && <section aria-labelledby="runs"><h2 id="runs">Review runs</h2>
      <RunTable runs={runs} base={`/prs/${pr.repo}/${pr.number}/runs`} /></section>}

    {(stack.data || stack.error) && <details class="fold" id="fold-stack"><summary>Local stack
      {stack.data && <><Chip>{stack.data.state}</Chip><span class="muted"><code>{stack.data.project}</code> · {plural(stack.data.containers?.length ?? 0, "container")}</span></>}
      {stack.error && <span class="err">{stack.error}</span>}</summary>
      {stack.data && <div>
        {stack.data.containers?.length ? <table>
          <thead><tr><th>Service</th><th>Container</th><th>State</th><th>Image</th></tr></thead>
          <tbody>{stack.data.containers.map((c) => <tr key={c.name}><td>{c.service}</td><td><code>{c.name}</code></td><td>{c.status}</td><td><code>{c.image}</code></td></tr>)}</tbody>
        </table> : <p class="muted">No containers: the next review starts the stack.</p>}
        <p class="muted">Volumes: {codes(stack.data.volumes)} · Network: {codes(stack.data.networks)}</p>
      </div>}
    </details>}

    <details class="fold" id="fold-tracking"><summary>Worktree and tracking
      <span class="muted">head {short(pr.head_sha)}{pr.synced_sha === pr.head_sha && ", synced"}</span></summary>
      <dl>
        <dt>Worktree</dt><dd><code>{pr.worktree}</code></dd>
        <dt>Head</dt><dd><code>{pr.head_sha}</code></dd>
        <dt>Synced</dt><dd><code>{pr.synced_sha || "—"}</code></dd>
        <dt>Created by sisyphus</dt><dd>{pr.created_worktree ? `worktree${pr.created_branch ? " and branch" : ""}; removed when the PR is untracked` : "no, reused an existing worktree"}</dd>
        <dt>Tracked since</dt><dd>{when(pr.tracked_at)}</dd>
        <dt>Last activity on GitHub</dt><dd>{when(pr.last_seen_at)}</dd>
        {pr.conflict_head_sha && <><dt>Conflict head</dt><dd><code>{pr.conflict_head_sha}</code></dd></>}
      </dl>
    </details>
  </>;
}

const codes = (xs: string[] | null) => xs?.length ? xs.map((x, i) => <Fragment key={x}>{i > 0 && ", "}<code>{x}</code></Fragment>) : "none";

function AgentFold({ pr }: { pr: PRView }) {
  const [inherit, setInherit] = useState(!pr.overrides_agent);
  const [agent, setAgent] = useState(pr.agent);
  const [msg, setMsg] = useState("");
  const [saving, setSaving] = useState(false);
  useEffect(() => { setInherit(!pr.overrides_agent); setAgent(pr.agent); }, [pr.overrides_agent, pr.agent.vendor, pr.agent.model, pr.agent.effort]);
  const profile = profiles[agent.vendor] ?? { models: [], efforts: [] };
  const save = async (e: Event) => {
    e.preventDefault();
    setSaving(true);
    setMsg("Saving…");
    try {
      await put(`${prAPI(pr)}/agent`, { inherit, agent });
      setMsg("Saved.");
    } catch (err) {
      setMsg((err as Error).message);
    }
    setSaving(false);
  };
  const select = (name: "vendor" | "model" | "effort", label: string, values: string[]) =>
    <div class="field"><label for={`agent-${name}`}><b>{label}</b></label><span class="control">
      <select id={`agent-${name}`} disabled={inherit} value={agent[name]}
        onChange={(e) => { const v = e.currentTarget.value; setAgent(name === "vendor" ? { vendor: v, model: "", effort: "" } : { ...agent, [name]: v }); }}>
        {withCurrent(values, agent[name]).map((v) => <option key={v}>{v}</option>)}
      </select></span></div>;
  return <details class="fold" id="fold-agent"><summary>Review agent
    <span class="muted">{pr.agent.vendor} · {pr.agent.model}{pr.agent.effort && ` · ${pr.agent.effort}`}{pr.overrides_agent ? " · this PR override" : " · app default"}</span>
  </summary><div>
    <form onSubmit={save}>
      <label><input type="checkbox" checked={inherit} onChange={(e) => setInherit(e.currentTarget.checked)} /> Use the app-wide default</label>
      {select("vendor", "Vendor", ["claude", "codex"])}
      {select("model", "Model", profile.models)}
      {select("effort", "Reasoning effort", profile.efforts)}
      <button class="btn" type="submit" disabled={saving}>Save agent</button><span class="muted" role="status">{msg}</span>
    </form>
  </div></details>;
}

function Archived({ row }: { row: HistoryRow }) {
  const runs = row.runs ?? [], runBase = `/history/${row.repo}/${row.number}/runs`;
  return <>
    <div class="pagehead">
      <h1>{row.title}</h1>
      <div class="meta-line"><span>{row.repo} #{row.number}</span>{row.author && <span>by <Ext href={`https://github.com/${row.author}`}>{row.author}</Ext></span>}
        <span>picked up {when(row.tracked_at)}</span>
        {isZero(row.dropped_at) ? <span>dropped: unknown</span> : <span>dropped {when(row.dropped_at)}: {row.reason}</span>}</div>
      <div class="actions"><Ext class="btn quiet" href={row.url}>Open on GitHub ↗</Ext></div>
    </div>
    <section aria-labelledby="latest"><h2 id="latest">Latest review</h2>
      {runs.length ? <LatestReview pr={row} runs={runs} runBase={runBase} />
        : <div class="card"><div class="card-b muted">No reviews kept. A PR's reviews are deleted when it's untracked, reset or wiped.</div></div>}
    </section>
    {runs.length > 0 && <section aria-labelledby="runs"><h2 id="runs">Review runs</h2><RunTable runs={runs} base={runBase} /></section>}
  </>;
}

type Filter = "text" | "tools" | "other";

function RunPage({ status: s, pr, runs, runID, archive }: { status: Status; pr: { id: string; repo: string; number: number; title: string }; runs: Run[]; runID: string; archive?: boolean }) {
  useTick();
  const run = runs.find((r) => r.id === runID);
  const files = `/api/prs/${pr.repo}/${pr.number}/runs/${runID}`;
  const log = useFetch<RunLog>(run ? `${files}/log` : "", runID + run?.status);
  const rf = useReviewFile(pr.id, run);
  const [live, setLive] = useState<ReviewEvent[]>([]);
  const [shown, setShown] = useState<Record<Filter, boolean>>({ text: true, tools: true, other: false });
  const setup = useRef<HTMLPreElement>(null);
  const running = run?.status === "running";

  // Follow a running run from where the loaded log ends; a finished one is all in the log.
  useEffect(() => {
    setLive([]);
    if (!running || !log.data) return;
    const src = stream(`${files}/events?from=${log.data.next}`);
    src.onmessage = (m) => {
      const follow = innerHeight + scrollY >= document.body.scrollHeight - 40;
      setLive((l) => [...l, JSON.parse(m.data)]);
      if (follow) requestAnimationFrame(() => scrollTo(0, document.body.scrollHeight));
    };
    src.addEventListener("done", () => unstream(src));
    return () => unstream(src);
  }, [running, log.data]);
  useEffect(() => { if (setup.current) setup.current.scrollTop = setup.current.scrollHeight; }, [live.length]);

  if (!run) return <NotFound what="run" />;
  const events = [...(log.data?.events ?? []), ...live];
  const output = events.filter((e) => e.kind === "output");
  const prHref = `/${archive ? "history" : "prs"}/${pr.repo}/${pr.number}`;
  return <>
    {!archive && <Notices status={s} />}
    <nav class="meta-line" aria-label="Breadcrumb"><a href={prHref}>{base(pr.repo)} #{pr.number} {pr.title}</a></nav>
    <div class="pagehead">
      <div class="actions"><h1>Review run · {stamp(run.started)}</h1><RunChip run={run} /></div>
      <div class="meta-line"><span>{reason(run.job.reason)} · {dur(run)}{run.claude && ` · ${run.claude.num_turns} turns · $${run.claude.cost_usd.toFixed(2)}`}</span>
        <span>head <code>{short(run.job.pr.head_sha)}</code></span>
        {running && <span>container <code>{run.container_name || run.container || "starting…"}</code></span>}
        {run.overdue && <span class="err">running longer than the review timeout</span>}</div>
      <div class="meta-line"><a href={`${files}/files/log`}>Raw log</a><a href={`${files}/files/prompt.md`}>Prompt</a><a href={`${files}/files/out/`}>Output files</a>
        {run.stack && <a href={`${files}/files/stack.log`}>Stack log</a>}
        <button class="linkbtn" onClick={() => post(`${files}/open`).catch((e: HTTPError) => alert(e.message))}>Open in Finder</button></div>
    </div>
    {run.error && <p class="notice">{run.error}</p>}
    {run.posted?.error && <p class="notice">Posting to GitHub failed: {run.posted.error}</p>}

    {rf.data && <section class="card" aria-labelledby="written">
      <div class="card-h"><h2 id="written">{run.posted ? "Posted to GitHub" : "Written, not posted"}</h2>
        {run.posted && <><span>{!!run.posted.comments && plural(run.posted.comments, "inline comment")}{!!run.posted.replies && ` · ${run.posted.replies} replies`}
          {run.posted.fallback && " · as a plain comment (GitHub refused the review)"}</span>
          {run.posted.url && <Ext class="end" href={run.posted.url}>View on GitHub ↗</Ext>}</>}</div>
      <div class="card-b review-body"><Md text={rf.data.body} /></div>
    </section>}

    <details class="fold" id="fold-setup" open={running}><summary>Setup
      <span class="muted">{output.length} lines of output{run.stack && <> · stack <code>{run.stack.project}</code>{!!run.stack.offset && `, PORT_OFFSET ${run.stack.offset}`}</>}</span></summary>
      <pre class="setup" ref={setup}>{output.map((e) => e.text + "\n").join("")}</pre>
    </details>

    <section class="card" aria-labelledby="transcript">
      <div class="card-h"><h2 id="transcript">Transcript</h2>
        <div class="filters" role="group" aria-label="Show">
          {([["text", "Messages"], ["tools", "Tools"], ["other", "System events"]] as const).map(([f, label]) =>
            <button key={f} aria-pressed={shown[f]} onClick={() => setShown({ ...shown, [f]: !shown[f] })}>{label}</button>)}
        </div></div>
      <div class={"events" + (Object.entries(shown).filter(([, on]) => !on).map(([f]) => " no-" + f).join(""))}>
        {log.loading && !log.data ? <Loading /> : events.filter((e) => e.kind !== "output").map((e, i) => <EventLine key={i} e={e} />)}
        {log.error && <p class="err">{log.error}</p>}
      </div>
    </section>
  </>;
}

function EventLine({ e }: { e: ReviewEvent }) {
  const cls = "ev " + e.kind + (e.is_error ? " bad" : "");
  if (e.kind === "tool") return <div class={cls}><b>{e.tool}</b><code>{e.text}</code></div>;
  if (e.kind === "tool_result") return <div class={cls}><details><summary>{e.is_error ? "error" : "result"}</summary><pre>{e.text}</pre></details></div>;
  return <div class={cls}>{e.text}</div>;
}
