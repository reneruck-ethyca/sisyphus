// Last mile: every handoff, newest first with stacks together.
import { useState } from "preact/hooks";
import { post, type HandoffView, type Status } from "../api";
import { Ago, Chip, Ext, Loading, Page } from "../components";
import { base, handoffTone, plural, reviewerChips } from "../format";

export function LastMile({ status: s }: { status?: Status }) {
  if (!s) return <Page title="Last mile" page="lastmile"><Loading /></Page>;
  const hs = s.handoffs ?? [];
  return <Page title="Last mile" status={s} page="lastmile">
    <main class="lastmile">
      <div class="pagehead"><h1>Last mile</h1><div class="meta-line"><span>{plural(hs.length, "handoff")}</span></div></div>
      {!s.handoffs && <p class="notice quiet">The last mile is off (<code>lastmile.disabled</code>).</p>}
      {(s.needs_human ?? []).map((v) => <p class="notice" role="alert" key={v.id}><b>Needs you: <a href={`/lastmile/${v.id}`}>{v.id}{v.pr_number ? ` · ${base(v.repo)} #${v.pr_number}` : ""}</a></b>
        {" "}{v.title}{v.prev_state && ` (was ${v.prev_state})`}: {v.reason}{" "}
        {v.pr_url && <><Ext href={v.pr_url}>Open PR ↗</Ext>{" "}</>}<a href={`/lastmile/${v.id}`}>Open handoff →</a></p>)}
      <div class="card">
        <div class="card-h"><span class="muted">Newest first, stacks together</span><a class="end" href="/lastmile/shadow"><b>Shadow-merge report →</b></a></div>
        {hs.length ? <>
          <ol class="lm">
            <li class="hd"><span>State</span><span>Handoff</span><span>Fix rounds</span><span>Reviewers</span><span>Updated</span><span /></li>
            {hs.map((v) => <li key={v.id} class={v.depth ? `depth${v.depth}` : undefined}>
              <Chip tone={handoffTone(v.state)}>{v.state}</Chip>
              <span class="grow"><a class="ttl" href={`/lastmile/${v.id}`}>{v.title}</a>
                <span class="facts"><span class="id">{v.id}</span>{v.pr_url && <> · <Ext href={v.pr_url}>{base(v.repo)} #{v.pr_number}</Ext></>}
                  {v.waiting_on && ` · waiting on ${v.waiting_on}`}{v.reason && ` · ${v.reason}`}{v.prev_state && ` · was ${v.prev_state}`}</span>
                {v.error && <span class="err">{v.error}</span>}{v.warning && <span class="facts">{v.warning}</span>}</span>
              <span class="rounds"><Rounds v={v} /></span>
              <span class="rvs"><Reviewers v={v} /></span>
              <span class="t"><Ago t={v.updated} /></span>
              <span class="acts"><Actions v={v} /></span></li>)}
          </ol>
          <div class="card-f legend"><span><span class="rv">alice</span> requested by TLM</span><span><span class="rv human">+carol</span> added by a human</span>
            <span><span class="rv gone">bob</span> removed by a human, never re-added</span></div>
        </> : <p class="card-b muted">No handoffs. An agent hands a branch over with <code>tlm handoff</code>.</p>}
      </div>
    </main>
  </Page>;
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

// act POSTs a handoff action; the status stream brings the change.
export function useAct(): [string, (url: string) => Promise<boolean>] {
  const [busy, setBusy] = useState("");
  return [busy, async (url) => {
    setBusy(url);
    try {
      await post(url);
      return true;
    } catch (e) {
      alert((e as Error).message);
      return false;
    } finally {
      setBusy("");
    }
  }];
}

export function Actions({ v }: { v: HandoffView }) {
  const [busy, act] = useAct();
  const resume = `/api/handoffs/${encodeURIComponent(v.id)}/resume`, cancel = `/api/handoffs/${encodeURIComponent(v.id)}/cancel`;
  return <>
    {v.state === "needs-human" && <><button class="btn small" disabled={!!busy} onClick={() => act(resume)}>Resume</button>{" "}</>}
    {!v.over && <button class="btn quiet small" disabled={!!busy} onClick={() => act(cancel)}>Cancel</button>}
  </>;
}
