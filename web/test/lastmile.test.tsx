import { render } from "preact";
import { expect, it } from "vitest";
import type { HandoffView, Status } from "../src/api";
import { LastMile } from "../src/pages/lastmile";

const h = (id: string, pr: number, o: Partial<HandoffView> = {}): HandoffView => ({
  id, repo: "o/fidesplus", branch: id, worktree: "/wt", base: "dev", title: `T${pr}`, state: "pr-open",
  autonomy: { fix_ci: true, address_reviews: true, merge: "off" }, max_fix_rounds: 3, created: "", updated: "2026-11-26T10:00:00Z",
  pr_number: pr, over: false, reviewing: [], depth: 0, ...o,
});
const stack = (position: number) => ({ snapshot: { stack: { number: 9, base: "dev", bottom: 70, position, size: 3 } } as HandoffView["snapshot"] });

// Views order: each root, then its children bottom-up. The rail draws them top first with the base at the foot.
it("draws a native stack and a chain on rails, top first, and a lone handoff plainly", () => {
  const hs = [
    h("a", 70, stack(1)), h("b", 71, { ...stack(2), depth: 1, waiting_on: "a" }), h("c", 72, { ...stack(3), depth: 2, waiting_on: "b" }),
    h("d", 30, { parent_pr: 15, base: "pbac" }), h("e", 31, { depth: 1, waiting_on: "d" }),
    h("f", 28),
  ];
  const el = document.createElement("div");
  render(<LastMile status={{ handoffs: hs, needs_human: [] } as unknown as Status} />, el);

  const [native, chain] = el.querySelectorAll(".rail");
  expect(native.classList.contains("chain")).toBe(false);
  expect([...native.querySelectorAll(".rr .title")].map((t) => t.textContent)).toEqual(["T72", "T71", "T70"]);
  expect(native.querySelector(".dot.next")).not.toBeNull();
  expect(native.querySelectorAll(".rr .meta")[2].textContent).toContain("1 of 3 · lands next");
  expect(native.querySelector(".foot")!.textContent).toBe("dev");

  expect(chain.classList.contains("chain")).toBe(true);
  expect([...chain.querySelectorAll("a.rr .title")].map((t) => t.textContent)).toEqual(["T31", "T30"]);
  expect(chain.querySelector(".dot.next")).toBeNull();
  expect(chain.querySelectorAll("a.rr .meta")[1].textContent).toContain("waits for #15");
  expect(chain.querySelector(".ghost")!.textContent).toContain("#15 · not a handoff");

  expect(el.querySelectorAll(".rail").length).toBe(2);
  expect([...el.querySelectorAll("aside > a.pr .title")].map((t) => t.textContent)).toEqual(["T28"]);
});
