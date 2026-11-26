// One handoff (the right pane of the last mile): its state, the dirty-worktree panel, merge policy, reviewers, the PR, fix rounds and the timeline.
import { Fragment } from "preact";
import { useEffect, useState } from "preact/hooks";
import { HTTPError, post, put, session, type Dirty, type HandoffView } from "../api";
import { Ago, Chip, Ext } from "../components";
import { full, handoffIcon, handoffTone, plural, short, splitState, stamp } from "../format";
import { loadSession, saveSession } from "../live";
import { Actions, ApprovalNote, Approved, Reviewers, Rounds, useAct } from "./lastmile";

export function HandoffMain({ v }: { v: HandoffView }) {
  const dirty = v.state === "needs-human" && v.dirty && !v.agent_running ? v.dirty : undefined;
  const events = [...(v.events ?? [])].reverse(); // newest first
  return <main class="wide">
      <div class="pagehead">
        <div class="meta-line"><span class="muted">{v.id}</span></div>
        <h1>{v.title}</h1>
        <div class="meta-line"><Chip tone={handoffTone(v.state)}>{v.state}</Chip><Approved v={v} />{v.prev_state && <span>was {v.prev_state}</span>}
          <span>{v.repo}{v.pr_url && <> <Ext href={v.pr_url}>#{v.pr_number} ↗</Ext></>}</span>
          <span>branch <code>{v.branch}</code> onto <code>{v.base}</code></span>
          {v.waiting_on && <span>waiting on <a href={`/lastmile/${v.waiting_on}`}>{v.waiting_on}</a></span>}
          {!!v.parent_pr && <span>chained on #{v.parent_pr} (not handed off: watched, never merged)</span>}
          {v.snapshot?.stack && <span>GitHub stack #{v.snapshot.stack.number} · {v.snapshot.stack.position} of {v.snapshot.stack.size}</span>}
          {!!v.queue_kicks && <span>{plural(v.queue_kicks, "queue kick")}</span>}
          <span>updated <Ago t={v.updated} /></span></div>
        {v.agent_running && <p class="notice quiet"><Chip tone="live">{v.agent_running === "dirty" ? "agent" : `${v.agent_running} agent`}</Chip> running since <Ago t={v.agent_started} />
          {v.agent_running === "dirty" && ", finishing the uncommitted work. If it fails, the changes come back uncommitted."}</p>}
        {!dirty && v.reason && <p class={"notice" + (v.state !== "needs-human" ? " quiet" : "")}>{v.reason}{v.state === "needs-human" && <>{" "}<ApprovalNote v={v} /></>}</p>}
        {v.error && <p class="err">{v.error}</p>}{v.warning && <p class="muted">{v.warning}</p>}
        {dirty ? <DirtyPanel key={v.id + ":" + v.dirty_hash} v={v} d={dirty} /> : <>
          {v.state === "needs-human" && <p class="muted">Resume hands it back to TLM, which re-reads the PR from the start on its next tick.
            It doesn't fix the problem above: deal with that first{v.worktree && <> (worktree <code>{v.worktree}</code>)</>}, or it lands back here.</p>}
          <div class="actions"><Actions v={v} /></div>
        </>}
      </div>

      <div class="split">
        <div class="side">
          <section><h2>Merge policy</h2>
            <div class="card card-b">
              {v.shadow ? <>
                <p><b>Would merge: <span class={v.shadow.would_merge ? "y" : "n"}>{v.shadow.would_merge ? "yes" : "no"}</span></b>
                  {" "}<span class="muted">as of <Ago t={v.shadow.at} />, head <code>{short(v.shadow.head)}</code></span></p>
                {!!v.shadow.blockers?.length && <ul>{v.shadow.blockers.map((b, i) => <li key={i}>{b}</li>)}</ul>}
              </> : <p class="muted">No verdict yet{v.autonomy.merge !== "shadow" && `: this handoff runs with merge=${v.autonomy.merge}`}.</p>}
              <div class="actions"><a class="btn quiet small" href={`/lastmile/shadow?handoff=${encodeURIComponent(v.id)}#${v.id}`}>Shadow merge for this PR →</a></div>
            </div>
          </section>
          <ReviewersCard v={v} />
          {v.snapshot && <SnapshotCard v={v} />}
        </div>
        <div class="mainc">
          <section><h2>Fix rounds</h2><div class="card card-b rounds"><Rounds v={v} /></div></section>
          <section><h2>Timeline</h2>
            <div class="card">{events.length ? <ol class="timeline">{events.map((e, i) => <li key={i}>
              <time dateTime={e.time} title={full(e.time)}>{stamp(e.time)}</time>
              {e.kind === "state" ? (({ state, reason }) => <><Chip tone={handoffTone(state)}>{handoffIcon(state)} {state}</Chip><span class="text">{reason}</span></>)(splitState(e.text))
                : <><Chip>{e.kind}</Chip><span class="text">{e.text}</span></>}</li>)}</ol>
              : <p class="card-b muted">Nothing has happened yet.</p>}</div>
          </section>
        </div>
      </div>
    </main>;
}

function ReviewersCard({ v }: { v: HandoffView }) {
  const [logins, setLogins] = useState((v.reviewers ?? []).join(", "));
  const [err, setErr] = useState("");
  useEffect(() => setLogins((v.reviewers ?? []).join(", ")), [(v.reviewers ?? []).join(",")]);
  const save = async (e: Event) => {
    e.preventDefault();
    try {
      await put(`/api/handoffs/${encodeURIComponent(v.id)}/reviewers`, logins.split(",").map((s) => s.trim()).filter(Boolean));
      setErr("");
    } catch (x) {
      setErr((x as Error).message);
    }
  };
  return <section><h2>Reviewers</h2>
    <div class="card card-b">
      <p class="rvs"><Reviewers v={v} /></p>
      <p class="legend muted"><span><span class="rv">alice</span> requested by TLM</span><span><span class="rv human">+carol</span> added by a human</span>
        <span><span class="rv gone">bob</span> removed by a human, never re-added</span></p>
      <p class="muted">{v.reviewers_set ? "Explicit: the humans you named." : "Round-robin picks from the roster."}{v.reviewers_pending && " An edit waits for the next tick."}</p>
      <form class="inline" onSubmit={save}>
        <label class="muted" for="logins">Human reviewers, comma separated</label>
        <input id="logins" value={logins} autocomplete="off" onInput={(e) => setLogins(e.currentTarget.value)} />
        <button class="btn small" type="submit">Save</button>
      </form>
      {err && <p class="err">{err}</p>}
    </div>
  </section>;
}

const mark = (c: string) => c === "FAILURE" ? <span class="n">✗</span> : c === "SUCCESS" ? <span class="y">✓</span> : <>…</>;

function SnapshotCard({ v }: { v: HandoffView }) {
  const s = v.snapshot!;
  return <section><h2>Pull request</h2>
    <div class="card card-b"><dl>
      <dt>Head</dt><dd><code>{short(s.head)}</code> by {s.author}{s.draft && " · draft"}</dd>
      <dt>Checks</dt><dd>{s.ci === "green" ? <span class="y">✓</span> : s.ci === "red" ? <span class="n">✗</span> : <span class="muted">…</span>} {s.ci}
        {!!s.checks?.length && <details><summary class="muted">{plural(s.checks.length, "check")}</summary>
          {s.checks.map((c, i) => <Fragment key={i}>{mark(c.Conclusion)} <Ext href={c.URL}>{c.Name}</Ext> <span class="muted">{c.Status} {c.Conclusion}</span><br /></Fragment>)}</details>}</dd>
      {!!s.failures?.length && <><dt>Failures</dt><dd>{s.failures.map((f, i) => <Fragment key={i}>{f.name} <code>{f.fingerprint}</code>{f.flake && <> <span class="muted">flake</span></>}<br /></Fragment>)}</dd></>}
      <dt>Threads</dt><dd>{s.unresolved} unresolved{!!s.new_feedback?.length && <>, {s.new_feedback.length} new{s.new_feedback.map((f) => ` · ${f.author}${f.bot ? " (bot)" : ""}`)}</>}</dd>
      <dt>Mergeability</dt><dd>{s.mergeable} · {s.merge_state} · behind {s.behind} of <code>{s.base}</code></dd>
      <dt>Review</dt><dd>{s.review_decision === "APPROVED" ? <span class="y">✓</span> : s.review_decision === "CHANGES_REQUESTED" ? <span class="n">✗</span> : <span class="muted">…</span>}
        {" "}{s.review_decision || "no decision"}
        {(s.approvals ?? []).map((a) => <Fragment key={a.user}><br />{a.stale ? <span class="muted">↻</span> : <span class="y">✓</span>} {a.user} approved <code>{short(a.commit)}</code>
          {a.stale && <> <span class="n">stale</span></>}{a.bot && <> <span class="muted">bot</span></>}</Fragment>)}</dd>
      <dt>Diff</dt><dd>+{s.additions} −{s.deletions} in {plural(s.files?.length ?? 0, "file")}</dd>
    </dl></div>
  </section>;
}

// DirtyLinks are the escape hatches under the dirty panel: resume as is, or cancel.
function DirtyLinks({ v }: { v: HandoffView }) {
  const [busy, act] = useAct();
  return <><span class="muted">{v.reason}.</span>{" "}
    <button class="linkbtn" type="button" disabled={!!busy} onClick={() => act(`/api/handoffs/${encodeURIComponent(v.id)}/resume`)}>Resume as is</button>
    {!v.over && <> · <button class="linkbtn" type="button" disabled={!!busy} onClick={() => act(`/api/handoffs/${encodeURIComponent(v.id)}/cancel`)}>Cancel handoff</button></>}</>;
}

type Draft = { unticked: string[]; msg?: string; task: string };

// DirtyPanel lists the uncommitted files and offers commit, stash, an agent, or leaving it.
// Unticked files and drafts survive a reload (per tab, per worktree status).
function DirtyPanel({ v, d }: { v: HandoffView; d: Dirty }) {
  const key = `tlm-dirty:${v.id}:${v.dirty_hash}`;
  const [draft, setDraft] = useState<Draft>(() => loadSession(key, { unticked: [], task: "" }));
  const [open, setOpen] = useState(!d.dismissed);
  const [busy, setBusy] = useState("");
  const [diffs, setDiffs] = useState<Record<string, { text: string; err?: boolean; shown: boolean; added?: boolean }>>({});
  useEffect(() => setOpen(!d.dismissed), [d.dismissed]);
  const update = (p: Partial<Draft>) => { const n = { ...draft, ...p }; setDraft(n); saveSession(key, n); };
  const msg = draft.msg ?? d.message ?? ""; // the human's draft, else the model's subject, else empty
  const picked = d.files.filter((f) => !draft.unticked.includes(f.path));
  const n = picked.length;

  const act = async (action: string, body: object) => {
    setBusy(action);
    try {
      await post(`/api/handoffs/${encodeURIComponent(v.id)}/dirty/${action}`, { hash: v.dirty_hash, ...body });
      saveSession(key, undefined);
    } catch (e) {
      alert((e as Error).message);
      if (e instanceof HTTPError && e.status === 409) saveSession(key, undefined); // the worktree changed under the page
    }
    setBusy("");
  };
  const toggleDiff = async (path: string, code: string) => {
    const cur = diffs[path];
    if (cur && !cur.err) return setDiffs({ ...diffs, [path]: { ...cur, shown: !cur.shown } }); // a failed one loads again
    setDiffs((x) => ({ ...x, [path]: { text: "Loading…", shown: true } }));
    let text: string, ok: boolean;
    try {
      await session;
      const r = await fetch(`/api/handoffs/${encodeURIComponent(v.id)}/dirty/diff?path=${encodeURIComponent(path)}`);
      [text, ok] = [await r.text(), r.ok];
    } catch (e) {
      [text, ok] = [`Couldn't load the diff: ${(e as Error).message}. Click the file to try again.`, false];
    }
    setDiffs((x) => ({ ...x, [path]: { text, err: !ok, shown: true, added: code === "??" } }));
  };

  return <>
    {!open && <div class="dirty-banner"><span>⏸ {plural(d.files.length, "uncommitted file")} block{d.files.length === 1 && "s"} TLM</span>
      <button class="btn quiet small" type="button" onClick={() => setOpen(true)}>Review</button><DirtyLinks v={v} /></div>}
    <div class="card dirty" hidden={!open}>
      <div class="dirty-h"><b>Uncommitted changes block TLM</b>{v.worktree && <code class="muted">{v.worktree}</code>}</div>
      <div class="dirty-cols">
        <table>
          <colgroup><col class="chk" /><col class="code" /><col /><col class="num" /></colgroup>
          <thead><tr><th><input type="checkbox" aria-label="All files" checked={n === d.files.length} ref={(el) => { if (el) el.indeterminate = n > 0 && n < d.files.length; }}
            onChange={(e) => update({ unticked: e.currentTarget.checked ? [] : d.files.map((f) => f.path) })} /></th>
            <th colSpan={2}>{plural(d.files.length, "file")}</th><th /></tr></thead>
          <tbody>{d.files.map((f) => <Fragment key={f.path}>
            <tr><td><input type="checkbox" aria-label={`Commit ${f.path}`} checked={!draft.unticked.includes(f.path)}
              onChange={(e) => update({ unticked: e.currentTarget.checked ? draft.unticked.filter((p) => p !== f.path) : [...draft.unticked, f.path] })} /></td>
              <td><span class={"badge " + (f.code === "D" ? "d" : f.code === "A" || f.code === "??" ? "a" : "m")}>{f.code}</span></td>
              <td><button type="button" class="dirty-path" aria-expanded={!!diffs[f.path]?.shown} onClick={() => toggleDiff(f.path, f.code)}>{f.path}</button></td>
              <td class="num">{f.binary ? <span class="muted">binary</span> : <><span class="y">+{f.add}</span> <span class="n">−{f.del}</span></>}</td></tr>
            {diffs[f.path]?.shown && <tr class="diffrow"><td colSpan={4}><pre class="diff">{diffs[f.path].err ? diffs[f.path].text
              : diffs[f.path].text.split("\n").map((l, i) => <span key={i} class={diffs[f.path].added || (l.startsWith("+") && !l.startsWith("+++")) ? "add" : l.startsWith("-") && !l.startsWith("---") ? "del" : undefined}>{l + "\n"}</span>)}</pre></td></tr>}
          </Fragment>)}</tbody>
        </table>
        <div class="dirty-side">
          {d.err ? <p class="muted">Couldn't summarise: {d.err}</p> : d.summary ? <p>{d.summary}</p> : !d.message && <p class="muted analysing">Analysing…</p>}
          <label for="dirty-msg"><b>Commit message</b></label>
          <textarea id="dirty-msg" rows={3} placeholder={`WIP: ${v.branch}`} value={msg} onInput={(e) => update({ msg: e.currentTarget.value })} />
          <div class="actions">
            <button class="btn" type="button" disabled={!!busy || n === 0 || !msg.trim()} onClick={() => act("commit", { paths: picked.map((f) => f.path), message: msg })}>
              Commit {plural(n, "file")} &amp; resume</button>
            <button class="btn quiet" type="button" disabled={!!busy} onClick={() => act("stash", {})}>Stash all</button>
            <button class="linkbtn" type="button" disabled={!!busy} onClick={() => act("leave", {})}>Leave it</button></div>
          <p class="muted note">Pushes to #{v.pr_number}. Tests haven't run on these changes.</p>
          <details class="dirty-agent"><summary><b>Or hand it to an agent</b></summary>
            <label for="dirty-task" class="muted">Commits all {plural(d.files.length, "file")} locally with the message above, then a fix agent works from there.
              It pushes and resumes when done; if it fails, your changes come back uncommitted.</label>
            <textarea id="dirty-task" rows={3} placeholder="Default: finish it, make the tests pass, drop what doesn't belong in this PR." value={draft.task}
              onInput={(e) => update({ task: e.currentTarget.value })} />
            <div class="actions"><button class="btn quiet" type="button" disabled={!!busy}
              onClick={() => act("agent", { message: msg || `WIP: ${v.branch}`, task: draft.task })}>Start agent</button></div>
          </details>
        </div>
      </div>
      <div class="dirty-f"><DirtyLinks v={v} /></div>
    </div>
  </>;
}
