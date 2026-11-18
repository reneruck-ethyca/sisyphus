// The design review page: a PR's Markdown docs rendered, with its review threads beside them.
// Ported from the gh design-review extension; comments go to GitHub as the user.
import "github-markdown-css/github-markdown.css";
import type { RefObject } from "preact";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import type { PRView, Status } from "../api";
import { Ago, Chip, Ext, Page } from "../components";
import { isZero, plural, rel } from "../format";
import { designAPI, isStale, type DesignPR, type DocFile, type Mode, type ReviewEvent, type Thread } from "../design/api";
import { blockForLine, blocksInRange, buildComment, placeThread, selectionRange, type Placement, type Range } from "../design/anchor";
import { blocks, decode, markChanged, renderMarkdown, renderMermaid, rewriteLinks, sanitize } from "../design/render";

type Composer = { range: Range; quote: string; below: number }; // below: px from the doc column's top
type Placed = { t: Thread; p: Placement; block?: HTMLElement; unread: number };
type Highlight = "since" | "all" | "none";

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
  const reload = () => {
    const n = ++reloads.current;
    return Promise.all([api.threads(), api.pr()]).then(([t, p]) => {
      if (n === reloads.current) { setThreads(t); showPR(p); }
    }).catch(fail);
  };

  useEffect(() => { loadPR(); api.threads().then(setThreads).catch(fail); }, [id]);

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
    commented.forEach((_, el) => el.classList.add("commented"));
  }, [commented]);

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
  }, [placed]);

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
  const topOf = (el: Element) => `${el.getBoundingClientRect().top - mainRef.current!.getBoundingClientRect().top}px`;

  const highlight = (r: Range | null) => {
    const root = docRef.current!;
    root.querySelectorAll(".hl").forEach((e) => e.classList.remove("hl"));
    if (r) blocksInRange(blocks(root), r.start, r.end).forEach((b) => b.el.classList.add("hl"));
  };
  const jump = (line: number) => blockForLine(blocks(docRef.current!), line)?.el.scrollIntoView({ block: "center", behavior: "smooth" });
  const focusThread = (tid: string) => {
    const el = document.getElementById("thread-" + tid) as HTMLDetailsElement | null;
    if (!el) return;
    el.open = true;
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
  for (const x of placed) if (x.block) badges.set(x.block, [...(badges.get(x.block) ?? []), x]);

  const title = pr?.title ?? id;
  const back = `/prs/${ref.repo}/${ref.number}`;
  const pending = pr?.pending?.count ?? 0;

  return <Page title={`Design · ${title}`} status={status} page="reviews">
    <div class="dr-bar">
      <a href={back}>← {ref.repo.split("/")[1]} #{ref.number}</a>
      <b class="grow">{title}</b>
      {!updated && <label class="toggle"><input type="checkbox" checked={mode !== "none"}
        onChange={(e) => setMode(e.currentTarget.checked ? "all" : "none")} /> Show changes</label>}
      {pending > 0 && <Chip tone="changes">Pending review · {plural(pending, "comment")}</Chip>}
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
        {sel && (
          <button class="btn small sel-btn" style={{ top: `${sel.top}px`, left: `${sel.left}px` }}
            onMouseDown={(e) => e.preventDefault()} onClick={() => openComposer(sel)}>Comment</button>
        )}
        {file && composer && (
          <ComposerBox c={composer} file={file} api={api} fail={fail}
            onDone={() => { setComposer(null); reload(); }} onCancel={() => setComposer(null)} />
        )}
      </div>
      <aside class="dr-panel" aria-label="Threads">
        {file && <Panel placed={placed} api={api} reload={reload} fail={fail} highlight={highlight} jump={jump} />}
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

type Api = ReturnType<typeof designAPI>;
type PanelProps = {
  placed: Placed[]; api: Api;
  reload: () => void; fail: (e: unknown) => void; highlight: (r: Range | null) => void; jump: (line: number) => void;
};

function Panel({ placed, ...rest }: PanelProps) {
  const view = (x: Placed) => <ThreadView key={x.t.id} {...x} {...rest} />;
  const byLine = (xs: Placed[]) => xs.sort((a, b) => (a.p.section === "doc" ? a.p.start : 0) - (b.p.section === "doc" ? b.p.start : 0));
  const news = byLine(placed.filter((x) => x.unread > 0));
  const doc = byLine(placed.filter((x) => x.p.section === "doc" && !x.unread));
  const general = placed.filter((x) => x.p.section === "general" && !x.unread);
  const outdated = placed.filter((x) => x.p.section === "outdated" && !x.unread);
  return <>
    {news.length > 0 && <section><h3>New since your review · {news.length}</h3>{news.map(view)}</section>}
    {general.length > 0 && <section><h3>On the whole file</h3>{general.map(view)}</section>}
    <section>
      <h3>In this doc{doc.length ? ` · ${doc.length}` : ""}</h3>
      {doc.length ? doc.map(view) : !news.length && <p class="muted">No comments yet. Select text, or press + beside a block.</p>}
    </section>
    {outdated.length > 0 && <section><h3>Outdated</h3>{outdated.map(view)}</section>}
  </>;
}

function ComposerBox({ c, file, api, fail, onDone, onCancel }: {
  c: Composer; file: DocFile; api: Api; fail: (e: unknown) => void; onDone: () => void; onCancel: () => void;
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

function ThreadView({ t, p, unread, api, reload, fail, highlight, jump }: Placed & Omit<PanelProps, "placed">) {
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

const verdicts: [ReviewEvent, string, string][] = [
  ["COMMENT", "Comment", "Feedback without a verdict"],
  ["APPROVE", "Approve", "Ready to merge as it is"],
  ["REQUEST_CHANGES", "Request changes", "Must be addressed before merging"],
];

function SubmitDialog({ ref_, pr, threads, api, fail, author, onDone }: {
  ref_: RefObject<HTMLDialogElement | null>; pr: DesignPR | null; threads: Thread[]; api: Api;
  fail: (e: unknown) => void; author?: string; onDone: (event: ReviewEvent | null) => void;
}) {
  const [body, setBody] = useState("");
  const [event, setEvent] = useState<ReviewEvent>("COMMENT");
  const [busy, setBusy] = useState(false);
  const pending = threads.flatMap((t) => t.comments.filter((c) => c.state === "PENDING").map((c) => ({ t, c })));
  const close = () => ref_.current!.close();
  const act = async (fn: () => Promise<unknown>, after: () => void) => {
    setBusy(true);
    try { await fn(); close(); after(); } catch (e) { fail(e); } finally { setBusy(false); }
  };
  const text = (html: string) => new DOMParser().parseFromString(sanitize(html), "text/html").body.textContent?.trim() ?? "";
  // GitHub wants a body for a Comment or Request changes that carries no comments.
  const needsBody = event !== "APPROVE" && !pending.length && !body.trim();

  return <dialog ref={ref_} class="dr-dialog" aria-labelledby="dr-submit-h">
    <h2 id="dr-submit-h">Submit your review</h2>
    {pending.length > 0 ? <div>
      <div class="muted small">{plural(pending.length, "pending comment")} go{pending.length === 1 && "es"} out with it</div>
      <ul>{pending.map(({ t, c }) => <li key={c.id}><code>{t.path.split("/").pop()}</code> · {text(c.bodyHTML).slice(0, 90)}</li>)}</ul>
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
