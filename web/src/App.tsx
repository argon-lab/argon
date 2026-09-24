import { FunnelChoice, LocalLink, track } from "./funnel";
import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, DEMO_SESSION_LOST } from "./api";
import {
  Link,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom";
import { useDemoSession, useMeta } from "./hooks";
import { Badge, Button, Countdown, Tile, ErrorNote, Loading } from "./ui";
import Tour, { tourWasSeen } from "./Tour";
import Projects from "./pages/Projects";
import Project from "./pages/Project";
import Branch from "./pages/Branch";
import MergePlans from "./pages/MergePlans";
import MergePlan from "./pages/MergePlan";

export default function App() {
  const meta = useMeta();
  const session = useDemoSession();
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const qc = useQueryClient();
  const [sessionLost, setSessionLost] = useState(false);
  const [sessionNotice, setSessionNotice] = useState("");
  const recover = useMutation({
    mutationFn: api.demoSession,
    onSuccess: async (data) => {
      const projectData = (query: { queryKey: readonly unknown[] }) =>
        !["meta", "demo-session"].includes(String(query.queryKey[0]));
      await qc.cancelQueries({ predicate: projectData });
      qc.removeQueries({ predicate: projectData });
      qc.setQueryData(["demo-session"], data);
      setSessionLost(false);
      setSessionNotice(
        "A new sample session is ready. Your previous sample data is no longer available.",
      );
      navigate(`/p/${data.project}`, { replace: true });
    },
  });

  useEffect(() => {
    if (!meta.data?.demo) return;
    const lost = () => setSessionLost(true);
    window.addEventListener(DEMO_SESSION_LOST, lost);
    return () => window.removeEventListener(DEMO_SESSION_LOST, lost);
  }, [meta.data?.demo]);

  useEffect(() => {
    if (!meta.data?.demo || !session.data) return;
    const delay = new Date(session.data.expires_at).getTime() - Date.now();
    const timer = window.setTimeout(
      () => setSessionLost(true),
      Math.max(0, delay),
    );
    return () => window.clearTimeout(timer);
  }, [meta.data?.demo, session.data]);

  // Tour: keyed so "Guided tour" restarts it from step 0 by remounting.
  const [tourKey, setTourKey] = useState<number | null>(null);
  const demo =
    meta.data?.demo && session.data && !sessionLost && !recover.isPending;

  useEffect(() => {
    if (demo) track("demo_entered");
  }, [demo]);

  // Deep links from a previous instance/session must not strand visitors on
  // an unavailable project after a fresh session has been provisioned.
  useEffect(() => {
    if (!meta.data?.demo || !session.data || sessionLost) return;
    const linkedProject = pathname.match(/^\/p\/([^/]+)/)?.[1];
    if (
      pathname === "/" ||
      (linkedProject && linkedProject !== session.data.project)
    ) {
      if (linkedProject)
        setSessionNotice(
          "That sample session has ended. You are now in a fresh sample project.",
        );
      navigate(`/p/${session.data.project}`, { replace: true });
    }
  }, [meta.data?.demo, session.data, sessionLost, pathname, navigate]);

  // Offer the tour automatically the first time a demo visitor arrives.
  useEffect(() => {
    if (demo && tourKey === null && !tourWasSeen()) setTourKey(Date.now());
  }, [demo, tourKey]);

  return (
    <div className="mx-auto max-w-5xl px-4 pb-16">
      <header className="flex flex-wrap items-center gap-3 border-b border-brand-edge py-4">
        <Link to="/" className="flex items-center gap-3">
          <Tile />
          <span className="font-mono text-sm tracking-wide">Argon Console</span>
        </Link>
        <div className="ml-auto flex items-center gap-2">
          {demo && (
            <button
              className="border border-brand-edge px-2 py-1 font-mono text-[11px] text-brand-text-darker hover:border-brand-primary hover:text-brand-primary"
              onClick={() => setTourKey(Date.now())}
            >
              guided tour
            </button>
          )}
          {meta.data?.demo && <Badge tone="primary">demo</Badge>}
          {meta.data?.read_only && <Badge tone="primary">read-only</Badge>}
          {meta.data && <Badge>{meta.data.version}</Badge>}
          {meta.isError && <Badge>engine unreachable</Badge>}
        </div>
      </header>

      {demo && session.data && (
        <div className="mt-4 flex flex-wrap items-center gap-x-3 gap-y-1 border border-brand-primary/40 bg-brand-primary/5 px-3 py-2">
          <span className="font-mono text-xs text-brand-text-darker">
            Your session-scoped sample project expires{" "}
            <Countdown to={session.data.expires_at} />. Native database access
            is available locally.
          </span>
          <span className="ml-auto font-mono text-xs text-brand-muted">
            <LocalLink />
          </span>
        </div>
      )}

      {sessionNotice && !sessionLost && (
        <p role="status" className="mt-4 text-sm text-brand-text-darker">
          {sessionNotice}
        </p>
      )}
      {meta.data?.demo && (sessionLost || session.isError) && (
        <section
          role="alert"
          className="mt-6 border border-brand-primary/40 p-4"
        >
          <h1 className="font-mono text-sm">
            {sessionLost
              ? "Your sample session has ended"
              : "Your sample session could not start"}
          </h1>
          <p className="my-3 text-sm text-brand-text-darker">
            Sample data expires and may reset when the demo restarts. Start a
            new session to continue.
          </p>
          <Button
            tone="solid"
            disabled={recover.isPending}
            onClick={() => recover.mutate()}
          >
            {recover.isPending
              ? "Starting a new session…"
              : "Start a new sample session"}
          </Button>
          {recover.isError && <ErrorNote error={recover.error} />}
        </section>
      )}
      {meta.isPending && <Loading what="engine capabilities" />}
      {meta.isError && <ErrorNote error={meta.error} />}
      {meta.data?.demo && session.isPending && <Loading what="demo session" />}
      {meta.data?.demo && session.isError && (
        <ErrorNote error={session.error} />
      )}
      {meta.data && (!meta.data.demo || (demo && !session.isError)) && (
        <main className="pt-6">
          <Routes>
            <Route path="/" element={<Projects />} />
            <Route path="/p/:project" element={<Project />} />
            <Route
              path="/p/:project/b/:branch"
              element={<Branch key={pathname} />}
            />
            <Route path="/p/:project/merges" element={<MergePlans />} />
            <Route path="/p/:project/merges/:id" element={<MergePlan />} />
            <Route
              path="*"
              element={
                <p className="font-mono text-xs text-brand-muted">
                  nothing here.
                </p>
              }
            />
          </Routes>
        </main>
      )}

      {meta.data?.demo && <FunnelChoice />}

      {demo && tourKey !== null && (
        <Tour
          key={tourKey}
          project={session.data!.project}
          onClose={() => setTourKey(null)}
        />
      )}
    </div>
  );
}
