import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { blocks, markChanged, renderMarkdown } from "../src/design/render";

const fixture = (name: string) => readFileSync(`test/fixtures/${name}`, "utf8") // vitest runs from web/;

function ranges(src: string) {
  const root = document.createElement("div");
  root.innerHTML = renderMarkdown(src);
  return blocks(root).map((b) => `${b.el.tagName.toLowerCase()}${b.el.className ? "." + b.el.className.split(" ")[0] : ""} ${b.s}-${b.e}`);
}

describe("sourceLines", () => {
  it("maps every block in all.md to its source lines", () => {
    expect(ranges(fixture("all.md"))).toMatchInlineSnapshot(`
      [
        "h1 6-6",
        "h2 8-9",
        "p 11-13",
        "li 15-15",
        "li 16-18",
        "li 17-17",
        "li 18-18",
        "li 19-19",
        "li 21-21",
        "p 21-21",
        "li 23-23",
        "p 23-23",
        "tr 25-25",
        "tr 27-27",
        "tr 28-28",
        "div 30-32",
        "div 34-34",
        "div.mermaid-block 36-39",
        "blockquote 41-42",
        "p 41-41",
        "li 42-42",
        "div.markdown-alert 44-45",
        "p 44-45",
        "div 47-47",
        "hr 49-49",
        "li.task-list-item 51-51",
        "li.task-list-item 52-52",
      ]
    `);
  });

  it("marks only blocks whose own lines changed", () => {
    const root = document.createElement("div");
    root.innerHTML = renderMarkdown("- a\n  - b\n- c\n");
    markChanged(root, [2]);
    expect([...root.querySelectorAll(".changed")].map((e) => e.textContent)).toEqual(["b"]);
  });
});

describe("sanitize", () => {
  it("strips CSS from the doc", () => {
    const html = renderMarkdown('<style>.topbar{display:none}</style>\n\n<p style="position:fixed">x</p>\n');
    expect(html).not.toMatch(/<style|style=/);
    expect(html).toContain(">x</p>");
  });
});

describe("sanitize", () => {
  it("drops forms and their controls, which would post to sisyphus as the user", () => {
    const html = renderMarkdown('<form method="post" action="/api/restart"><button formaction="/api/x">Show</button><input name="a"></form>\n\ntext');
    expect(html).not.toMatch(/<form|<button|<input|action=/);
    expect(html).toContain("text");
  });
});

describe("rewriteLinks", () => {
  it("survives a lone % in a doc's link", async () => {
    const { rewriteLinks } = await import("../src/design/render");
    const root = document.createElement("div");
    root.innerHTML = renderMarkdown("[a](bench/50%-load.md) [b](#100%) ![c](img/x%.png)");
    rewriteLinks(root, "docs/a.md", "https://github.com/o/r/blob/h", new Set(["docs/bench/50%-load.md"]), (p) => "/blob?path=" + encodeURIComponent(p));
    expect(root.querySelector("img")!.getAttribute("src")).toBe("/blob?path=" + encodeURIComponent("docs/img/x%.png"));
  });
});
