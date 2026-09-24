import { LocalLink } from "./funnel";
import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "./api";
import { Button } from "./ui";

const DONE_KEY = "argon.tour.done";

export function tourWasSeen(): boolean {
  try {
    return localStorage.getItem(DONE_KEY) === "1";
  } catch {
    return false;
  }
}

type Step = {
  title: string;
  body: string;
  // Where this step lives: "project" stays on the project page, "branch"
  // needs the agent-run branch (created on the run step).
  page: "project" | "branch";
  anchor?: string; // data-tour target to scroll to + ring
};

const STEPS: Step[] = [
  {
    title: "Welcome — this is your own sandbox",
    body: "This is a session-scoped, temporary sample project on a hosted Argon engine. Native MongoDB connections are available in your local deployment. Argon gives MongoDB Git-like branches, time travel, merges, and per-actor undo. Let's walk the whole loop in about a minute.",
    page: "project",
  },
  {
    title: "1 · Run an agent session",
    body: "Next forks planner and executor from the same pinned $49 order. The script accepts planner’s $44 proposal into main. Executor proposes $1 on its separate branch. Review its real conflict, then undo or discard it.",
    page: "project",
    anchor: "scenario",
  },
  {
    title: "2 · Every write has an actor",
    body: "Executor’s branch carries its actor in history. Planner uses a separate branch from the same pin. For native drivers, attribution belongs to the branch or run, not to individual clients sharing a connection.",
    page: "branch",
    anchor: "agents",
  },
  {
    title: "3 · Time-travel the data",
    body: "Pick a collection (try orders), then hit ▶ play — or drag the slider. You're watching the collection exactly as it stood at each point in history, reconstructed from the log. Only retained history with complete capture images can be reconstructed safely.",
    page: "branch",
    anchor: "timetravel",
  },
  {
    title: "4 · Diff and merge — a data pull request",
    body: "Click 'compute diff against parent', then 'create merge plan'. The plan is a reviewable data PR. Planner’s accepted $44 and executor’s proposed $1 conflict on the same order. Inspect all three versions before selecting a resolution strategy.",
    page: "branch",
    anchor: "diff",
  },
  {
    title: "5 · Undo one agent",
    body: "Back in the Agents panel, 'undo this actor →' reverts a single agent's writes with append-only compensations — nothing is destroyed, the undo is itself history. This is the undo button for AI agents.",
    page: "branch",
    anchor: "agents",
  },
  {
    title: "That's the loop",
    body: "Continue with the same order example on your local engine. The quickstart includes MongoDB replica-set setup, capture supervision and the runnable Python assertions.",
    page: "project",
  },
];

// Ring the target panel; retry briefly since it may mount after navigation.
function highlight(anchor: string | undefined): () => void {
  let el: HTMLElement | null = null;
  let tries = 0;
  const clear = () => el?.classList.remove("tour-ring");
  const tick = () => {
    if (!anchor) return;
    const found = document.querySelector<HTMLElement>(
      `[data-tour="${anchor}"]`,
    );
    if (found) {
      el = found;
      el.classList.add("tour-ring");
      el.scrollIntoView({ behavior: "smooth", block: "center" });
    } else if (tries++ < 12) {
      setTimeout(tick, 100);
    }
  };
  tick();
  return clear;
}

export default function Tour({
  project,
  onClose,
}: {
  project: string;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const [i, setI] = useState(0);
  const [branch, setBranch] = useState<string | null>(null);
  const step = STEPS[i];

  const run = useMemo(
    () => ({
      scenario: async () => {
        // Reuse an existing run branch if the visitor already made one.
        const { branches } = await api.branches(project);
        const existing = (branches ?? []).find((b) =>
          b.name.startsWith("agent-run-"),
        );
        if (existing) return existing.name;
        const res = await api.demoScenario();
        return res.branch;
      },
    }),
    [project],
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Opening (or restarting) the tour brings the top of the page into view,
  // so the welcome step and the card are unmistakably together.
  useEffect(() => {
    window.scrollTo({ top: 0, behavior: "smooth" });
  }, []);

  // On each step, land on the right page and ring its anchor.
  useEffect(() => {
    const target =
      step.page === "branch" && branch
        ? `/p/${project}/b/${branch}`
        : `/p/${project}`;
    if (window.location.pathname !== target) navigate(target);
    const clear = highlight(step.anchor);
    return clear;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [i, branch]);

  const finish = () => {
    try {
      localStorage.setItem(DONE_KEY, "1");
    } catch {
      /* Storage is optional. */
    }
    onClose();
  };

  const next = async () => {
    // Leaving the "run" step: make sure a branch exists first.
    if (step.anchor === "scenario" && !branch) {
      setBusy(true);
      setError(null);
      try {
        setBranch(await run.scenario());
      } catch (e) {
        setError(
          e instanceof Error
            ? e.message
            : "Could not run the scenario. Try again.",
        );
        return;
      } finally {
        setBusy(false);
      }
    }
    if (i === STEPS.length - 1) finish();
    else setI((n) => n + 1);
  };

  // Steps without a panel to point at (welcome, finish) show as a centered
  // dialog behind a dim backdrop — unmissable, and clearly the thing the
  // "guided tour" button opened. Steps that reference a panel dock to the
  // corner and ring their target, so the page stays usable.
  const centered = !step.anchor;

  const card = (
    <div
      className={
        centered
          ? "tour-card relative z-50 w-[440px] max-w-[calc(100vw-2rem)] border border-brand-primary/60 bg-brand-surface shadow-2xl"
          : "tour-card fixed bottom-4 right-4 z-50 w-[360px] max-w-[calc(100vw-2rem)] border border-brand-primary/60 bg-brand-surface shadow-2xl"
      }
    >
      <div className="flex items-center justify-between border-b border-brand-edge px-4 py-2">
        <span className="font-mono text-[11px] uppercase tracking-widest text-brand-primary">
          Guided tour · {i + 1}/{STEPS.length}
        </span>
        <button
          className="font-mono text-[11px] text-brand-muted hover:text-brand-text"
          onClick={finish}
        >
          skip
        </button>
      </div>
      <div className="p-4">
        <h3 className="font-mono text-sm text-brand-text">{step.title}</h3>
        <p className="mt-2 text-sm leading-6 text-brand-text-darker">
          {step.body}
        </p>
        {error && (
          <p role="alert" className="mt-2 text-sm text-red-400">
            {error}
          </p>
        )}
        {i === STEPS.length - 1 && (
          <p className="mt-4 text-sm">
            <LocalLink />
          </p>
        )}
        <div className="mt-4 flex items-center gap-2">
          {i > 0 && (
            <Button onClick={() => setI((n) => Math.max(0, n - 1))}>
              back
            </Button>
          )}
          <div className="ml-auto">
            <Button tone="solid" disabled={busy} onClick={next}>
              {busy
                ? "running…"
                : i === STEPS.length - 1
                  ? "finish"
                  : step.anchor === "scenario"
                    ? "run it →"
                    : "next"}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );

  if (centered) {
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 px-4">
        {card}
      </div>
    );
  }
  return card;
}
