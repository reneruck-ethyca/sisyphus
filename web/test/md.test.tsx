import { render } from "preact";
import { expect, it } from "vitest";
import { Md } from "../src/components";

it("renders code and bold, and nothing else, from review text", () => {
  const el = document.createElement("div");
  render(<Md text={"Use `x<y>` and **care** <img src=x onerror=alert(1)>"} />, el);
  expect(el.innerHTML).toBe("Use <code>x&lt;y&gt;</code> and <b>care</b> &lt;img src=x onerror=alert(1)&gt;");
});
