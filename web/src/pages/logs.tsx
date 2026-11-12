// Logs: sisyphus's recent log lines, then new ones as they come.
import { useEffect, useState } from "preact/hooks";
import type { LogEntry, Status } from "../api";
import { Page } from "../components";
import { stream, unstream } from "../live";

const keep = 1000;
const time = (t: string) => new Date(t).toLocaleTimeString([], { hour12: false });

export function Logs({ status }: { status?: Status }) {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [state, setState] = useState("connecting…");
  useEffect(() => {
    let dropped = false, follow = true;
    const src = stream("/api/logs/stream");
    src.onopen = () => {
      if (dropped) setEntries([]); // sisyphus may have restarted: seqs start over, the backlog is resent
      setState("live");
    };
    src.onerror = () => { dropped = true; setState("disconnected, retrying…"); };
    src.onmessage = (m) => {
      const e: LogEntry = JSON.parse(m.data);
      follow = innerHeight + scrollY >= document.body.scrollHeight - 20;
      setEntries((l) => (l.length && e.seq <= l[l.length - 1].seq ? l : [...l, e].slice(-keep)));
      if (follow) requestAnimationFrame(() => scrollTo(0, document.body.scrollHeight));
    };
    return () => unstream(src);
  }, []);
  return <Page title="Logs" status={status} page="logs">
    <div class="log">{entries.map((e) => <div key={e.seq} class={e.level}><span class="muted">{time(e.time)}</span> {e.level} {e.msg} <span class="muted">{e.attrs ?? ""}</span></div>)}</div>
    <p class="muted" style="padding: 0 20px">{state}</p>
  </Page>;
}
