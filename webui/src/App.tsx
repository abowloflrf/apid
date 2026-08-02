import { useEffect, useState } from "react";
import { AuthError, fetchJSON, onAuthFailure } from "./api";
import { LoginView } from "./components/Login";
import { StatsTopbar } from "./components/StatsTopbar";
import { ToastHost } from "./components/Toast";
import { useStatsQuery } from "./useStatsQuery";
import { LiveView } from "./views/LiveView";
import { StatsView } from "./views/StatsView";
import { TopologyView } from "./views/TopologyView";
import type { Options } from "./types";

const VIEWS = ["stats", "live", "routes"];
const viewFromHash = (): string => (VIEWS.includes(location.hash.slice(1)) ? location.hash.slice(1) : "stats");

export default function App() {
  const [view, setView] = useState(viewFromHash());
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [liveCount, setLiveCount] = useState(0);
  const [topoRefreshKey, setTopoRefreshKey] = useState(0);
  const q = useStatsQuery();

  // View-specific body classes (grid layout etc.), mirroring the original app.
  useEffect(() => {
    document.body.classList.toggle("view-routes", view === "routes");
    document.body.classList.toggle("view-live", view === "live");
  }, [view]);

  // Hash-based view routing: #stats / #live / #routes.
  useEffect(() => {
    const onHash = () => setView(viewFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Probe whether the stats key is needed; the shared fetch layer calls
  // onAuthFailure on any 401 so a stale/removed key drops back to the login.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await fetchJSON<Options>("options");
        if (!cancelled) setAuthed(true);
      } catch (e) {
        // Non-auth errors (e.g. storage disabled) still show the dashboard.
        if (!cancelled) setAuthed(!(e instanceof AuthError));
      }
    })();
    onAuthFailure(() => setAuthed(false));
    return () => {
      cancelled = true;
    };
  }, []);

  // Entrance animation: the HTML body starts with .loading/.intro classes.
  useEffect(() => {
    document.body.classList.remove("loading");
    const t = window.setTimeout(() => document.body.classList.remove("intro"), 1200);
    return () => window.clearTimeout(t);
  }, []);

  // Initial load: options first (populates filters), then the full dashboard.
  useEffect(() => {
    void q.loadOptions();
    void q.loadAll();
    // Runs once on mount; loadAll reads the latest query state internally.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Optional 30s auto-refresh, only while the stats view is active.
  useEffect(() => {
    if (!q.auto || view !== "stats") return;
    const t = window.setInterval(() => void q.loadAll(), 30000);
    return () => window.clearInterval(t);
  }, [q.auto, view, q.loadAll]);

  const goView = (v: string) => {
    setView(v);
    history.replaceState(null, "", v === "stats" ? "#" : "#" + v);
  };

  if (authed === null) {
    return <div className="boot" />;
  }
  if (authed === false) {
    return <LoginView onAuthed={() => setAuthed(true)} />;
  }

  return (
    <>
      <StatsTopbar
        view={view}
        onView={goView}
        q={q}
        liveCount={liveCount}
        onRefresh={() => (view === "routes" ? setTopoRefreshKey((k) => k + 1) : void q.loadAll())}
      />
      <main>
        {view === "stats" && <StatsView q={q} />}
        {view === "routes" && <TopologyView refreshKey={topoRefreshKey} />}
        {view === "live" && <LiveView onLiveCount={setLiveCount} />}
      </main>
      <ToastHost />
    </>
  );
}
