// Shadow merge: what the merge policy would have done, against what a person did.
// ?handoff=id or ?pr=owner/repo%23n adds that PR's verdict history.
import { useEffect } from "preact/hooks";
import type { ShadowReport, Signals, Status } from "../api";
import { Ago, Chip, Ext, Loading, Page } from "../components";
import { base, isZero, plural, secs, short, stamp } from "../format";
import { useFetch } from "../live";

export function Shadow({ status: s, query }: { status?: Status; query: URLSearchParams }) {
  const pr = query.get("pr") ?? "", handoff = query.get("handoff") ?? "";
  const narrowed = pr || handoff;
  const all = useFetch<ShadowReport>("/api/lastmile/shadow", s?.handoffs?.length);
  const focus = useFetch<ShadowReport>(narrowed ? `/api/lastmile/shadow?${query}` : "", narrowed);
  const focusRow = focus.data?.prs[0];
  const focusID = focusRow?.handoff_id ?? "";
  useEffect(() => { if (focusID && location.hash) document.getElementById(location.hash.slice(1))?.scrollIntoView(); }, [focusID]);

  const r = all.data;
  return <Page title="Shadow merge" status={s} page="lastmile">
    <main class="lastmile">
      <div class="pagehead">
        <nav class="meta-line" aria-label="Breadcrumb"><a href="/lastmile">Last mile</a><span>›</span><span>Shadow merge</span></nav>
        <h1>Shadow merge</h1>
        <div class="meta-line"><span>What the merge policy would have done, against what a person did. Nothing merges on it.</span></div>
      </div>
      {all.error && <p class="notice">{all.error}</p>}
      {focus.error && <p class="notice">{focus.error}</p>}
      {!r && !all.error && <Loading />}

      {r && <div class="tiles">
        <div class="tile"><b>{r.totals.observed}</b><span>observed</span></div>
        <div class="tile"><b>{r.totals.merged}</b><span>merged</span></div>
        <div class="tile"><b>{r.totals.closed}</b><span>closed</span></div>
        <div class="tile"><b>{r.totals.open}</b><span>open</span></div>
        <div class="tile"><b>{r.totals.merged ? secs(r.mean_time_in_review_sec) : "n/a"}</b><span>mean time in review</span></div>
      </div>}

      {focus.data && <section class="focus" id={focusID}>
        <div class="meta-line" style="color: var(--fg)"><h2>{focusRow ? `${focusRow.repo} #${focusRow.pr}` : narrowed} · verdict history</h2>
          <a href="/lastmile/shadow">Show all PRs</a>{focusID && <a href={`/lastmile/${focusID}`} style="margin-left: auto">Back to handoff →</a>}</div>
        <div class="card scrollx"><table>
          <thead><tr><th>When</th><th>Head</th><th>Entry</th><th>Blockers</th><th>Signals</th></tr></thead>
          <tbody>{focus.data.history?.length ? focus.data.history.map((l, i) => <tr key={i}><td>{stamp(l.time)}</td><td><code>{short(l.head)}</code></td>
            <td>{l.outcome ? <><Chip tone={l.outcome === "merged" ? "ok" : undefined}>{l.outcome}</Chip>{l.merged_by && ` by ${l.merged_by}`}</>
              : <Chip tone={l.would_merge ? "ok" : "bad"}>would merge: {l.would_merge ? "yes" : "no"}</Chip>}</td>
            <td>{(l.blockers ?? []).join("; ")}</td><td>{l.signals && <SignalLine s={l.signals} />}</td></tr>)
            : <tr><td colSpan={5} class="muted">Nothing logged for this PR yet.</td></tr>}</tbody>
        </table></div>
      </section>}

      {r && <>
        <section><h2>Verdict vs. what the human did</h2>
          <div class="card scrollx"><table>
            <thead><tr><th>Verdict</th><th>Merged</th><th>Closed</th><th>Still open</th></tr></thead>
            <tbody>
              <tr><td>Would merge: yes</td><td>{r.agreement.would_merge_yes.merged}</td><td>{r.agreement.would_merge_yes.closed}</td><td>{r.agreement.would_merge_yes.open}</td></tr>
              <tr><td>Would merge: no</td><td>{r.agreement.would_merge_no.merged}</td><td>{r.agreement.would_merge_no.closed}</td><td>{r.agreement.would_merge_no.open}</td></tr>
            </tbody>
          </table></div>
        </section>
        <section><h2>Signals per PR</h2>
          <div class="card scrollx"><table>
            <thead><tr><th>PR</th><th>Would merge</th><th>Blockers</th><th>Signals</th><th>Outcome</th><th>Verdict</th></tr></thead>
            <tbody>{r.prs.length ? r.prs.map((p) => <tr key={p.handoff_id} id={`pr-${p.handoff_id}`}>
              <td>{p.pr > 0 && <Ext href={`https://github.com/${p.repo}/pull/${p.pr}`}>{base(p.repo)} #{p.pr}</Ext>}<br /><a class="muted" href={`/lastmile/${p.handoff_id}`}>{p.handoff_id}</a></td>
              <td>{isZero(p.verdict_at) ? <span class="muted">none yet</span> : <Chip tone={p.would_merge ? "ok" : "bad"}>{p.would_merge ? "yes" : "no"}</Chip>}</td>
              <td>{(p.blockers ?? []).join("; ")}</td>
              <td><SignalLine s={p.signals} /></td>
              <td>{p.outcome || "open"}{p.merged_by && ` by ${p.merged_by}`}{!isZero(p.outcome_at) && <><br /><span class="muted">{stamp(p.outcome_at!)}</span></>}</td>
              <td class="muted">{!isZero(p.verdict_at) && <><Ago t={p.verdict_at} /><br /><a href={`/lastmile/shadow?handoff=${encodeURIComponent(p.handoff_id)}#${p.handoff_id}`}>history</a></>}</td></tr>)
              : <tr><td colSpan={6} class="muted">No PR observed yet. Handoffs with <code>merge=shadow</code> show up here.</td></tr>}</tbody>
          </table></div>
        </section>
      </>}
    </main>
  </Page>;
}

function SignalLine({ s }: { s: Signals }) {
  const approvers = s.approvers ?? [], risky = s.risky_paths ?? [];
  const rounds = Object.entries(s.fix_rounds ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return <>diff +{s.additions} −{s.deletions} · {s.files} files · {plural(approvers.length, "approval")}{s.approvals_stale && " (stale)"} · {s.unresolved} unresolved
    {" "}· behind {s.behind}{risky.length > 0 && ` · risky ${risky.join(", ")}`} · {rounds.map(([k, v]) => `${k} rounds ${v} · `)}flakes {s.flake_reruns} · {secs(s.time_in_review_sec)} in review</>;
}
