// Home: what needs you, then reviews, the last mile, capacity and the shadow report.
import type { ShadowReport, Status } from "../api";
import { Ago, BudgetMeter, Chip, Ext, Loading, Notices, Page } from "../components";
import { base, clock, isZero, lmCount, pct, plural, runPath, secs } from "../format";
import { useFetch } from "../live";
import { Approved } from "./lastmile";
import { DesignButton, runHref } from "./reviews";

export function Home({ status: s, error }: { status?: Status; error: string }) {
  const lastmileOn = !!s?.handoffs; // nil handoffs: the last mile is off
  const shadow = useFetch<ShadowReport>(lastmileOn ? "/api/lastmile/shadow" : "", s?.handoffs?.length);
  if (!s) return <Page title="Home"><Loading />{error && <p class="notice">{error}</p>}</Page>;
  const prs = s.prs ?? [], handoffs = s.handoffs ?? [];
  const inGroup = (g: string) => prs.filter((p) => p.group === g);
  const reviewing = inGroup("reviewing"), recent = (s.activity ?? []).slice(0, 3);
  const b = s.budget;
  return <Page title="Home" status={s}>
    <main class="home">
      <div class="meta-line"><h1>Home</h1><span>{plural(prs.length, "tracked PR")} · {plural(handoffs.length, "handoff")}</span></div>
      {error && <p class="notice">{error}</p>}

      <section aria-labelledby="needs"><div class="sh"><h2 id="needs">Needs you</h2><span class="muted">{plural(s.needs_count, "item")}</span></div>
        {s.last_error && <p class="notice">The last check failed: {s.last_error}</p>}
        <Notices status={s} />
        {s.needs_count > 0 ? <ol class="list hl card">
          {(s.needs_human ?? []).map((v) => <li key={v.id}><Chip tone="bad">needs-human</Chip><span class="src">Last mile</span>
            <span class="grow"><a href={`/lastmile/${v.id}`}>{v.title}</a><Approved v={v} /><span class="facts">{v.id}{v.pr_number ? ` · ${base(v.repo)} #${v.pr_number}` : ""}
              {v.prev_state && ` · was ${v.prev_state}`} · {v.reason}{v.pr_url && <> · <Ext href={v.pr_url}>PR ↗</Ext></>}</span></span>
            <span class="t"><Ago t={v.updated} /></span></li>)}
          {(s.failing ?? []).map((p) => <li key={p.id}><Chip tone={p.tone}>{p.verdict}</Chip><span class="src">{p.design ? "Design" : "Review"}</span>
            <span class="grow"><a href={`/prs/${p.repo}/${p.number}`}>{p.title}</a><span class="facts">{base(p.repo)} #{p.number}{p.design && p.author && ` · ${p.author}`} · {p.last_error || p.review_note}</span></span>
            <span class="t ends">{!isZero(p.verdict_at) && <Ago t={p.verdict_at} />}<DesignButton pr={p} /></span></li>)}
          {(s.awaiting_you ?? []).map((p) => <li key={p.id}><Chip tone={p.tone}>{p.verdict}</Chip><span class="src">Your review</span>
            <span class="grow"><a href={`/prs/${p.repo}/${p.number}`}>{p.title}</a><span class="facts">{base(p.repo)} #{p.number}{p.author && ` · ${p.author}`} · sisyphus reviewed · waiting for yours
              {p.url && <> · <Ext href={p.url}>PR ↗</Ext></>}</span></span>
            <span class="t"><Ago t={p.verdict_at} /></span></li>)}
        </ol>
        : !s.last_error && !s.runtime_error && s.reviews && <div class="card empty"><div><b>Nothing needs you right now</b>
          <div class="muted">Handoffs that need a person, design reviews, failed reviews and reviews waiting for yours show up here.</div></div></div>}
      </section>

      <div class="two">
        <section aria-labelledby="rv"><div class="sh"><h2 id="rv">Reviews</h2><a class="end" href="/reviews">All reviews →</a></div>
          <div class="card">
            <div class="counts">
              <a class="count bad" href="/reviews"><b>{inGroup("attention").length}</b><span>Needs attention</span></a>
              <a class="count live" href="/reviews"><b>{(s.reviewing ?? []).length}</b><span>Reviewing{s.queued > 0 && ` · queued ${s.queued}`}</span></a>
              <a class="count ok" href="/reviews"><b>{inGroup("reviewed").length}</b><span>Reviewed</span></a>
              <a class="count" href="/reviews"><b>{inGroup("waiting").length}</b><span>Not reviewed</span></a>
            </div>
            <div class="sub">Running now</div>
            {reviewing.length ? <ul class="mini">{reviewing.map((p) => <li key={p.id}><Chip tone={p.tone}>{p.verdict}</Chip>
              <a class="grow" href={runHref(p)}>{p.title}</a>
              <span class="t">{!isZero(p.verdict_at) ? <Ago t={p.verdict_at} /> : "waiting for a slot"}</span></li>)}</ul>
              : <p class="empty muted">No review running.</p>}
            <div class="sub">Latest activity</div>
            {recent.length ? <ul class="mini">{recent.map((a) => <li key={a.pr + a.run}><Chip tone={a.tone}>{a.label}</Chip>
              <a class="grow" href={runPath(a.pr, a.run)}>{a.title}</a><span class="t"><Ago t={a.at} /></span></li>)}</ul>
              : <p class="empty muted">No reviews yet.</p>}
          </div>
        </section>

        <section aria-labelledby="lmh"><div class="sh"><h2 id="lmh">Last mile</h2><a class="end" href="/lastmile">All handoffs →</a></div>
          <div class="card">
            <div class="counts">
              <a class="count" href="/lastmile"><b>{lmCount(handoffs)}</b><span>Open</span></a>
              <a class="count bad" href="/lastmile"><b>{lmCount(handoffs, "ci-red", "conflict")}</b><span>CI red / conflict</span></a>
              <a class="count changes" href="/lastmile"><b>{lmCount(handoffs, "awaiting-review", "feedback")}</b><span>Awaiting review · feedback</span></a>
              <a class="count ok" href="/lastmile"><b>{lmCount(handoffs, "approved")}</b><span>Approved</span></a>
            </div>
            <div class="sub">Agents in flight</div>
            {s.agents_running?.length ? <ul class="mini">{s.agents_running.map((v) => <li key={v.id}><Chip tone="live">{v.agent_running}</Chip>
              <a class="grow" href={`/lastmile/${v.id}`}>{v.title}</a><span class="t"><Ago t={v.agent_started} /></span></li>)}</ul>
              : <p class="empty muted">No agent running.</p>}
            <div class="sub">Recently merged</div>
            {s.merged?.length ? <ul class="mini">{s.merged.map((v) => <li key={v.id}><Chip tone="merged">merged</Chip>
              <a class="grow" href={`/lastmile/${v.id}`}>{v.title}{v.pr_number ? ` · ${base(v.repo)} #${v.pr_number}` : ""}</a><span class="t"><Ago t={v.updated} /></span></li>)}</ul>
              : <p class="empty muted">Nothing merged yet.</p>}
          </div>
        </section>
      </div>

      <div class="two">
        <section aria-labelledby="cap"><div class="sh"><h2 id="cap">Budget and capacity</h2><a class="end" href="/settings">Settings →</a></div>
          <div class="card">
            <div class="card-b stack">
              {s.budget_note ? <>
                <div class="meterrow"><span>5-hour window</span><BudgetMeter b={b} label={`${pct(b.used)} of the ${pct(b.share)} share used`} /><b>{pct(b.used)} of {pct(b.share)}</b></div>
                <dl><dt>Account</dt><dd>{pct(b.account)} of the window used by everyone</dd>
                  {!isZero(b.resets_at) && <><dt>Resets</dt><dd>{clock(b.resets_at)}</dd></>}
                  <dt>Held back</dt><dd>{s.budget_wait ? "yes" : "no"}</dd></dl>
              </> : <p class="muted">No 5-hour budget set.</p>}
              {s.slots_max > 0 && <>
                <div class="meterrow"><span>Run slots</span><meter min={0} max={s.slots_max} value={s.slots_used} aria-label={`${s.slots_used} of ${s.slots_max} slots in use`} /><b>{s.slots_used} of {s.slots_max}</b></div>
                <div class="muted">{plural((s.reviewing ?? []).length, "review")} · {plural((s.agents_running ?? []).length, "last-mile agent")} · {plural(s.queued, "review")} queued</div>
              </>}
            </div>
            <div class="card-f">Reviews and last-mile agents share <code>review.max_concurrent</code> and the 5-hour share.</div>
          </div>
        </section>

        {shadow.data && <section aria-labelledby="sm"><div class="sh"><h2 id="sm">Shadow merge</h2><a class="end" href="/lastmile/shadow">Shadow-merge report →</a></div>
          <div class="card">
            <table class="matrix">
              <thead><tr><th>Last verdict</th><th class="n">You merged</th><th class="n">You closed</th><th class="n">Still open</th></tr></thead>
              <tbody>
                <tr><td>would merge: yes</td><td class="n agree">{shadow.data.agreement.would_merge_yes.merged}</td><td class="n miss">{shadow.data.agreement.would_merge_yes.closed}</td><td class="n">{shadow.data.agreement.would_merge_yes.open}</td></tr>
                <tr><td>would merge: no</td><td class="n miss">{shadow.data.agreement.would_merge_no.merged}</td><td class="n agree">{shadow.data.agreement.would_merge_no.closed}</td><td class="n">{shadow.data.agreement.would_merge_no.open}</td></tr>
              </tbody>
            </table>
            <div class="card-f">{plural(shadow.data.totals.observed, "PR")} observed{shadow.data.totals.merged > 0 && ` · mean time in review ${secs(shadow.data.mean_time_in_review_sec)} (merged PRs)`} · agreement is by each PR's last verdict</div>
          </div>
        </section>}
      </div>
    </main>
  </Page>;
}
