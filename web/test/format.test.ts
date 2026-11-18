import { describe, expect, it } from "vitest";
import type { HandoffView, Run } from "../src/api";
import { dur, fuzzy, gh, handoffTone, outcome, reviewerChips, rel, runPath, secs, splitState } from "../src/format";

const run = (p: Partial<Run>): Run => ({ id: "r", job: { pr: {} as Run["job"]["pr"], agent: { vendor: "", model: "", effort: "" }, reason: "new" },
  image: "", started: "2026-11-12T10:00:00Z", finished: "2026-11-12T10:01:05Z", status: "succeeded", exit_code: 0, ...p });

describe("format", () => {
  it("says how long ago", () => {
    const now = Date.parse("2026-11-12T12:00:00Z");
    expect(rel("2026-11-12T11:59:40Z", now)).toBe("just now");
    expect(rel("2026-11-12T11:55:00Z", now)).toBe("5 min ago");
    expect(rel("2026-11-12T14:00:00Z", now)).toBe("in 2 h");
    expect(rel("2026-11-09T12:00:00Z", now)).toBe("3 d ago");
  });

  it("says what a run came to", () => {
    expect(outcome(run({ status: "running" }))).toEqual({ label: "Reviewing", tone: "live" });
    expect(outcome(run({ status: "error" })).label).toBe("Failed");
    expect(outcome(run({})).label).toBe("Not posted");
    expect(outcome(run({ posted: { event: "REQUEST_CHANGES" } }))).toEqual({ label: "Changes requested", tone: "changes" });
    expect(outcome(run({ posted: { event: "APPROVE", error: "403" } })).label).toBe("Posting failed");
    expect(outcome(run({ posted: {} })).label).toBe("Replied");
  });

  it("times runs and durations", () => {
    expect(dur(run({}))).toBe("1 min 5 s");
    expect(dur(run({ status: "running" }), Date.parse("2026-11-12T12:30:00Z"))).toBe("2 h 30 min");
    expect(secs(45)).toBe("45 s");
    expect(secs(5 * 3600)).toBe("5.0 h");
    expect(secs(3 * 86400)).toBe("3.0 d");
  });

  it("colours handoff states", () => {
    expect(handoffTone("merged")).toBe("merged");
    expect(handoffTone("queued")).toBe("live");
    expect(handoffTone("approved")).toBe("ok");
  });

  it("only links GitHub", () => {
    expect(gh("https://github.com/o/r/pull/1")).toBe("https://github.com/o/r/pull/1");
    expect(gh("javascript:alert(1)")).toBe("");
    expect(gh("https://evil.example/x")).toBe("");
    expect(gh(undefined)).toBe("");
  });

  it("tells who reviews", () => {
    const v = { reviewing: ["a", "c"], requested: ["a"], removed: ["b", "c"], snapshot: {} } as unknown as HandoffView;
    expect(reviewerChips(v)).toEqual([{ login: "a", cls: "" }, { login: "c", cls: "human" }, { login: "b", cls: "gone" }]);
  });

  it("matches titles fuzzily and splits state events", () => {
    expect(fuzzy("rbc dflt", "RBAC the default")).toBe(true);
    expect(fuzzy("xyz", "RBAC the default")).toBe(false);
    expect(fuzzy("", "anything")).toBe(true);
    expect(splitState("ci-red: 2 checks failed")).toEqual({ state: "ci-red", reason: "2 checks failed" });
    expect(splitState("merged")).toEqual({ state: "merged", reason: "" });
    expect(runPath("o/r#3", "x")).toBe("/prs/o/r/3/runs/x");
    expect(runPath("o/r#3", "x", true)).toBe("/history/o/r/3/runs/x");
  });
});
