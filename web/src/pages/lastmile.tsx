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

function HandoffList({ hs, sel }: { hs: HandoffView[]; sel?: string }) {
  const open = hs.filter((v) => !v.over).map((v) => v.waiting_on ? v : { ...v, depth: 0 });
  const done = hs.filter((v) => v.over).sort((a, b) => b.updated.localeCompare(a.updated));
  const item = (v: HandoffView) => <a key={v.id} class={"pr" + (v.depth ? ` depth${Math.min(v.depth, 3)}` : "")} href={`/lastmile/${v.id}`}
    aria-current={v.id === sel ? "page" : undefined}>
    <span class="row"><Chip tone={handoffTone(v.state)}>{v.state}</Chip><Ago t={v.updated} /></span>
    <span class="title">{v.title}</span>
    <span class="meta">{v.pr_number ? `${base(v.repo)} #${v.pr_number}` : v.id}{v.waiting_on && ` · waiting on ${v.waiting_on}`}{approval(v) && " · ✓ approved"}</span>
  </a>;
  return <>
    <h2><span>Open</span><span>{open.length}</span></h2>
    {open.length ? open.map(item) : <p class="empty">{hs.length ? "No open handoffs." : "No handoffs yet."}</p>}
    {!!done.length && <><h2><span>History</span><span>{done.length}</span></h2>{done.map(item)}</>}
  </>;
}

function Overview({ s }: { s: Status }) {
  const hs = s.handoffs ?? [], needs = s.needs_human ?? [];
  return <main>
    <div class="pagehead"><h1>Last mile</h1><div class="meta-line"><span>{plural(hs.length, "handoff")}</span>
      <a href="/lastmile/shadow">Shadow-merge report →</a></div></div>
    {!s.handoffs && <p class="notice quiet">The last mile is off (<code>lastmile.disabled</code>).</p>}
    <section aria-labelledby="needs"><h2 id="needs">Needs you</h2>
      {needs.length ? needs.map((v) => <p class="notice" role="alert" key={v.id}><b><a href={`/lastmile/${v.id}`}>{v.id}{v.pr_number ? ` · ${base(v.repo)} #${v.pr_number}` : ""}</a></b>
        {" "}{v.title}{v.prev_state && ` (was ${v.prev_state})`}: {v.reason}{" "}<ApprovalNote v={v} />
        {v.pr_url && <><Ext href={v.pr_url}>Open PR ↗</Ext>{" "}</>}<a href={`/lastmile/${v.id}`}>Open handoff →</a></p>)
        : <div class="card empty"><div><b>Nothing needs you right now</b>
          <div class="muted">{hs.length ? "Pick a handoff on the left to see its state, merge policy and timeline." : <>An agent hands a branch over with <code>tlm handoff</code>.</>}</div></div></div>}
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
    {v.can_merge && !v.merge_requested && v.state !== "queued" && <><button class="btn small" disabled={!!busy}
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
