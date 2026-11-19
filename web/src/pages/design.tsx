// The design review page: a PR's Markdown docs rendered, with its review threads beside them.
// Ported from the gh design-review extension; comments go to GitHub as the user.
import "github-markdown-css/github-markdown.css";
import type { JSX, RefObject } from "preact";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { post, prAPI, type PRView, type Status } from "../api";
import { Ago, Chip, Ext, Page } from "../components";
import { isZero, plural, rel } from "../format";
import { askLabel, designAPI, isStale, type Accept, type Comment, type DesignPR, type DocFile, type Mode, type DesignAPI, type Suggestion,
  type SuggestionStatus, type Suggestions, type Thread, type Verdict } from "../design/api";
import { blockForLine, blocksInRange, buildComment, placeThread, selectionRange, type Placement, type Range } from "../design/anchor";
import { blocks, decode, markChanged, renderMarkdown, renderMermaid, rewriteLinks, sanitize } from "../design/render";
import { defaultFilter, fromAI, inDoc, isOutdated, threadOf, type Filter } from "../design/suggestions";

type Composer = { range: Range; quote: string; below: number }; // below: px from the doc column's top
type Placed = { t: Thread; p: Placement; block?: HTMLElement; unread: number };
type Highlight = "since" | "all" | "none";
type Spot = { line: number; label: string }; // line: where it sits in the doc, 0 when it isn't on a block

// unreadIn counts a thread's comments by others that came after your review.
export const unreadIn = (t: Thread, since: string, viewer: string) =>
  since ? t.comments.filter((c) => c.state === "SUBMITTED" && c.author !== viewer && c.createdAt > since).length : 0;

export function Design({ status, pr: ref }: { status?: Status; pr: { repo: string; number: number } }) {
  const id = `${ref.repo}#${ref.number}`;
  const pv: PRView | undefined = status?.prs?.find((p) => p.id === id);
  const head = useRef("");
  const api = useMemo(() => designAPI(ref.repo, ref.number, () => head.current), [id]);

  const [pr, setPr] = useState<DesignPR | null>(null);
  const [path, setPath] = useState("");
  const [file, setFile] = useState<DocFile | null>(null);
  const [threads, setThreads] = useState<Thread[]>([]);
  const [sugg, setSugg] = useState<Suggestions | null>(null);
  const [filter, setFilter] = useState<Filter | null>(null);
  const view = filter ?? "posted";
  const [banner, setBanner] = useState("");
  const [toast, setToast] = useState("");
  const [composer, setComposer] = useState<Composer | null>(null);
  const [hover, setHover] = useState<HTMLElement | null>(null);
  const [sel, setSel] = useState<(Composer & { top: number; left: number }) | null>(null);
  const [layout, setLayout] = useState(0); // bumped to re-measure gutter positions
  const mainRef = useRef<HTMLDivElement>(null);
  const docRef = useRef<HTMLElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const pendingAnchor = useRef<string | null>(null); // set when a link opens another doc; "" means its top
  const pendingFocus = useRef<string | null>(null); // a thread to show once its doc has rendered

  // Since your review: what came in after it is marked new, and its commit is what changes are counted from.
  const d = pv?.design;
  const reviewed = d && !isZero(d.reviewed_at) ? d : undefined;
  // Mirrors tracker.DesignState.Updated: once you approved, only new commits count.
  const updated = !!reviewed && (!!d!.new_commits || ((d!.replies ?? 0) > 0 && d!.verdict !== "APPROVED"));
  const [mode, setMode] = useState<Highlight>(updated ? "since" : "all");
  useEffect(() => { if (updated) setMode("since"); }, [updated]); // the status may arrive after the first render

  const fail = (e: unknown) => {
    if (isStale(e)) setBanner("New commits were pushed. Reload to see them before commenting.");
    else setToast(e instanceof Error ? e.message : String(e));
  };

  // Writes send the head the page shows: the doc's, or the PR's while no doc has loaded (none to show, say).
  const showPR = (p: DesignPR) => {
    if (!file) head.current = p.headSha;
    setPr(p);
  };
  const loadPR = () => api.pr().then((p) => {
    showPR(p);
    setPath((cur) => {
      const fromHash = decode(location.hash.slice(1));
      const want = cur || fromHash;
      return p.docs.some((x) => x.path === want) ? want : p.docs[0]?.path ?? "";
    });
  }).catch(fail);
  const reloads = useRef(0); // only the latest reload may land: two quick actions answer out of order
  const showSugg = (s: Suggestions) => {
    setSugg(s);
    setFilter((f) => f ?? defaultFilter(s));
  };
  const reload = () => {
    const n = ++reloads.current;
    api.suggestions().then((s) => n === reloads.current && showSugg(s)).catch(fail);
    return Promise.all([api.threads(), api.pr()]).then(([t, p]) => {
      if (n === reloads.current) { setThreads(t); showPR(p); }
    }).catch(fail);
  };

  useEffect(() => { loadPR(); api.threads().then(setThreads).catch(fail); api.suggestions().then(showSugg).catch(fail); }, [id]);

  const runState = pv?.review.state ?? "";
  const ran = useRef(false);
  useEffect(() => {
    if (runState) ran.current = true;
    else if (ran.current) { ran.current = false; reload(); }
  }, [runState]);
  const [acting, setActing] = useState(false);
  const act = (action: "review" | "cancel") => {
    setActing(true);
    post(`${prAPI(ref)}/${action}`).catch(fail).finally(() => setActing(false));
  };

  useEffect(() => {
    if (!path) return;
    history.replaceState(null, "", location.pathname + "#" + encodeURIComponent(path));
    setFile(null);
    setComposer(null);
    let live = true; // a doc clicked before this one answers must not replace it
    api.file(path, reviewed?.reviewed_sha ?? "").then((f) => {
      if (!live) return;
      head.current = f.headSha;
      trackedHead.current = pvHead.current;
      setFile(f);
    }, (e) => live && fail(e));
    return () => void (live = false);
  }, [path, reviewed?.reviewed_sha]);

  // The tracker sees new commits on its next check, and may lag GitHub: only a head it learns of after the
  // doc loaded, and that isn't the doc's, means the doc is behind.
  const pvHead = useRef(pv?.head_sha);
  pvHead.current = pv?.head_sha;
  const trackedHead = useRef<string | undefined>(undefined);
  useEffect(() => {
    const h = pv?.head_sha;
    if (!file || !h) return;
    if (trackedHead.current === undefined) trackedHead.current = h; // the status arrived after the doc
    if (h !== file.headSha && h !== trackedHead.current) setBanner("New commits were pushed. Reload to see them.");
  }, [pv?.head_sha, file]);

  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(""), 8000);
    return () => clearTimeout(t);
  }, [toast]);

  useLayoutEffect(() => {
    const root = docRef.current;
    if (!root || !file || !pr) return;
    root.innerHTML = renderMarkdown(file.content);
    const [owner, name] = pr.headRepo.split("/");
    rewriteLinks(root, file.path, `${new URL(pr.url).origin}/${owner}/${name}/blob/${file.headSha}`, new Set(pr.docs.map((x) => x.path)),
      (p) => api.blob(p, file.headSha));
    const anchor = pendingAnchor.current;
    pendingAnchor.current = null;
    if (anchor !== null) scrollToAnchor(anchor, "auto");
    setLayout((l) => l + 1);
    renderMermaid(root).then(() => {
      setLayout((l) => l + 1);
      if (anchor) scrollToAnchor(anchor, "auto"); // diagrams above the target moved it
    }).catch(fail); // mermaid itself didn't load (the app was rebuilt under the tab, say)
  }, [file, pr?.url]);

  // Change bars: since your review, or everything the PR changed. In an added file every block is new, so
  // its whole-PR bars would say nothing.
  useLayoutEffect(() => {
    const root = docRef.current;
    if (!root || !file) return;
    const lines = mode === "none" ? [] : mode === "since" && file.sinceLines ? file.sinceLines
      : file.status === "added" ? [] : file.addedLines;
    markChanged(root, lines);
  }, [file, mode, layout]);

  useEffect(() => {
    const ro = new ResizeObserver(() => setLayout((l) => l + 1));
    if (docRef.current) ro.observe(docRef.current);
    return () => ro.disconnect();
  }, []);

  const here = useMemo(() => threads.filter((t) => t.path === path), [threads, path]);
  const placed: Placed[] = useMemo(() => {
    const bs = docRef.current ? blocks(docRef.current) : [];
    return here.map((t) => {
      const p = placeThread(t, file?.headSha ?? "");
      return { t, p, unread: unreadIn(t, reviewed?.reviewed_at ?? "", pr?.viewer ?? ""),
        block: p.section === "doc" ? blockForLine(bs, p.line)?.el : undefined };
    });
  }, [here, file, layout, reviewed?.reviewed_at, pr?.viewer]);

  // Innermost blocks covered by each thread's lines.
  const commented = useMemo(() => {
    const m = new Map<HTMLElement, Thread[]>();
    const bs = docRef.current ? blocks(docRef.current) : [];
    for (const x of placed) {
      if (x.p.section !== "doc") continue;
      for (const b of blocksInRange(bs, x.p.start, x.p.line)) m.set(b.el, [...(m.get(b.el) ?? []), x.t]);
    }
    return m;
  }, [placed]);

  useLayoutEffect(() => {
    const root = docRef.current;
    if (!root) return;
    root.querySelectorAll(".commented").forEach((e) => e.classList.remove("commented"));
    if (view !== "ai") commented.forEach((_, el) => el.classList.add("commented"));
  }, [commented, view]);

  const docs = pr?.docs.map((x) => x.path) ?? [];
  const items = sugg?.items ?? [];
  const ai = useMemo(() => fromAI(items), [sugg]);
  const spot = (s: Suggestion): Spot => {
    const t = s.kind === "reply" ? threadOf(s, threads) : undefined;
    const p = t && placeThread(t, file?.headSha ?? "");
    const [a, b] = p ? (p.section === "doc" ? [p.start, p.line] : [0, 0]) : [s.start_line ?? s.line ?? 0, s.line ?? 0];
    const lines = b ? `L${a}${a !== b ? `–${b}` : ""}` : "Whole file";
    if (inDoc(s, docs, threads)) return { line: b, label: lines };
    const where = t?.path ?? s.path;
    return { line: 0, label: where ? `${where}${b ? " " + lines : ""}` : "thread not found" };
  };
  const mine = items.filter((s) => inDoc(s, docs, threads) === path);
  const outside = items.filter((s) => !inDoc(s, docs, threads));

  // Blocks with open suggestions, unless only posted comments show.
  const aiBlocks = useMemo(() => {
    const m = new Map<HTMLElement, Suggestion[]>();
    if (view === "posted" || !docRef.current) return m;
    const bs = blocks(docRef.current);
    for (const s of mine) {
      const el = s.status === "open" && spot(s).line ? blockForLine(bs, spot(s).line)?.el : undefined;
      if (el) m.set(el, [...(m.get(el) ?? []), s]);
    }
    return m;
  }, [placed, sugg, view]);
  useLayoutEffect(() => {
    const root = docRef.current;
    if (!root) return;
    root.querySelectorAll(".ai-block").forEach((e) => e.classList.remove("ai-block"));
    aiBlocks.forEach((_, el) => el.classList.add("ai-block"));
  }, [aiBlocks]);

  // Every thread with something new, across docs, in reading order: "Next unread" walks them.
  const unread = useMemo(() => {
    if (!reviewed || !pr) return [];
    const order = pr.docs.map((x) => x.path);
    return threads.filter((t) => unreadIn(t, reviewed.reviewed_at, pr.viewer) > 0)
      .sort((a, b) => order.indexOf(a.path) - order.indexOf(b.path) || (a.line ?? 0) - (b.line ?? 0));
  }, [threads, reviewed?.reviewed_at, pr]);
  const newComments = useMemo(() => unread.reduce((n, t) => n + unreadIn(t, reviewed!.reviewed_at, pr!.viewer), 0), [unread]);
  const [cursor, setCursor] = useState(-1);
  const nextUnread = () => {
    const i = (cursor + 1) % unread.length, t = unread[i];
    setCursor(i);
    if (t.path === path) focusThread(t.id);
    else { pendingFocus.current = t.id; setPath(t.path); }
  };
  useEffect(() => {
    if (pendingFocus.current && placed.some((x) => x.t.id === pendingFocus.current)) {
      focusThread(pendingFocus.current);
      pendingFocus.current = null;
    }
  }, [placed, view]);

  const scrollToAnchor = (anchor: string, behavior: ScrollBehavior = "smooth") => {
    const root = docRef.current!;
    (anchor ? root.querySelector(`[id="${CSS.escape(anchor)}"]`) : root)?.scrollIntoView({ block: "start", behavior });
  };

  const onDocClick = (e: MouseEvent) => {
    // Links to another doc in the PR open it here; in-doc anchors scroll.
    const a = (e.target as Element).closest("a");
    const href = a?.getAttribute("href");
    if (a?.dataset.doc !== undefined) {
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
      e.preventDefault();
      if (a.dataset.doc === path) return scrollToAnchor(a.dataset.anchor!);
      pendingAnchor.current = a.dataset.anchor!;
      setPath(a.dataset.doc);
      return;
    }
    if (href?.startsWith("#")) {
      e.preventDefault();
      scrollToAnchor(decode(href.slice(1)));
      return;
    }
    const el = (e.target as Element).closest<HTMLElement>(".commented");
    // Leave link clicks and text selections (the user is about to comment) alone.
    if (!el || !docRef.current!.contains(el) || a || !getSelection()?.isCollapsed) return;
    const ts = commented.get(el);
    if (ts) focusThread(ts[0].id);
  };

  // Preact 11 no longer appends "px" to numeric styles.
  const yOf = (el: Element) => el.getBoundingClientRect().top - mainRef.current!.getBoundingClientRect().top;
  const topOf = (el: Element) => `${yOf(el)}px`;

  const highlight = (r: Range | null) => {
    const root = docRef.current!;
    root.querySelectorAll(".hl").forEach((e) => e.classList.remove("hl"));
    if (r) blocksInRange(blocks(root), r.start, r.end).forEach((b) => b.el.classList.add("hl"));
  };
  const jump = (line: number) => blockForLine(blocks(docRef.current!), line)?.el.scrollIntoView({ block: "center", behavior: "smooth" });
  const focusThread = (tid: string) => focusEl("thread-" + tid);
  const focusEl = (eid: string) => {
    const el = document.getElementById(eid);
    if (!el) return;
    if (el instanceof HTMLDetailsElement) el.open = true;
    el.scrollIntoView({ block: "center", behavior: "smooth" });
    el.classList.remove("flash");
    void el.offsetWidth;
    el.classList.add("flash");
  };

  const onMouseUp = () => requestAnimationFrame(() => {
    const s = getSelection();
    const range = s && selectionRange(s, docRef.current!);
    if (!s || !range) return setSel(null);
    const rect = s.getRangeAt(0).getBoundingClientRect();
    const box = mainRef.current!.getBoundingClientRect();
    setSel({ range, quote: s.toString(), top: rect.top - box.top - 40, left: rect.left - box.left, below: rect.bottom - box.top + 8 });
  });
  const openComposer = (c: Composer) => {
    setComposer(c);
    setSel(null);
    getSelection()?.removeAllRanges();
  };

  const badges = new Map<HTMLElement, Placed[]>();
  if (view !== "ai") for (const x of placed) if (x.block) badges.set(x.block, [...(badges.get(x.block) ?? []), x]);

  // Accepting places a doc comment as the composer would; anything else goes as the AI wrote it.
  const draftOf = (s: Suggestion, body: string): Accept => {
    if (s.kind === "reply") return { body };
    if (!s.line) return { body, subjectType: "FILE" };
    if (!file || inDoc(s, docs, threads) !== file.path) return { body, subjectType: "LINE", line: s.line, startLine: s.start_line };
    const range = { start: s.start_line ?? s.line, end: s.line };
    const quote = blocksInRange(blocks(docRef.current!), range.start, range.end).map((b) => b.el.textContent).join("\n");
    return buildComment(range, quote, body, file.hunks, file.headSha);
  };
  const showComment = (gid: string) => {
    setFilter("posted");
    const t = threads.find((x) => x.comments.some((c) => c.id === gid));
    if (!t || !docs.includes(t.path)) return;
    pendingFocus.current = t.id;
    if (t.path !== path) setPath(t.path);
  };
  const card = (s: Suggestion) => {
    const t = s.kind === "reply" ? threadOf(s, threads) : undefined;
    return <SuggestionCard key={s.id} s={s} spot={spot(s)} outdated={isOutdated(s, file?.headSha ?? pr?.headSha ?? "")}
      replyTo={t?.comments.find((c) => String(s.comment_id) === c.databaseId)}
      api={api} reload={reload} draftOf={draftOf} jump={jump} onShow={showComment} />;
  };
  const undecided = (s: Suggestion) => s.status === "open" || s.status === "posting";
  const byLine = (xs: Suggestion[]) => xs.map((s) => ({ s, line: spot(s).line })).sort((a, b) => a.line - b.line);
  const latest = sugg?.runs.reduce<Suggestions["runs"][number] | undefined>((a, r) => (!a || r.at > a.at ? r : a), undefined);
  const ofLatest = items.filter((s) => s.run === latest?.run);
  const shownOutside = view === "ai" ? outside : outside.filter(undecided);
  const ask = pv && askLabel(pv);

  const title = pr?.title ?? id;
  const back = `/prs/${ref.repo}/${ref.number}`;
  const pending = pr?.pending?.count ?? 0;

  return <Page title={`Design · ${title}`} status={status} page="reviews">
    <div class="dr-bar">
      <a href={back}>← {ref.repo.split("/")[1]} #{ref.number}</a>
      <b class="grow">{title}</b>
      {!updated && <label class="toggle"><input type="checkbox" checked={mode !== "none"}
        onChange={(e) => setMode(e.currentTarget.checked ? "all" : "none")} /> Show changes</label>}
      {sugg && <>
        <span>Show</span>
        <div class="seg" role="group" aria-label="Which comments to show">
          {([["ai", `AI suggestions · ${items.filter((s) => s.status === "open").length}`], ["posted", `Posted · ${threads.length}`], ["all", "All"]] as const)
            .map(([k, l]) => <button key={k} type="button" aria-pressed={view === k} onClick={() => setFilter(k)}>{l}</button>)}
        </div>
      </>}
      {pending > 0 && <Chip tone="changes">Pending review · {plural(pending, "comment")}</Chip>}
      {ask && <button class="btn quiet small" disabled={ask.disabled || acting} onClick={() => act("review")}>{ask.text}</button>}
      {pr && <Ext class="btn quiet small" href={pr.url}>GitHub ↗</Ext>}
      <button class="btn small" disabled={!pr} onClick={() => dialogRef.current!.showModal()}>Submit review</button>
    </div>
    {updated && <div class="dr-since" role="status">
      <b>Since your review <Ago t={reviewed!.reviewed_at} />:</b>
      <span>{[d!.new_commits && "new commits", newComments > 0 && plural(newComments, "new comment")].filter(Boolean).join(" · ") || "nothing new on the docs"}</span>
      <span class="grow" />
      <span>Highlight</span>
      <div class="seg" role="group" aria-label="Which changes to highlight">
        {([["since", "Since my review"], ["all", "Whole PR"], ["none", "None"]] as const).map(([k, l]) =>
          <button key={k} type="button" aria-pressed={mode === k} onClick={() => setMode(k)}>{l}</button>)}
      </div>
      {unread.length > 0 && <button class="btn quiet small" type="button" onClick={nextUnread}>Next unread ↓</button>}
    </div>}
    {(runState === "running" || runState === "queued") && <div class="dr-since" role="status">
      <Chip tone="live">AI review {runState}</Chip>
      <span class="grow">Its suggestions show up here when it finishes; nothing is posted to GitHub.</span>
      <button class="btn quiet small" type="button" disabled={acting} onClick={() => act("cancel")}>Cancel run</button>
    </div>}
    {banner && <p class="notice dr-banner">{banner} <button class="linkbtn" onClick={() => location.reload()}>Reload</button></p>}
    <div class="dr">
      <nav class="dr-tree" aria-label="Docs in this PR">
        <h2>Docs in this PR</h2>
        {pr && <Tree docs={pr.docs} path={path} onSelect={setPath}
          news={new Set(unread.map((t) => t.path))} counts={countBy(threads, (t) => t.path)} />}
        {pr && !pr.docs.length && <p class="muted">This PR changes no Markdown docs.</p>}
      </nav>
      <div ref={mainRef} class={"dr-doc" + (mode === "since" && file?.sinceLines ? " since" : "")} onMouseUp={onMouseUp} onClick={onDocClick}
        onMouseLeave={() => setHover(null)}
        onMouseOver={(e) => {
          const b = (e.target as Element).closest<HTMLElement>("[data-line-start]");
          if (b && docRef.current!.contains(b)) setHover(b);
        }}>
        {!file && <div class="skeleton">{[90, 70, 80, 60, 75].map((w) => <div style={{ width: w + "%" }} />)}</div>}
        <article ref={docRef} class="markdown-body" />
        {file && hover && (
          <button class="plus" title="Comment on this block" aria-label="Comment on this block" style={{ top: topOf(hover) }}
            onClick={() => openComposer({ range: { start: +hover.dataset.lineStart!, end: +hover.dataset.lineEnd! }, quote: hover.textContent ?? "",
              below: hover.getBoundingClientRect().bottom - mainRef.current!.getBoundingClientRect().top + 8 })}>+</button>
        )}
        {file && [...badges].map(([el, xs]) => {
          const news = xs.reduce((n, x) => n + x.unread, 0);
          return <button class={"badge" + (news ? " new" : "")} style={{ top: topOf(el) }} onClick={() => focusThread(xs[0].t.id)}
            onMouseEnter={() => highlight({ start: +el.dataset.lineStart!, end: +el.dataset.lineEnd! })} onMouseLeave={() => highlight(null)}
            aria-label={plural(xs.length, "thread") + (news ? `, ${news} new` : "")}>
            {news ? `${news} new` : xs.length}
          </button>;
        })}
        {file && [...aiBlocks].map(([el, ss]) =>
          <button class="badge ai" style={{ top: `${yOf(el) + (badges.has(el) ? 26 : 0)}px` }} onClick={() => focusEl("sugg-" + ss[0].id)}
            aria-label={plural(ss.length, "AI suggestion")}>AI {ss.length}</button>)}
        {sel && (
          <button class="btn small sel-btn" style={{ top: `${sel.top}px`, left: `${sel.left}px` }}
            onMouseDown={(e) => e.preventDefault()} onClick={() => openComposer(sel)}>Comment</button>
        )}
        {file && composer && (
          <ComposerBox c={composer} file={file} api={api} fail={fail}
            onDone={() => { setComposer(null); reload(); }} onCancel={() => setComposer(null)} />
        )}
      </div>
      <aside class="dr-panel" aria-label="Comments">
        {file && view === "ai" && latest && <section>
          <h3>AI review</h3>
          <div class="dr-sugg summary">
            <div class="who"><Chip tone="merged">AI</Chip><b>Run <Ago t={latest.at} /></b><span>on {latest.head.slice(0, 7)}</span></div>
            <div class="markdown-body" dangerouslySetInnerHTML={{ __html: renderMarkdown(latest.body) }} />
            <div class="who"><span>{ofLatest.filter((s) => !undecided(s)).length} of {ofLatest.length} decided</span>
              {latest.event && <span class="end">Suggested verdict: {verdicts.find(([k]) => k === latest.event)?.[1] ?? latest.event}</span>}</div>
          </div>
        </section>}
        {file && view !== "posted" && shownOutside.length > 0 && <section>
          <h3>Outside the docs · {shownOutside.length}</h3>{shownOutside.map(card)}
        </section>}
        {file && view === "ai" && <section>
          <h3>Suggestions in this doc{mine.length ? ` · ${mine.length}` : ""}</h3>
          {mine.length ? byLine(mine).map((x) => card(x.s))
            : <p class="muted">{items.length ? "No AI suggestions on this doc." : "No AI suggestions. Ask the AI for a review to get some."}</p>}
        </section>}
        {file && view !== "ai" && <Panel placed={placed} api={api} reload={reload} fail={fail} highlight={highlight} jump={jump} ai={ai}
          extra={view === "all" ? byLine(mine.filter(undecided)).map((x) => ({ line: x.line, node: card(x.s) })) : []} />}
      </aside>
    </div>
    <SubmitDialog ref_={dialogRef} pr={pr} threads={threads} api={api} fail={fail} author={pv?.author}
      onDone={(event) => { reload(); setToast(event === null ? "Pending review discarded."
        : event === "APPROVE" ? "Approved. Sisyphus puts it back in Needs you on new commits."
        : "Submitted. Sisyphus puts it back in Needs you on new commits or comments."); }} />
    {toast && <div class="dr-toast" role="status" onClick={() => setToast("")}>{toast}</div>}
  </Page>;
}

const countBy = <T,>(xs: T[], key: (x: T) => string) => xs.reduce((m, x) => m.set(key(x), (m.get(key(x)) ?? 0) + 1), new Map<string, number>());

type TreeNode = { name: string; doc?: DesignPR["docs"][number]; kids: Map<string, TreeNode> };

function buildTree(docs: DesignPR["docs"]): TreeNode {
  const root: TreeNode = { name: "", kids: new Map() };
  for (const doc of docs) {
    let n = root;
    for (const seg of doc.path.split("/")) {
      if (!n.kids.has(seg)) n.kids.set(seg, { name: seg, kids: new Map() });
      n = n.kids.get(seg)!;
    }
    n.doc = doc;
  }
  return root;
}

type TreeProps = { path: string; onSelect: (p: string) => void; news: Set<string>; counts: Map<string, number> };

// Directories sort before files; a directory holding only one directory shows as "a/b/c".
function TreeLevel({ node, ...p }: TreeProps & { node: TreeNode }) {
  const kids = [...node.kids.values()].sort((a, b) => +!!a.doc - +!!b.doc || a.name.localeCompare(b.name));
  return <ul>
    {kids.map((k) => {
      if (k.doc) {
        const doc = k.doc, n = p.counts.get(doc.path) ?? 0;
        return <li key={k.name}>
          <a href={"#" + encodeURIComponent(doc.path)} aria-current={doc.path === p.path ? "page" : undefined} title={doc.path}
            onClick={(e) => { e.preventDefault(); p.onSelect(doc.path); }}>
            <span>{p.news.has(doc.path) && <span class="dot" aria-label="new comments" />}{k.name}</span>
            <small>{doc.status}{n > 0 && ` · ${plural(n, "thread")}`}</small>
          </a>
        </li>;
      }
      let dir = k, name = k.name;
      while (!dir.doc && dir.kids.size === 1 && ![...dir.kids.values()][0].doc) {
        dir = [...dir.kids.values()][0];
        name += "/" + dir.name;
      }
      return <li key={name}><details open><summary>{name}</summary><TreeLevel node={dir} {...p} /></details></li>;
    })}
  </ul>;
}

const Tree = ({ docs, ...p }: TreeProps & { docs: DesignPR["docs"] }) =>
  <TreeLevel node={useMemo(() => buildTree(docs), [docs])} {...p} />;

type PanelProps = {
  placed: Placed[]; api: DesignAPI; ai: Set<string>; extra: { line: number; node: JSX.Element }[]; // extra: open suggestions, in line order
  reload: () => void; fail: (e: unknown) => void; highlight: (r: Range | null) => void; jump: (line: number) => void;
};

function Panel({ placed, extra, ...rest }: PanelProps) {
  const view = (x: Placed) => <ThreadView key={x.t.id} {...x} {...rest} />;
  const byLine = (xs: Placed[]) => xs.sort((a, b) => (a.p.section === "doc" ? a.p.start : 0) - (b.p.section === "doc" ? b.p.start : 0));
  const news = byLine(placed.filter((x) => x.unread > 0));
  const doc = [...byLine(placed.filter((x) => x.p.section === "doc" && !x.unread)).map((x) => ({ line: x.p.section === "doc" ? x.p.start : 0, node: view(x) })),
    ...extra].sort((a, b) => a.line - b.line);
  const general = placed.filter((x) => x.p.section === "general" && !x.unread);
  const outdated = placed.filter((x) => x.p.section === "outdated" && !x.unread);
  return <>
    {news.length > 0 && <section><h3>New since your review · {news.length}</h3>{news.map(view)}</section>}
    {general.length > 0 && <section><h3>On the whole file</h3>{general.map(view)}</section>}
    <section>
      <h3>In this doc{doc.length ? ` · ${doc.length}` : ""}</h3>
      {doc.length ? doc.map((x) => x.node) : !news.length && <p class="muted">No comments yet. Select text, or press + beside a block.</p>}
    </section>
    {outdated.length > 0 && <section><h3>Outdated</h3>{outdated.map(view)}</section>}
  </>;
}

function ComposerBox({ c, file, api, fail, onDone, onCancel }: {
  c: Composer; file: DocFile; api: DesignAPI; fail: (e: unknown) => void; onDone: () => void; onCancel: () => void;
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const fileLevel = buildComment(c.range, c.quote, "", file.hunks, file.headSha).subjectType === "FILE";
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.querySelector("textarea")?.focus({ preventScroll: true });
    ref.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, [c]);

  const post = async (mode: Mode) => {
    if (!text.trim() || busy) return;
    setBusy(true);
    try {
      await api.createThread({ path: file.path, mode, ...buildComment(c.range, c.quote, text, file.hunks, file.headSha) });
      onDone();
    } catch (e) {
      fail(e); // the draft stays in the textarea
    } finally {
      setBusy(false);
    }
  };

  return <div class="dr-composer" ref={ref} style={{ top: `${c.below}px` }}>
    <div class="muted small">Lines {c.range.start}{c.range.end !== c.range.start && `–${c.range.end}`}</div>
    {c.quote.trim() && <blockquote>{c.quote.trim().slice(0, 300)}</blockquote>}
    {fileLevel && <div class="note">{file.patchMissing
      ? "GitHub doesn't show this file's diff (too large), so this is posted as a comment on the file."
      : "Outside the PR's changes: this is posted as a comment on the file."}</div>}
    <label class="sr-only" for="dr-new">Your comment</label>
    <textarea id="dr-new" value={text} placeholder="Leave a comment (Markdown)" onInput={(e) => setText(e.currentTarget.value)}
      onKeyDown={(e) => {
        if (e.key === "Escape") onCancel();
        if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); post(e.shiftKey ? "now" : "review"); }
      }} />
    <div class="row">
      <button class="btn small" disabled={busy} onClick={() => post("review")} title="⌘↩">Add to review</button>
      <button class="btn quiet small" disabled={busy} onClick={() => post("now")} title="⇧⌘↩">Post now</button>
      <button class="btn quiet small end" onClick={onCancel}>Cancel</button>
    </div>
  </div>;
}

function ThreadView({ t, p, unread, api, ai, reload, fail, highlight, jump }: Placed & Omit<PanelProps, "placed" | "extra">) {
  const [reply, setReply] = useState("");
  const [busy, setBusy] = useState(false);
  const run = async (fn: () => Promise<unknown>, after?: () => void) => {
    setBusy(true);
    try { await fn(); after?.(); reload(); } catch (e) { fail(e); } finally { setBusy(false); }
  };
  const send = (mode: Mode) => reply.trim() && run(() => api.reply(t, reply, mode), () => setReply(""));
  const pendingThread = t.comments.every((c) => c.state === "PENDING");
  const firstNew = t.comments.length - unread;

  let label = "Whole file";
  if (p.section === "doc") label = `L${p.start}${p.line !== p.start ? `–${p.line}` : ""}`;
  if (p.section === "outdated") label = `was L${t.originalLine ?? "?"}`;

  return <details id={"thread-" + t.id} class={"dr-thread" + (unread ? " unread" : "")} open={!t.isResolved}
    onMouseEnter={() => p.section === "doc" && highlight({ start: p.start, end: p.line })} onMouseLeave={() => highlight(null)}>
    <summary>
      {unread > 0 && <Chip tone="live">{unread} new</Chip>}
      <a href="#" onClick={(e) => { e.preventDefault(); if (p.section === "doc") jump(p.line); }}>{label}</a>
      {p.section === "doc" && p.approximate && <span class="muted small">approximate</span>}
      {t.isResolved && <Chip>Resolved</Chip>}
      {t.isOutdated && p.section !== "outdated" && <Chip>Outdated</Chip>}
      <span class="muted small">{t.comments[0]?.author}</span>
    </summary>
    {t.comments.map((c, i) => <div class={"dr-comment" + (i >= firstNew ? " new" : "")} key={c.id}>
      <div class="who">
        {c.avatarUrl && <img src={c.avatarUrl} alt="" width={20} height={20} />}
        <b>{c.author}</b>
        {c.state === "PENDING" ? <Chip tone="changes">Pending</Chip>
          : <Ext href={c.url}><time dateTime={c.createdAt}>{rel(c.createdAt)}</time></Ext>}
        {ai.has(c.id) && <Chip tone="merged">from AI suggestion</Chip>}
        {i >= firstNew && <span class="newtag">new</span>}
      </div>
      <div class="markdown-body" dangerouslySetInnerHTML={{ __html: sanitize(c.bodyHTML) }} />
    </div>)}
    <label class="sr-only" for={"reply-" + t.id}>Reply</label>
    <textarea id={"reply-" + t.id} placeholder="Reply…" value={reply} onInput={(e) => setReply(e.currentTarget.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send(e.shiftKey ? "now" : "review"); }
      }} />
    <div class="row">
      <button class="btn small" disabled={busy} onClick={() => send("review")}>Add to review</button>
      {!pendingThread && <button class="btn quiet small" disabled={busy} onClick={() => send("now")}>Reply now</button>}
      {!pendingThread && <button class="btn quiet small end" disabled={busy} onClick={() => run(() => api.setResolved(t.id, !t.isResolved))}>
        {t.isResolved ? "Unresolve" : "Resolve"}
      </button>}
    </div>
  </details>;
}

const plain = (html: string) => new DOMParser().parseFromString(sanitize(html), "text/html").body.textContent?.trim() ?? "";

const decided: Partial<Record<SuggestionStatus, [string, string]>> = {
  review: ["In your review", "changes"], posted: ["Posted", "ok"], dismissed: ["Dismissed", ""],
};

function SuggestionCard({ s, spot, outdated, replyTo, api, reload, draftOf, jump, onShow }: {
  s: Suggestion; spot: Spot; outdated: boolean; replyTo?: Comment; api: DesignAPI; reload: () => void;
  draftOf: (s: Suggestion, body: string) => Accept; jump: (line: number) => void; onShow: (githubID: string) => void;
}) {
  const [text, setText] = useState(s.edited_body ?? s.body);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setErr("");
    try { await fn(); } catch (e) { setErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); reload(); }
  };
  const accept = (mode: Mode) => text.trim() && run(() => api.accept(s.id, { ...draftOf(s, text), mode }));
  const set = (to: SuggestionStatus) => run(() => api.setSuggestion(s.id, to));
  const at = <a href="#" onClick={(e) => { e.preventDefault(); if (spot.line) jump(spot.line); }}>{spot.label}</a>;
  const error = err && <p class="note" role="alert">{err}</p>;
  const body = <div class="markdown-body" dangerouslySetInnerHTML={{ __html: renderMarkdown(text) }} />;

  const done = decided[s.status];
  if (done) return <div id={"sugg-" + s.id} class="dr-sugg done">
    <div class="who">
      <Chip tone={done[1]}>{done[0]}</Chip>{at}<span class="short">{text}</span>
      {s.status === "dismissed" ? <button class="linkbtn" type="button" disabled={busy} onClick={() => set("open")}>Undo</button>
        : <button class="linkbtn" type="button" onClick={() => onShow(s.github_id ?? "")}>Show</button>}
    </div>
    {error}
  </div>;

  return <div id={"sugg-" + s.id} class="dr-sugg">
    <div class="who">
      <Chip tone="merged">AI suggestion</Chip>{at}
      <span>{s.kind === "reply" ? `reply to ${replyTo?.author ?? "a comment"}` : "new thread"}</span>
      {outdated && <Chip>Outdated · run on {s.head.slice(0, 7)}</Chip>}
    </div>
    {replyTo && <blockquote><b>{replyTo.author}:</b> {plain(replyTo.bodyHTML).slice(0, 200)}</blockquote>}
    {s.status === "posting" ? <>
      <p class="note">Posting was interrupted; check GitHub</p>
      {body}
      <div class="row">
        <button class="btn small" type="button" disabled={busy} onClick={() => set("posted")}>Mark posted</button>
        <button class="btn quiet small" type="button" disabled={busy} onClick={() => set("open")}>Back to open</button>
      </div>
    </> : outdated ? <>
      <p class="note">Made on an older version of the doc, so it can't be posted. Ask the AI again to review the current version.</p>
      {body}
      <div class="row"><button class="btn quiet small end" type="button" disabled={busy} onClick={() => set("dismissed")}>Dismiss</button></div>
    </> : <>
      <label class="sr-only" for={"sugg-text-" + s.id}>Suggestion text</label>
      <textarea id={"sugg-text-" + s.id} value={text} onInput={(e) => setText(e.currentTarget.value)} />
      <div class="row">
        <button class="btn small" type="button" disabled={busy} onClick={() => accept("review")}>Add to review</button>
        <button class="btn quiet small" type="button" disabled={busy} onClick={() => accept("now")}>{s.kind === "reply" ? "Reply now" : "Post now"}</button>
        <button class="btn quiet small end" type="button" disabled={busy} onClick={() => set("dismissed")}>Dismiss</button>
      </div>
    </>}
    {error}
  </div>;
}

const verdicts: [Verdict, string, string][] = [
  ["COMMENT", "Comment", "Feedback without a verdict"],
  ["APPROVE", "Approve", "Ready to merge as it is"],
  ["REQUEST_CHANGES", "Request changes", "Must be addressed before merging"],
];

function SubmitDialog({ ref_, pr, threads, api, fail, author, onDone }: {
  ref_: RefObject<HTMLDialogElement | null>; pr: DesignPR | null; threads: Thread[]; api: DesignAPI;
  fail: (e: unknown) => void; author?: string; onDone: (event: Verdict | null) => void;
}) {
  const [body, setBody] = useState("");
  const [event, setEvent] = useState<Verdict>("COMMENT");
  const [busy, setBusy] = useState(false);
  const pending = threads.flatMap((t) => t.comments.filter((c) => c.state === "PENDING").map((c) => ({ t, c })));
  const close = () => ref_.current!.close();
  const act = async (fn: () => Promise<unknown>, after: () => void) => {
    setBusy(true);
    try { await fn(); close(); after(); } catch (e) { fail(e); } finally { setBusy(false); }
  };
  // GitHub wants a body for a Comment or Request changes that carries no comments.
  const needsBody = event !== "APPROVE" && !pending.length && !body.trim();

  return <dialog ref={ref_} class="dr-dialog" aria-labelledby="dr-submit-h">
    <h2 id="dr-submit-h">Submit your review</h2>
    {pending.length > 0 ? <div>
      <div class="muted small">{plural(pending.length, "pending comment")} go{pending.length === 1 && "es"} out with it</div>
      <ul>{pending.map(({ t, c }) => <li key={c.id}><code>{t.path.split("/").pop()}</code> · {plain(c.bodyHTML).slice(0, 90)}</li>)}</ul>
    </div> : <p class="muted small">No pending comments: this posts the verdict and summary alone.</p>}
    <label for="dr-sum"><b>Summary</b> <span class="muted">(optional)</span></label>
    <textarea id="dr-sum" rows={4} value={body} onInput={(e) => setBody(e.currentTarget.value)} />
    <fieldset>
      <legend class="muted small">Verdict</legend>
      {verdicts.map(([k, l, help]) => <label key={k} class={"opt" + (event === k ? " on" : "")}>
        <input type="radio" name="dr-verdict" checked={event === k} onChange={() => setEvent(k)} />
        <span>{l}<small>{help}</small></span>
      </label>)}
    </fieldset>
    <p class="watch">Sisyphus then watches the PR and puts it back in <b>Needs you</b> when {author ?? "the author"} pushes a commit
      {event === "APPROVE" ? "" : " or someone comments"}.</p>
    <div class="row">
      <button class="btn" disabled={busy || !pr || needsBody} title={needsBody ? "Write a summary, or approve" : undefined} onClick={() => act(() => api.submit(event, body), () => { setBody(""); onDone(event); })}>Submit review</button>
      <button class="btn quiet" type="button" onClick={close}>Keep editing</button>
      {(pr?.pending?.count ?? 0) > 0 && <button class="btn quiet danger end" disabled={busy}
        onClick={() => confirm("Discard your pending review and all its comments?") && act(() => api.discard(), () => onDone(null))}>Discard pending</button>}
    </div>
  </dialog>;
}
