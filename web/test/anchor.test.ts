import { describe, expect, it } from "vitest";
import { blockForLine, buildComment, clamp, placeThread } from "../src/design/anchor";
import type { Thread } from "../src/design/api";

const hunks = [{ start: 5, end: 10 }, { start: 20, end: 30 }];

describe("clamp", () => {
  it("inside one hunk", () => expect(clamp({ start: 6, end: 8 }, hunks)).toEqual({ start: 6, end: 8 }));
  it("spanning two hunks keeps the largest overlap", () =>
    expect(clamp({ start: 9, end: 25 }, hunks)).toEqual({ start: 20, end: 25 }));
  it("outside all hunks", () => expect(clamp({ start: 12, end: 15 }, hunks)).toBeNull());
  it("single line", () => expect(clamp({ start: 5, end: 5 }, hunks)).toEqual({ start: 5, end: 5 }));
});

describe("buildComment", () => {
  it("line comment omits startLine for one line", () =>
    expect(buildComment({ start: 7, end: 7 }, "q", "hi", hunks, "abcdef123")).toEqual({ subjectType: "LINE", line: 7, startLine: undefined, body: "hi" }));
  it("quotes the selection when clamped", () =>
    expect(buildComment({ start: 8, end: 12 }, "sel", "hi", hunks, "abcdef123").body).toBe("> sel\n\nhi"));
  it("falls back to a file comment with marker", () =>
    expect(buildComment({ start: 12, end: 14 }, "sel", "hi", hunks, "abcdef123")).toEqual({
      subjectType: "FILE", body: "> sel\n\nhi\n\n<!-- design-review:L12-L14@abcdef1 -->",
    }));
});

const thread = (t: Partial<Thread>): Thread => ({
  id: "t", path: "a.md", subjectType: "LINE", startLine: null, line: null, originalStartLine: null, originalLine: null,
  isResolved: false, isOutdated: false, anchor: null, comments: [], ...t,
});

describe("placeThread", () => {
  it("LINE thread", () => expect(placeThread(thread({ line: 4, startLine: 2 }), "abc")).toEqual({ section: "doc", start: 2, line: 4, approximate: false }));
  it("FILE thread with marker, approximate on SHA mismatch", () =>
    expect(placeThread(thread({ subjectType: "FILE", anchor: { startLine: 3, line: 5, sha: "1234567" } }), "abcdef0")).toEqual({
      section: "doc", start: 3, line: 5, approximate: true,
    }));
  it("FILE thread without marker", () => expect(placeThread(thread({ subjectType: "FILE" }), "abc")).toEqual({ section: "general" }));
  it("outdated", () => expect(placeThread(thread({ isOutdated: true, originalLine: 3 }), "abc")).toEqual({ section: "outdated" }));
});

describe("blockForLine", () => {
  const bs = [{ s: 3, e: 8 }, { s: 4, e: 4 }, { s: 5, e: 8 }, { s: 12, e: 12 }];
  it("innermost containing block", () => expect(blockForLine(bs, 4)).toBe(bs[1]));
  it("blank line goes to the next block below", () => expect(blockForLine(bs, 10)).toBe(bs[3]));
  it("front matter goes to the first block", () => expect(blockForLine(bs, 1)).toBe(bs[0]));
});
