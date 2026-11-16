import { describe, expect, it } from "vitest";
import type { Comment, Thread } from "../src/design/api";
import { unreadIn } from "../src/pages/design";

const c = (author: string, createdAt: string, state: Comment["state"] = "SUBMITTED"): Comment =>
  ({ id: author + createdAt, databaseId: "1", author, avatarUrl: "", bodyHTML: "", createdAt, url: "", state });
const thread = (...comments: Comment[]) => ({ comments } as Thread);

describe("unreadIn", () => {
  const since = "2026-11-10T09:00:00Z";
  it("counts others' comments after your review", () => {
    expect(unreadIn(thread(c("me", "2026-11-09T00:00:00Z"), c("alice", "2026-11-11T00:00:00Z"), c("bob", "2026-11-12T00:00:00Z")), since, "me")).toBe(2);
  });
  it("leaves out your own, pending and older comments", () => {
    expect(unreadIn(thread(c("me", "2026-11-11T00:00:00Z"), c("alice", "2026-11-11T00:00:00Z", "PENDING"), c("bob", "2026-11-01T00:00:00Z")), since, "me")).toBe(0);
  });
  it("marks nothing before you've reviewed", () => {
    expect(unreadIn(thread(c("alice", "2026-11-11T00:00:00Z")), "", "me")).toBe(0);
  });
});
