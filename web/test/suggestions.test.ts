import { describe, expect, it } from "vitest";
import type { DocFile, Suggestion, Suggestions, Thread } from "../src/design/api";
import { defaultFilter, draftOf, fromAI, inDoc, isOutdated } from "../src/design/suggestions";

const s = (x: Partial<Suggestion>): Suggestion => ({ id: "r-c0", run: "r", head: "h1", kind: "comment", body: "b", status: "open", ...x });
const list = (...items: Suggestion[]): Suggestions => ({ runs: [], items });
const thread = (path: string, databaseId: string) => ({ path, comments: [{ databaseId }] } as Thread);

describe("defaultFilter", () => {
  it("opens on AI suggestions while any are open", () =>
    expect(defaultFilter(list(s({ status: "dismissed" }), s({ status: "open" })))).toBe("ai"));
  it("opens on Posted once all are decided", () =>
    expect(defaultFilter(list(s({ status: "posted" }), s({ status: "dismissed" })))).toBe("posted"));
});

describe("isOutdated", () => {
  it("is outdated on another head", () => expect(isOutdated(s({ head: "old" }), "new")).toBe(true));
  it("is current on the same head", () => expect(isOutdated(s({ head: "new" }), "new")).toBe(false));
});

describe("inDoc", () => {
  const docs = ["docs/a.md"];
  const threads = [thread("docs/a.md", "42"), thread("main.go", "7")];
  it("a comment on a doc shows on it", () => expect(inDoc(s({ path: "docs/a.md", line: 3 }), docs, threads)).toBe("docs/a.md"));
  it("a comment on a non-doc file is outside", () => expect(inDoc(s({ path: "main.go", line: 3 }), docs, threads)).toBeNull());
  it("a comment on removed lines is outside", () =>
    expect(inDoc(s({ path: "docs/a.md", line: 3, side: "LEFT" }), docs, threads)).toBeNull());
  it("a reply shows on its thread's doc", () => expect(inDoc(s({ kind: "reply", comment_id: 42 }), docs, threads)).toBe("docs/a.md"));
  it("a reply to a non-doc thread is outside", () => expect(inDoc(s({ kind: "reply", comment_id: 7 }), docs, threads)).toBeNull());
  it("a reply whose thread is gone is outside", () => expect(inDoc(s({ kind: "reply", comment_id: 9 }), docs, threads)).toBeNull());
});

describe("fromAI", () => {
  it("collects the comments of accepted suggestions", () =>
    expect(fromAI([s({ status: "review", github_id: "A" }), s({ status: "posted", github_id: "B" }),
      s({ status: "open" }), s({ status: "dismissed", github_id: "C" }), s({ status: "posting", github_id: "D" })]))
      .toEqual(new Set(["A", "B"])));
});

describe("draftOf", () => {
  const docs = ["docs/a.md"];
  const threads = [thread("docs/a.md", "42")];
  const file = { path: "docs/a.md", headSha: "abc1234", hunks: [{ start: 1, end: 10 }] } as DocFile;
  const doc = document.createElement("div");
  doc.innerHTML = `<p data-line-start="2" data-line-end="3">Two lines</p>`;

  it("places an in-doc comment as the composer would", () =>
    expect(draftOf(s({ path: "docs/a.md", line: 3, start_line: 2 }), "Why?", docs, threads, file, doc))
      .toEqual({ subjectType: "LINE", line: 3, startLine: 2, body: "Why?" }));
  it("posts an in-doc comment outside the diff on the file, quoting it", () =>
    expect(draftOf(s({ path: "docs/a.md", line: 3, start_line: 2 }), "Why?", docs, threads, { ...file, hunks: [] }, doc))
      .toEqual({ subjectType: "FILE", body: "> Two lines\n\nWhy?\n\n<!-- design-review:L2-L3@abc1234 -->" }));
  it("sends a LEFT comment as the AI wrote it, leaving the side to the server", () =>
    expect(draftOf(s({ path: "docs/a.md", line: 3, start_line: 2, side: "LEFT" }), "Why?", docs, threads, file, doc))
      .toEqual({ subjectType: "LINE", line: 3, startLine: 2, body: "Why?" }));
  it("sends a reply as its body and mode only", () =>
    expect({ ...draftOf(s({ kind: "reply", comment_id: 42 }), "Agreed", docs, threads, file, doc), mode: "now" })
      .toEqual({ body: "Agreed", mode: "now" }));
});
