// Last mile: every handoff on the left (newest first, stacks together); the overview or one handoff on the right.
import { useState } from "preact/hooks";
import { post, put, type HandoffView, type Status } from "../api";
import { Ago, Chip, Ext, Loading, Page } from "../components";
import { approval, base, handoffTone, plural, reviewerChips } from "../format";
import { HandoffMain } from "./handoff";

export function LastMile({ status: s, id }: { status?: Status; id?: string }) {
  if (!s) return <Page title={id ?? "Last mile"} page="lastmile"><Loading /></Page>;
  const hs = s.handoffs ?? [];
  const v = id ? hs.find((h) => h.id === id) : undefined;
  return <Page title={id ?? "Last mile"} status={s} page="lastmile">
    <div class="cols">
      <aside aria-label="Handoffs"><HandoffList hs={hs} sel={id} /></aside>
      {id ? v ? <HandoffMain key={v.id} v={v} />
        : <main><div class="pagehead"><h1>No such handoff</h1><p class="muted">It may have been cleaned up.</p></div></main>
        : <Overview s={s} />}
    </div>
  </Page>;
}

// stacks splits the open handoffs, in Views order (each root followed by its descendants), into
// one run per stack, bottom first. A handoff whose parent is over starts its own run.
// ponytail: a fork (two children on one parent) still draws as one straight rail.
export function stacks(open: HandoffView[]): HandoffView[][] {
  const out: HandoffView[][] = [];
  for (const v of open) v.waiting_on && out.length ? out[out.length - 1].push(v) : out.push([v]);
  return out;
}

const StackIcon = () => <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
  <rect x="2" y="9" width="12" height="4" rx="1" /><path d="M4 6.5h8M6 3.5h4" /></svg>;
const ChainIcon = () => <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
  <path d="M6.5 9.5l3-3" /><path d="M7 4.5l1.2-1.2a2.5 2.5 0 013.5 3.5L10.5 8" /><path d="M9 11.5l-1.2 1.2a2.5 2.5 0 01-3.5-3.5L5.5 8" /></svg>;

function HandoffList({ hs, sel }: { hs: HandoffView[]; sel?: string }) {
  const open = hs.filter((v) => !v.over);
  const done = hs.filter((v) => v.over).sort((a, b) => b.updated.localeCompare(a.updated));
  const cur = (v: HandoffView) => v.id === sel ? "page" as const : undefined;
  const meta = (v: HandoffView, note?: string | false) => <span class="meta">{v.pr_number ? `${base(v.repo)} #${v.pr_number}` : v.id}
    {note && ` · ${note}`}{approval(v) && " · ✓ approved"}</span>;
  const head = (v: HandoffView) => <><span class="row"><Chip tone={handoffTone(v.state)}>{v.state}</Chip><Ago t={v.updated} /></span>
    <span class="title">{v.title}</span></>;
  const item = (v: HandoffView) => <a key={v.id} class="pr" href={`/lastmile/${v.id}`} aria-current={cur(v)}>{head(v)}{meta(v)}</a>;
  // A stack or chain on a rail in merge order: its base branch on top, then the PR that merges first.
  const rail = (g: HandoffView[]) => {
    const bottom = g[0], gs = g.find((v) => v.snapshot?.stack)?.snapshot?.stack, ghost = bottom.parent_pr;
    if (g.length === 1 && !gs && !ghost) return item(bottom);
    const next = !ghost && (bottom.snapshot?.stack?.position ?? 1) === 1;
    const note = (v: HandoffView) => {
      const s = v.snapshot?.stack, pos = s && `${s.position} of ${s.size}`;
      if (v !== bottom) return pos;
      return ghost ? `waits for #${ghost}` : [pos, next && "lands next"].filter(Boolean).join(" · ");
    };
    return <div key={bottom.id} class={"rail" + (gs ? "" : " chain")}>
      <div class="base" title={`${gs ? "GitHub stack" : "Branch chain"} onto this base, in merge order`}>
        <span class="node">{gs ? <StackIcon /> : <ChainIcon />}</span>{gs?.base ?? (bottom.snapshot?.base || bottom.base)}</div>
      {ghost ? <div class="rr ghost"><span class="node"><span class="dot hollow" /></span>
        <span class="body"><span class="meta">{base(bottom.repo)} #{ghost} · not a handoff, open</span></span></div>
        : null}
      {g.map((v) => <a key={v.id} class="pr rr" href={`/lastmile/${v.id}`} aria-current={cur(v)}>
        <span class="node"><span class={`dot ${handoffTone(v.state)}${v === bottom && next ? " next" : ""}`} /></span>
        <span class="body">{head(v)}{meta(v, note(v))}</span>
      </a>)}
    </div>;
  };
  return <>
    <h2><span>Open</span><span>{open.length}</span></h2>
    {open.length ? stacks(open).map(rail) : <p class="empty">{hs.length ? "No open handoffs." : "No handoffs yet."}</p>}
    {!!done.length && <details class="history" open={done.some((v) => v.id === sel)}>
      <summary><h2><span>History</span><span>{done.length}</span></h2></summary>{done.map((v) => item(v))}</details>}
  </>;
}

function Overview({ s }: { s: Status }) {
  const hs = s.handoffs ?? [], needs = s.needs_human ?? [];
  return <main>
    <div class="pagehead"><h1>Last mile</h1><div class="meta-line"><span>{plural(hs.length, "handoff")}</span></div></div>
    {!s.handoffs && <p class="notice quiet">The last mile is off (<code>lastmile.disabled</code>).</p>}
    <section aria-labelledby="needs"><h2 id="needs">Needs you</h2>
      {needs.length ? needs.map((v) => <p class="notice" role="alert" key={v.id}><b><a href={`/lastmile/${v.id}`}>{v.id}{v.pr_number ? ` · ${base(v.repo)} #${v.pr_number}` : ""}</a></b>
        {" "}{v.title}{v.prev_state && ` (was ${v.prev_state})`}: {v.reason}{" "}<ApprovalNote v={v} />
        {v.pr_url && <><Ext href={v.pr_url}>Open PR ↗</Ext>{" "}</>}<a href={`/lastmile/${v.id}`}>Open handoff →</a></p>)
        : <div class="card empty"><div><b>Nothing needs you right now</b>
          <div class="muted">{hs.length ? "Pick a handoff on the left to see its state, reviewers and timeline." : <>An agent hands a branch over with <code>tlm handoff</code>.</>}</div></div></div>}
    </section>
  </main>;
}

// Approved is the PR's approval as a chip, with "mergeable" when GitHub would merge it now.
export function Approved({ v }: { v: HandoffView }) {
  const a = approval(v);
  return a && <>{" "}<Chip tone="ok">✓ {a.text}{a.mergeable && " · mergeable"}</Chip></>;
}

// ApprovalNote says, next to a needs-human reason, that the PR is approved: what the human is asked
// about may be all that stands between it and the merge.
export function ApprovalNote({ v }: { v: HandoffView }) {
  const a = approval(v);
  return a && <><b>The PR is {a.text}{a.mergeable && " and GitHub would merge it now"}.</b>{" "}</>;
}

export function Rounds({ v }: { v: HandoffView }) {
  const rounds = Object.entries(v.fix_rounds ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (!rounds.length) return <>ci <b>0</b>/{v.max_fix_rounds}</>;
  return <>{rounds.map(([k, n]) => <span key={k}>{k} <b>{n}</b>/{v.max_fix_rounds} </span>)}</>;
}

export function Reviewers({ v }: { v: HandoffView }) {
  const chips = reviewerChips(v);
  if (!chips.length) return <span class="muted">nobody yet</span>;
  return <>{chips.map((c) => <span key={c.login} class={"rv" + (c.cls ? " " + c.cls : "")}>{c.cls === "human" && "+"}{c.login}</span>)}</>;
}

// act POSTs a handoff action (or PUTs when given a body); the status stream brings the change.
export function useAct(): [string, (url: string, body?: unknown) => Promise<boolean>] {
  const [busy, setBusy] = useState("");
  return [busy, async (url, body) => {
    setBusy(url);
    try {
      await (body === undefined ? post(url) : put(url, body));
      return true;
    } catch (e) {
      alert((e as Error).message);
      return false;
    } finally {
      setBusy("");
    }
  }];
}

const modes = [["off", "off"], ["auto", "when checks pass"], ["approved", "when approved"]];

function MergeMode({ v, busy, act }: { v: HandoffView; busy: string; act: (url: string, body?: unknown) => Promise<boolean> }) {
  const url = `/api/handoffs/${encodeURIComponent(v.id)}/merge-mode`, cur = v.autonomy.merge;
  const set = (mode: string) => act(url, { mode });
  if (v.snapshot?.auto_merge_allowed) return <label class="muted"><input type="checkbox" checked={cur === "approved" || cur === "auto"} disabled={!!busy}
    onChange={(e) => set(e.currentTarget.checked ? "approved" : "off")} /> Auto-merge</label>;
  return <label class="muted">Auto-merge{" "}<select value={cur} disabled={!!busy} onChange={(e) => set(e.currentTarget.value)}>
    {cur === "shadow" && <option value="shadow">shadow</option>}
    {modes.map(([m, l]) => <option key={m} value={m}>{l}</option>)}</select></label>;
}

export function MergeControls({ v }: { v: HandoffView }) {
  const [busy, act] = useAct();
  const merge = `/api/handoffs/${encodeURIComponent(v.id)}/merge`;
  return <>
    {v.merge_requested && !v.over && <><span class="muted">merge pending</span>{" "}</>}
    {v.can_merge && !v.merge_requested && v.state !== "queued" && <><button class="btn small merge" disabled={!!busy}
      onClick={() => confirm(`Squash and merge ${v.repo}#${v.pr_number}?`) && act(merge)}>Squash and merge</button>{" "}</>}
    {!v.over && <MergeMode v={v} busy={busy} act={act} />}
  </>;
}

export function Actions({ v, merge = true }: { v: HandoffView; merge?: boolean }) {
  const [busy, act] = useAct();
  const id = encodeURIComponent(v.id), resume = `/api/handoffs/${id}/resume`, cancel = `/api/handoffs/${id}/cancel`;
  const behind = v.snapshot?.behind ?? 0;
  return <>
    {v.state === "needs-human" && <><button class="btn small" disabled={!!busy} onClick={() => act(resume)}>Resume</button>{" "}</>}
    {v.rebase_asked ? <><span class="muted">rebase pending ({v.rebase_asked})</span>{" "}</>
      : !v.over && !v.dirty && behind > 0 && <><button class="btn quiet small" disabled={!!busy}
        title={`Rebase ${v.branch} onto ${v.base}${v.state === "needs-human" ? ", resuming the handoff" : ""}; a conflict goes to the conflict agent or to you`}
        onClick={() => act(`/api/handoffs/${id}/rebase`)}>Rebase ({behind} behind)</button>{" "}</>}
    {merge && <MergeControls v={v} />}{" "}
    {!v.over && <button class="btn quiet small" disabled={!!busy} onClick={() => act(cancel)}>Cancel</button>}
  </>;
}
