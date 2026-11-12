// Live state shared by the pages: the router, the status stream, timers and fetches.
import { useEffect, useState } from "preact/hooks";
import { get, session, type Status } from "./api";

export type { Status };

// --- router: same-origin links change the path in place; the window's external links go through sisyphus.

const routeListeners = new Set<() => void>();
export function navigate(to: string, replace = false) {
  history[replace ? "replaceState" : "pushState"](null, "", to);
  routeListeners.forEach((f) => f());
  scrollTo(0, 0);
}
addEventListener("popstate", () => routeListeners.forEach((f) => f()));

// The app window (its user agent says so) can't open tabs or leave the dashboard: its external
// links open in the default browser via sisyphus.
const inWindow = navigator.userAgent.includes("sisyphus-window");

document.addEventListener("click", (e) => {
  const a = (e.target as Element | null)?.closest?.("a[href]") as HTMLAnchorElement | null;
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  if (a.origin !== location.origin) {
    if (!inWindow) return;
    e.preventDefault();
    fetch("/api/open", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "url=" + encodeURIComponent(a.href) });
    return;
  }
  if (a.target || a.hasAttribute("download") || a.pathname.startsWith("/api/")) return;
  e.preventDefault();
  if (a.pathname + a.search !== location.pathname + location.search) navigate(a.pathname + a.search + a.hash);
});

export function useLocation(): URL {
  const [, set] = useState(0);
  useEffect(() => {
    const f = () => set((n) => n + 1);
    routeListeners.add(f);
    return () => void routeListeners.delete(f);
  }, []);
  return new URL(location.href);
}

// --- status: one stream for the whole app, pushed on every change.

let status: Status | undefined;
let statusError = "";
const statusListeners = new Set<() => void>();
const streams: EventSource[] = [];

// stream opens an EventSource that's closed when the page is left (see pagehide below).
export function stream(url: string): EventSource {
  const s = new EventSource(url);
  streams.push(s);
  return s;
}
export function unstream(s: EventSource) {
  s.close();
  streams.splice(streams.indexOf(s) >>> 0, 1);
}
// Chrome keeps left pages in its back/forward cache with their streams open; a few of them use up
// the browser's connections to this host. Close them on leaving, and reload a page restored from the cache.
addEventListener("pagehide", () => streams.forEach((s) => s.close()));
addEventListener("pageshow", (e) => e.persisted && location.reload());

const signedOut = "Not signed in: open the dashboard from the sisyphus menu (Open window or Open dashboard).";

let following = false;
async function followStatus() {
  if (following) return;
  following = true;
  await session;
  const s = stream("/api/status/stream");
  s.onmessage = (m) => {
    status = JSON.parse(m.data);
    statusError = "";
    statusListeners.forEach((f) => f());
  };
  s.onerror = async () => {
    // A 401 closes the stream for good; anything else reconnects on its own.
    const unauthorized = s.readyState === EventSource.CLOSED && (await fetch("/api/status").catch(() => null))?.status === 401;
    statusError = unauthorized ? signedOut : "Lost the connection to sisyphus; reconnecting…";
    statusListeners.forEach((f) => f());
  };
}

export function useStatus(): { status?: Status; error: string } {
  const [, set] = useState(0);
  useEffect(() => {
    followStatus();
    const f = () => set((n) => n + 1);
    statusListeners.add(f);
    return () => void statusListeners.delete(f);
  }, []);
  return { status, error: statusError };
}

// --- time: <Ago> re-renders every 30 s so ages don't go stale.

export function useTick(ms = 30000): number {
  const [now, set] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => set(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

// --- fetches: useFetch loads url (none while it's ""), again whenever key changes.

export type Loaded<T> = { data?: T; error?: string; loading: boolean };

export function useFetch<T>(url: string, key: unknown = ""): Loaded<T> {
  const [state, set] = useState<Loaded<T>>({ loading: true });
  useEffect(() => {
    if (!url) return set({ loading: false });
    let live = true;
    set((s) => ({ ...s, loading: true }));
    get<T>(url).then(
      (data) => live && set({ data, loading: false }),
      (e: Error) => live && set({ error: e.message, loading: false }),
    );
    return () => void (live = false);
  }, [url, String(key)]);
  return state;
}

// sessionState is per-tab memory that survives a reload: a search, a draft.
export function loadSession<T>(key: string, fallback: T): T {
  try {
    const v = sessionStorage.getItem(key);
    return v === null ? fallback : JSON.parse(v);
  } catch {
    return fallback;
  }
}
export function saveSession(key: string, v: unknown) {
  try {
    if (v === undefined) sessionStorage.removeItem(key);
    else sessionStorage.setItem(key, JSON.stringify(v));
  } catch {
    /* private mode or blocked storage: it's a convenience */
  }
}
