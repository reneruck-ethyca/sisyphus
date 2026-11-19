import { render } from "preact";
import { expect, it, vi } from "vitest";
import type { HandoffView } from "../src/api";
import { Actions } from "../src/pages/lastmile";

const fetchMock = vi.fn(async () => new Response("{}"));
vi.stubGlobal("fetch", fetchMock);

const view = (over: Partial<HandoffView>): HandoffView => ({
  id: "h1", repo: "o/r", branch: "feature/x", worktree: "/wt", base: "main", title: "T", state: "approved",
  autonomy: { fix_ci: true, address_reviews: true, merge: "auto" }, max_fix_rounds: 3, created: "", updated: "2026-11-12T10:00:00Z",
  pr_number: 7, over: false, reviewing: [], depth: 0, can_merge: true,
  snapshot: { auto_merge_allowed: true } as HandoffView["snapshot"], ...over,
});

it("shows GitHub auto-merge on for merge=auto too", () => {
  const el = document.createElement("div");
  render(<Actions v={view({})} />, el);
  expect(el.querySelector<HTMLInputElement>('input[type="checkbox"]')!.checked).toBe(true);
});

it("asks before merging, and says a pressed merge is pending", () => {
  const el = document.createElement("div");
  const ask = vi.fn(() => false);
  vi.stubGlobal("confirm", ask);
  render(<Actions v={view({})} />, el);
  [...el.querySelectorAll("button")].find((b) => b.textContent === "Squash and merge")!.click();
  expect(ask).toHaveBeenCalledWith("Squash and merge o/r#7?");
  expect(fetchMock).not.toHaveBeenCalled();

  render(<Actions v={view({ merge_requested: true, can_merge: false })} />, el);
  expect(el.textContent).toContain("merge pending");

  render(<Actions v={view({ merge_requested: true })} />, el);
  expect(el.textContent).toContain("merge pending");
  expect(el.textContent).not.toContain("Squash and merge");
});

it("offers a rebase only when the branch is behind, and says one is pending", () => {
  const el = document.createElement("div");
  const rebase = () => [...el.querySelectorAll("button")].find((b) => b.textContent?.startsWith("Rebase"));
  render(<Actions v={view({})} />, el);
  expect(rebase()).toBeUndefined();
  render(<Actions v={view({ snapshot: { behind: 4 } as HandoffView["snapshot"] })} />, el);
  expect(rebase()!.textContent).toBe("Rebase (4 behind)");
  render(<Actions v={view({ state: "needs-human", snapshot: { behind: 4 } as HandoffView["snapshot"] })} />, el);
  expect(rebase()).toBeUndefined();
  render(<Actions v={view({ rebase_asked: "fix agent", snapshot: { behind: 4 } as HandoffView["snapshot"] })} />, el);
  expect(rebase()).toBeUndefined();
  expect(el.textContent).toContain("rebase pending (fix agent)");
});
