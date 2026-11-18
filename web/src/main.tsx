import { render } from "preact";
import "./style.css";
import { Page } from "./components";
import { useLocation, useStatus } from "./live";
import { Design } from "./pages/design";
import { Handoff } from "./pages/handoff";
import { Home } from "./pages/home";
import { LastMile } from "./pages/lastmile";
import { Logs } from "./pages/logs";
import { Reviews } from "./pages/reviews";
import { Settings } from "./pages/settings";
import { Shadow } from "./pages/shadow";

// route matches a path against a pattern like "/prs/:owner/:repo/:n"; null = no match.
function route(pattern: string, path: string): Record<string, string> | null {
  const ps = pattern.split("/"), xs = path.replace(/\/+$/, "").split("/");
  if (ps.length !== xs.length && !(pattern === "/" && path === "/")) return null;
  const out: Record<string, string> = {};
  for (let i = 0; i < ps.length; i++) {
    if (ps[i].startsWith(":")) out[ps[i].slice(1)] = decodeURIComponent(xs[i]);
    else if (ps[i] !== xs[i]) return null;
  }
  return out;
}

function App() {
  const url = useLocation();
  const { status, error } = useStatus();
  const path = url.pathname;
  let m: Record<string, string> | null;
  const pr = (p: Record<string, string>) => ({ repo: `${p.owner}/${p.repo}`, number: +p.n });

  if (path === "/") return <Home status={status} error={error} />;
  if (path === "/reviews") return <Reviews status={status} error={error} />;
  if ((m = route("/prs/:owner/:repo/:n", path))) return <Reviews status={status} error={error} pr={pr(m)} />;
  if ((m = route("/prs/:owner/:repo/:n/design", path))) return <Design key={m.owner + m.repo + m.n} status={status} pr={pr(m)} />;
  if ((m = route("/prs/:owner/:repo/:n/runs/:run", path))) return <Reviews status={status} error={error} pr={pr(m)} run={m.run} />;
  if (path === "/history") return <Reviews status={status} error={error} archive />;
  if ((m = route("/history/:owner/:repo/:n", path))) return <Reviews status={status} error={error} archive pr={pr(m)} />;
  if ((m = route("/history/:owner/:repo/:n/runs/:run", path))) return <Reviews status={status} error={error} archive pr={pr(m)} run={m.run} />;
  if (path === "/lastmile") return <LastMile status={status} />;
  if (path === "/lastmile/shadow") return <Shadow status={status} query={url.searchParams} />;
  if ((m = route("/lastmile/:id", path))) return <Handoff key={m.id} status={status} id={m.id} />;
  if (path === "/logs") return <Logs status={status} />;
  if (path === "/settings") return <Settings status={status} />;
  return <Page title="Not found" status={status}><main><h1>Not found</h1><p class="muted">Nothing lives at <code>{path}</code>. <a href="/">Home</a></p></main></Page>;
}

render(<App />, document.getElementById("app")!);
