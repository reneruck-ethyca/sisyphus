import { render } from "preact";
import { act } from "preact/test-utils";
import { expect, it, vi } from "vitest";
import { useFetch } from "../src/live";

// One pending answer per URL, resolved by hand.
const pending: Record<string, (body: string) => void> = {};
vi.stubGlobal("fetch", vi.fn((url: string) => new Promise((ok) => (pending[url] = (body) => ok(new Response(body))))));

let seen: { data?: string; loading: boolean } = { loading: true };
function Probe({ url }: { url: string }) {
  seen = useFetch<string>(url);
  return null;
}
const flush = () => act(async () => { await new Promise((r) => setTimeout(r, 0)); });

it("shows nothing of the old URL's answer while a new URL loads", async () => {
  const el = document.createElement("div");
  act(() => render(<Probe url="/api/a" />, el));
  await flush();
  pending["/api/a"]('"A"');
  await flush();
  expect(seen).toMatchObject({ data: "A", loading: false });

  act(() => render(<Probe url="/api/b" />, el));
  await flush();
  expect(seen.data).toBeUndefined();
  expect(seen.loading).toBe(true);
  pending["/api/b"]('"B"');
  await flush();
  expect(seen).toMatchObject({ data: "B", loading: false });
});
