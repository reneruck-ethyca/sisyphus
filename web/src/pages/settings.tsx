// Settings: the config file's editable fields, and the agent skill.
import { useEffect, useState } from "preact/hooks";
import { get, post, put, type Field, type Settings as SettingsData, type SkillStatus, type Status } from "../api";
import { Loading, Page, useSkills } from "../components";
import { plural, profiles, withCurrent } from "../format";
import { useFetch } from "../live";

// shown is a field's value as the form holds it; a bool with Invert shows the other way round.
const shown = (f: Field) => (f.type === "bool" ? (f.value === true) !== !!f.invert : String(f.value ?? ""));
// saved turns a form value back into what the config file takes.
const saved = (f: Field, v: unknown) => (f.type === "bool" ? (v as boolean) !== !!f.invert : f.type === "number" ? Number(v) : v);

export function Settings({ status }: { status?: Status }) {
  const loaded = useFetch<SettingsData>("/api/settings");
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [base, setBase] = useState<Record<string, unknown>>({});
  const [msg, setMsg] = useState("");
  const [savedOK, setSavedOK] = useState(false);
  const [restarting, setRestarting] = useState(false);
  useEffect(() => {
    if (!loaded.data) return;
    const v = Object.fromEntries(loaded.data.fields.map((f) => [f.key, shown(f)]));
    setValues(v);
    setBase(v);
  }, [loaded.data]);

  if (!loaded.data) return <Page title="Settings" status={status} page="settings">{loaded.error ? <p class="notice">{loaded.error}</p> : <Loading />}</Page>;
  const st = loaded.data;
  const changed = st.fields.filter((f) => values[f.key] !== base[f.key]);
  const set = (key: string, v: unknown) => { setValues({ ...values, [key]: v }); setSavedOK(false); };

  // Picking a vendor offers its model family and reasoning efforts; a saved custom value stays visible.
  const vendor = String(values["review.agent.vendor"] ?? "");
  const options = (f: Field): string[] | undefined => {
    if (f.key === "review.agent.model") return withCurrent(profiles[vendor]?.models ?? [], String(values[f.key] ?? ""));
    if (f.key === "review.agent.effort") return withCurrent(profiles[vendor]?.efforts ?? [], String(values[f.key] ?? ""));
    return f.options && withCurrent(f.options, String(values[f.key] ?? ""));
  };
  const onVendor = (v: string) => { setValues({ ...values, "review.agent.vendor": v, "review.agent.model": "", "review.agent.effort": "" }); setSavedOK(false); };

  const save = async (e: Event) => {
    e.preventDefault();
    try {
      // Only changed fields are saved, so defaults aren't pinned into the file.
      await put("/api/settings", Object.fromEntries(changed.map((f) => [f.key, saved(f, values[f.key])])));
      setBase(values);
      setSavedOK(true);
      setMsg("Saved. Restart sisyphus to apply.");
    } catch (x) {
      setMsg((x as Error).message);
    }
  };
  const restart = async () => {
    setRestarting(true);
    await post("/api/restart").catch(() => {});
    setMsg("Restarting…");
    const until = Date.now() + 60_000;
    const back = async () => {
      try { if ((await fetch("/api/status")).ok) return location.reload(); } catch { /* not back yet */ }
      if (Date.now() > until) return setMsg("sisyphus hasn't come back after a minute. Check that it's running, then reload this page.");
      setTimeout(back, 1000);
    };
    setTimeout(back, 1500);
  };

  const field = (f: Field) => {
    const id = `f-${f.key}`, v = values[f.key];
    const opts = options(f);
    let control;
    if (f.type === "bool") control = <input type="checkbox" id={id} checked={v as boolean} onChange={(e) => set(f.key, e.currentTarget.checked)} />;
    else if (opts && (!f.suggest || f.key.startsWith("review.agent."))) control = <select id={id} value={String(v)}
      onChange={(e) => (f.key === "review.agent.vendor" ? onVendor : (x: string) => set(f.key, x))(e.currentTarget.value)}>{opts.map((o) => <option key={o}>{o}</option>)}</select>;
    else if (f.type === "number") control = <input type="number" step="any" id={id} value={String(v)} onInput={(e) => set(f.key, e.currentTarget.value)} />;
    else control = <><input id={id} value={String(v)} list={opts ? `o-${f.key}` : undefined} onInput={(e) => set(f.key, e.currentTarget.value)} />
      {opts && <datalist id={`o-${f.key}`}>{opts.map((o) => <option key={o} value={o} />)}</datalist>}</>;
    return <div class={"field" + (v !== base[f.key] ? " changed" : "")} key={f.key}>
      <label for={id}><b>{f.label}</b><span class="muted">{f.help}</span><small>{f.key}</small></label>
      <span class="control">{control}</span>
    </div>;
  };

  return <Page title="Settings" status={status} page="settings">
    <main class="settings">
      <div class="pagehead"><h1>Settings</h1>
        <div class="muted">Saved to <code>{st.path}</code>. Changes apply after a restart; repos, hooks and env stay in the file.</div></div>
      <SkillCard />
      <form id="settings" onSubmit={save}>
        {st.groups.map((g) => {
          const fs = st.fields.filter((f) => f.group === g);
          if (g === "Advanced") return <details class="fold" id="fold-advanced" key={g}><summary>Advanced
            <span class="muted">{fs.map((f) => f.label).join(", ")}</span></summary>{fs.map(field)}</details>;
          return <section class="card" aria-labelledby={`g-${g}`} key={g}>
            <div class="card-h"><h2 id={`g-${g}`}>{g}</h2></div>{fs.map(field)}</section>;
        })}
      </form>
    </main>
    {(changed.length > 0 || savedOK || msg) && <div class="savebar" role="region" aria-label="Unsaved changes">
      <span role="status">{changed.length && !savedOK ? plural(changed.length, "unsaved change") : msg}</span>
      {savedOK ? <button type="button" class="btn" disabled={restarting} onClick={restart}>Restart sisyphus</button> : <>
        <button type="button" class="btn quiet" onClick={() => { setValues(base); setMsg(""); }}>Discard</button>
        <button type="submit" class="btn" form="settings">Save</button></>}
    </div>}
  </Page>;
}

const skillLabel = { absent: "not installed", current: "up to date", stale: "update available" };

// SkillCard installs, updates or removes the handoff skill per agent. Actions run in the background;
// while one runs the card asks how it went every half second.
function SkillCard() {
  const [st, setSt] = useSkills();
  const [err, setErr] = useState("");
  useEffect(() => {
    if (!st?.busy) return;
    const t = setTimeout(async () => {
      try { setSt(await get<SkillStatus>("/api/skill")); setErr(""); }
      catch (e) { setErr((e as Error).message); setSt({ ...st }); } // keeps polling: sisyphus may be restarting
    }, 500);
    return () => clearTimeout(t);
  }, [st]);
  const act = async (id: string, action: string) => {
    try { setSt(await post<SkillStatus>(`/api/skill/${id}/${action}`)); setErr(""); } catch (e) { setErr((e as Error).message); }
  };
  const agents = st?.agents ?? [];
  const note = err || (st?.error ? "That failed: " + st.error : st && !agents.length ? "Neither Claude Code nor Codex was found on this machine." : "");
  return <section class="card" aria-labelledby="g-skill">
    <div class="card-h"><h2 id="g-skill">Agent skill</h2>
      <span class="muted">Teaches coding agents when and how to hand a branch to TLM (<code>handing-off-to-tlm</code>).</span></div>
    <ul class="skill-agents">{agents.map((a) => {
      const btn = (text: string, action: string, quiet = false) => {
        const spinning = st?.busy === `${a.id}/${action}`;
        return <button type="button" class={"btn small" + (quiet ? " quiet" : "") + (spinning ? " spinning" : "")} disabled={!!st?.busy}
          aria-label={spinning ? `${text}: working` : undefined} onClick={() => act(a.id, action)}>{text}</button>;
      };
      return <li key={a.id}><b>{a.name}</b><span class={"chip" + ({ stale: " changes", current: " ok", absent: "" }[a.state])}>{skillLabel[a.state]}</span>
        {a.state === "absent" && btn("Install", "install")}{a.state === "stale" && btn("Update", "install")}{a.state !== "absent" && btn("Uninstall", "uninstall", true)}</li>;
    })}</ul>
    {note && <p class="muted skill-msg" role="status">{note}</p>}
  </section>;
}
