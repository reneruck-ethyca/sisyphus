import { render } from "preact";
import { expect, it, vi } from "vitest";
import type { HandoffView, Status } from "../src/api";
import { Handoff } from "../src/pages/handoff";

vi.stubGlobal("fetch", vi.fn(async () => new Response("{}")));

const view = (hash: string, file: string): HandoffView => ({
  id: "h1", repo: "o/r", branch: "feature/x", worktree: "/wt", base: "main", title: "T", state: "needs-human", reason: "uncommitted changes",
  autonomy: { fix_ci: true, address_reviews: true, merge: "off" }, max_fix_rounds: 3, created: "", updated: "2026-11-12T10:00:00Z",
  pr_number: 7, over: false, reviewing: [], depth: 0, dirty_hash: hash,
  dirty: { status: hash, message: "Suggested", files: [{ path: file, code: "M", add: 1, del: 0 }] },
});
const status = (v: HandoffView) => ({ handoffs: [v], needs_human: [v] }) as unknown as Status;

// A new worktree status is a new panel: a draft for the old files must not be committed against the new ones.
it("starts the dirty panel over when the worktree changes", () => {
  const el = document.createElement("div");
  render(<Handoff status={status(view("A", "a.go"))} id="h1" />, el);
  const msg = el.querySelector<HTMLTextAreaElement>("#dirty-msg")!;
  msg.value = "my draft for A";
  msg.dispatchEvent(new Event("input", { bubbles: true }));
  el.querySelector<HTMLInputElement>('input[aria-label="Commit a.go"]')!.click();

  render(<Handoff status={status(view("B", "b.go"))} id="h1" />, el);
  expect(el.querySelector<HTMLTextAreaElement>("#dirty-msg")!.value).toBe("Suggested");
  expect(el.querySelector<HTMLInputElement>('input[aria-label="Commit b.go"]')!.checked).toBe(true);
});
