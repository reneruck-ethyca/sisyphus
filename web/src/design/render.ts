import markdownIt, { type MarkdownIt } from "markdown-it";
import frontMatter from "markdown-it-front-matter";
import taskLists from "markdown-it-task-lists";
import footnote from "markdown-it-footnote";
import alerts from "markdown-it-github-alerts";
import anchor from "markdown-it-anchor";
import DOMPurify from "dompurify";

// Block tokens that get data-line-start / data-line-end (1-based, inclusive).
const BLOCKS = new Set([
  "paragraph_open", "heading_open", "list_item_open", "blockquote_open", "tr_open",
  "fence", "code_block", "html_block", "hr", "alert_open",
]);

function sourceLines(md: MarkdownIt) {
  // Pushed last, so it runs after github-alerts has turned blockquotes into alerts.
  md.core.ruler.push("source_lines", (state) => {
    const lines = state.src.split("\n");
    for (const t of state.tokens) {
      if (!t.map || !BLOCKS.has(t.type)) continue;
      const start = t.map[0] + 1;
      let end = t.map[1];
      while (end > start && !lines[end - 1].trim()) end--;
      t.attrSet("data-line-start", String(start));
      t.attrSet("data-line-end", String(end));
    }
  });

  const rules = md.renderer.rules;
  // These renderers drop token attrs, so wrap their output in a div that carries them.
  for (const name of ["fence", "code_block", "html_block"] as const) {
    const orig = rules[name]!;
    rules[name] = (tokens, idx, opts, env, self) => {
      const t = tokens[idx];
      const attrs = self.renderAttrs(t);
      if (name === "fence" && t.info.trim() === "mermaid") {
        return `<div class="mermaid-block" data-mermaid${attrs}><div class="mermaid-svg"></div>` +
          `<details><summary>View source</summary><pre><code>${md.utils.escapeHtml(t.content)}</code></pre></details></div>\n`;
      }
      const saved = t.attrs;
      t.attrs = null;
      const inner = orig(tokens, idx, opts, env, self);
      t.attrs = saved;
      return `<div${attrs}>${inner}</div>\n`;
    };
  }
  const alertOpen = rules.alert_open!;
  rules.alert_open = (tokens, idx, opts, env, self) =>
    alertOpen(tokens, idx, opts, env, self).replace("<div ", `<div${self.renderAttrs(tokens[idx])} `);
}

const md = markdownIt({ html: true, linkify: true })
  .use(frontMatter, () => {})
  .use(taskLists)
  .use(footnote)
  .use(alerts)
  // GitHub's slug rule, so in-doc links written against GitHub ("#41-what-…" for "4.1 What …") resolve.
  .use(anchor, { slugify: (s: string) => s.trim().toLowerCase().replace(/[^\p{L}\p{M}\p{N}\s_-]/gu, "").replace(/\s/g, "-") })
  .use(sourceLines);

// sanitize keeps a doc's (or a comment's) HTML from acting on the dashboard: no CSS, which could restyle or
// spoof it, and no forms, which would post to sisyphus's API as the user. GitHub strips both too.
export const sanitize = (html: string) => DOMPurify.sanitize(html, {
  FORBID_TAGS: ["style", "form", "button", "input", "select", "textarea", "option"],
  FORBID_ATTR: ["style", "action", "formaction"],
});

export const renderMarkdown = (src: string): string => sanitize(md.render(src));

// decode is decodeURIComponent that keeps what it can't decode (a lone "%" in a doc's link).
export function decode(s: string): string {
  try { return decodeURIComponent(s); } catch { return s; }
}

export type Block = { el: HTMLElement; s: number; e: number };

export function blocks(root: ParentNode): Block[] {
  return [...root.querySelectorAll<HTMLElement>("[data-line-start]")].map((el) => ({
    el, s: Number(el.dataset.lineStart), e: Number(el.dataset.lineEnd),
  }));
}

// Mark blocks whose own lines (not those of nested blocks) include an added line.
export function markChanged(root: HTMLElement, added: number[]) {
  const set = new Set(added);
  for (const b of blocks(root)) {
    const kids = blocks(b.el);
    let hit = false;
    for (let l = b.s; l <= b.e && !hit; l++) hit = set.has(l) && !kids.some((k) => k.s <= l && l <= k.e);
    b.el.classList.toggle("changed", hit);
  }
}

const isRelative = (s: string) => !/^([a-z][a-z0-9+.-]*:|\/\/|#)/i.test(s);

function resolve(docPath: string, rel: string) {
  const u = new URL(rel, "https://x/" + docPath);
  return { path: decode(u.pathname.slice(1)), hash: u.hash };
}

// Point relative images at the blob proxy. Relative links to a doc in the PR get data-doc/data-anchor
// so the app opens them in place; other relative links go to the file on GitHub.
export function rewriteLinks(root: HTMLElement, docPath: string, repoBlobUrl: string, prDocs: Set<string>, blobURL: (path: string) => string) {
  for (const img of root.querySelectorAll<HTMLImageElement>("img[src]")) {
    const src = img.getAttribute("src")!;
    if (isRelative(src)) img.src = blobURL(resolve(docPath, src).path);
  }
  for (const a of root.querySelectorAll<HTMLAnchorElement>("a[href]")) {
    const href = a.getAttribute("href")!;
    if (href.startsWith("#")) continue;
    if (isRelative(href)) {
      const r = resolve(docPath, href);
      if (prDocs.has(r.path)) {
        a.href = "#" + encodeURIComponent(r.path);
        a.dataset.doc = r.path;
        a.dataset.anchor = decode(r.hash.slice(1));
        continue;
      }
      a.href = `${repoBlobUrl}/${r.path}${r.hash}`;
    }
    a.target = "_blank";
    a.rel = "noopener noreferrer";
  }
}

let mermaidSeq = 0;

export async function renderMermaid(root: HTMLElement) {
  const els = root.querySelectorAll<HTMLElement>("[data-mermaid]");
  if (!els.length) return;
  const { default: mermaid } = await import("mermaid");
  const dark = matchMedia("(prefers-color-scheme: dark)").matches;
  mermaid.initialize({ startOnLoad: false, securityLevel: "strict", theme: dark ? "dark" : "default" });
  for (const el of els) {
    const out = el.querySelector<HTMLElement>(".mermaid-svg")!;
    const id = `mmd-${mermaidSeq++}`;
    try {
      out.innerHTML = (await mermaid.render(id, el.querySelector("code")!.textContent!)).svg;
    } catch (e) {
      // One bad diagram must not break the page: show the error and open the source.
      document.getElementById("d" + id)?.remove();
      out.className = "mermaid-error";
      out.textContent = "Mermaid error: " + ((e as Error)?.message ?? String(e));
      el.querySelector("details")?.setAttribute("open", "");
    }
  }
}
