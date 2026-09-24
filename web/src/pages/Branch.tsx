import { LocalLink, NativeNotice, track } from "../funnel";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, Entry, UndoResult } from "../api";
import { useReadOnly, useNativeConnections, useDemo } from "../hooks";
import {
  actorColor,
  Badge,
  Button,
  ConfirmButton,
  Dot,
  ErrorNote,
  Json,
  Loading,
  Mono,
  Panel,
  Select,
  TextInput,
} from "../ui";

// A prefill request from the agent lens into the undo panel: which actor's
// writes to stage, and from which LSN. The nonce lets the same actor be
// re-requested (it changes even when actor/fromLSN repeat).
type UndoRequest = { fromLSN: number; actor: string; nonce: number };

// One ascending pass over the branch's history, shared (react-query dedupes
// the key) by the scrubber's frame stops and the agent lens's per-actor
// stats. 500 entries is plenty for a console view; the timeline paginates
// separately for the full log.
function useHistory(project: string, branch: string) {
  return useQuery({
    queryKey: ["history-asc", project, branch],
    queryFn: () => api.entries(project, branch, { order: "asc", limit: 500 }),
    refetchInterval: 5000,
  });
}

type ActorStat = {
  actor: string;
  count: number;
  minLSN: number;
  collections: string[];
};

function actorStats(entries: Entry[]): ActorStat[] {
  const by = new Map<string, ActorStat>();
  for (const e of entries) {
    if (!e.actor || !e.collection) continue;
    const s = by.get(e.actor) ?? {
      actor: e.actor,
      count: 0,
      minLSN: e.lsn,
      collections: [],
    };
    s.count += 1;
    s.minLSN = Math.min(s.minLSN, e.lsn);
    if (!s.collections.includes(e.collection)) s.collections.push(e.collection);
    by.set(e.actor, s);
  }
  return [...by.values()].sort((a, b) => a.minLSN - b.minLSN);
}

function Actions({ project, branch }: { project: string; branch: string }) {
  const native = useNativeConnections();
  const status = useQuery({
    queryKey: ["ingesters"],
    queryFn: api.ingesters,
    refetchInterval: 5000,
  });
  const readOnly = useReadOnly();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["branch", project, branch],
    queryFn: () => api.branch(project, branch),
    refetchInterval: 5000,
  });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["branch", project, branch] });
    qc.invalidateQueries({ queryKey: ["branches", project] });
    qc.invalidateQueries({ queryKey: ["ingesters"] });
  };
  const checkout = useMutation({
    mutationFn: () => api.checkout(project, branch),
    onSuccess: refresh,
  });
  const release = useMutation({
    mutationFn: () => api.release(project, branch),
    onSuccess: refresh,
  });
  const snapshot = useMutation({
    mutationFn: () => api.createSnapshot(project, branch),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteBranch(project, branch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["branches", project] });
      navigate(`/p/${project}`);
    },
  });

  const capture = status.data?.capture?.find(
    (c) => c.branch_id === q.data?.branch.id,
  );
  const err = checkout.error ?? release.error ?? snapshot.error ?? remove.error;
  return (
    <Panel title={`Branch · ${branch}`}>
      {q.isError && <ErrorNote error={q.error} />}
      {q.data && (
        <>
          <div className="flex flex-wrap items-center gap-3 text-sm">
            <span className="font-mono text-xs">
              head {q.data.branch.head_lsn} · base {q.data.branch.base_lsn}
            </span>
            {q.data.branch.state === "live" && (
              <span className="flex items-center gap-1 font-mono text-xs text-brand-text-darker">
                <Dot on /> live · {q.data.branch.physical_db}
              </span>
            )}
            {q.data.branch.expires_at && <Badge tone="primary">sandbox</Badge>}
          </div>

          {native && q.data.connection_string && (
            <div className="mt-3 flex items-center gap-2">
              <code className="overflow-x-auto border border-brand-edge bg-brand-dark px-2 py-1 font-mono text-xs text-brand-text-darker">
                {q.data.connection_string}
              </code>
              <Button
                onClick={() =>
                  navigator.clipboard.writeText(q.data.connection_string!)
                }
              >
                copy
              </Button>
            </div>
          )}

          {!readOnly && (
            <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
              {native &&
                (q.data.branch.state === "live" ? (
                  <Button
                    disabled={release.isPending}
                    onClick={() => release.mutate()}
                  >
                    release
                  </Button>
                ) : (
                  <Button
                    tone="solid"
                    disabled={checkout.isPending}
                    onClick={() => checkout.mutate()}
                  >
                    checkout
                  </Button>
                ))}
              <Button
                disabled={snapshot.isPending}
                onClick={() => snapshot.mutate()}
              >
                snapshot
              </Button>
              {branch !== "main" && (
                <ConfirmButton
                  tone="danger"
                  disabled={remove.isPending}
                  onConfirm={() => remove.mutate()}
                >
                  delete branch
                </ConfirmButton>
              )}
              {snapshot.isSuccess && (
                <span className="font-mono text-xs text-brand-muted">
                  snapshot @ lsn {snapshot.data.lsn}
                </span>
              )}
            </div>
          )}
          {checkout.isSuccess && !q.data.connection_string && (
            <p className="mt-2 font-mono text-xs text-brand-muted">
              checked out — refreshing…
            </p>
          )}
          {err && (
            <div className="mt-2">
              <ErrorNote error={err} />
            </div>
          )}
          {!native && (
            <div className="mt-4">
              <NativeNotice />
            </div>
          )}
          {status.isError && <ErrorNote error={status.error} />}
          {capture && (
            <div className="mt-4 border border-brand-edge p-3 text-sm leading-6">
              <p className="text-brand-text">
                Capture: {capture.state} · actor:{" "}
                {capture.actor || "unassigned"}
              </p>
              {capture.updated_at && (
                <p className="text-brand-text-darker">
                  Status updated {new Date(capture.updated_at).toLocaleString()}
                </p>
              )}
              {capture.last_captured_at && (
                <p className="text-brand-text-darker">
                  Last checkpoint{" "}
                  {new Date(capture.last_captured_at).toLocaleString()} ·
                  captured LSN {capture.head_lsn ?? "unknown"}
                </p>
              )}
              {capture.last_event_lag_ms !== undefined && (
                <p className="text-brand-text-darker">
                  Last observed capture delay: {capture.last_event_lag_ms} ms
                  (not current queue lag)
                </p>
              )}
              {capture.error && (
                <p role="alert" className="break-words text-red-400">
                  {capture.error}
                </p>
              )}
              <p className="text-brand-text-darker">
                Actor attribution is shared by this branch/run, not individual
                driver clients. Drop/rename DDL is unsupported and marks history
                incomplete.
              </p>
            </div>
          )}
          {!readOnly && native && q.data.branch.state === "live" && (
            <p className="mt-3 font-mono text-xs text-brand-muted">
              Keep the engine running. Confirm capture is healthy before
              reviewing native writes; incomplete images can prevent undo.
            </p>
          )}
        </>
      )}
    </Panel>
  );
}

function Diff({ project, branch }: { project: string; branch: string }) {
  const readOnly = useReadOnly();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const diff = useQuery({
    queryKey: ["diff", project, branch],
    queryFn: () => api.diff(project, branch),
    enabled: open,
  });
  const preview = useMutation({
    mutationFn: () => api.mergePreview(project, branch),
    onSuccess: (plan) => {
      qc.invalidateQueries({ queryKey: ["plans", project] });
      navigate(`/p/${project}/merges/${plan.id}`);
    },
  });

  useEffect(() => {
    if (open && diff.isSuccess) track("first_diff");
  }, [open, diff.isSuccess]);

  if (branch === "main") return null;
  return (
    <Panel title="03 · Diff — what a merge would change" anchor="diff">
      {!open && (
        <Button onClick={() => setOpen(true)}>
          compute diff against parent
        </Button>
      )}
      {open && diff.isFetching && <Loading what="diff" />}
      {open && diff.isError && <ErrorNote error={diff.error} />}
      {open && diff.data && (
        <>
          <p className="font-mono text-xs text-brand-text-darker">
            {diff.data.source_branch} → {diff.data.target_branch} ·{" "}
            {(diff.data.changes ?? []).length} changes ·{" "}
            <span
              className={
                (diff.data.conflicts ?? []).length ? "text-red-400" : ""
              }
            >
              {(diff.data.conflicts ?? []).length} conflicts
            </span>
          </p>
          <div className="mt-2 space-y-1">
            {(diff.data.changes ?? []).slice(0, 20).map((ch, i) => (
              <p key={i} className="font-mono text-xs">
                <span
                  className={ch.delete ? "text-red-400" : "text-brand-primary"}
                >
                  {ch.delete ? "delete" : "put"}
                </span>{" "}
                <span className="text-brand-text-darker">
                  {ch.collection}/{ch.document_id}
                </span>
              </p>
            ))}
            {(diff.data.changes ?? []).length > 20 && (
              <p className="font-mono text-xs text-brand-muted">
                … {(diff.data.changes ?? []).length - 20} more
              </p>
            )}
          </div>
          {!readOnly && (diff.data.changes ?? []).length > 0 && (
            <div className="mt-3 border-t border-brand-edge pt-3">
              <Button
                tone="solid"
                disabled={preview.isPending}
                onClick={() => preview.mutate()}
              >
                create merge plan
              </Button>
              <span className="ml-2 font-mono text-xs text-brand-muted">
                persists a reviewable data PR
              </span>
            </div>
          )}
          {preview.isError && <ErrorNote error={preview.error} />}
        </>
      )}
    </Panel>
  );
}

function Undo({
  project,
  branch,
  actors,
  request,
}: {
  project: string;
  branch: string;
  actors: string[];
  request: UndoRequest | null;
}) {
  const readOnly = useReadOnly();
  const qc = useQueryClient();
  const [fromLSN, setFromLSN] = useState("");
  const [toLSN, setToLSN] = useState("");
  const [actor, setActor] = useState("");
  type Scope = { from_lsn: number; to_lsn?: number; actor?: string };
  type Review = { result: UndoResult; scope: Scope };
  const [review, setReview] = useState<Review | null>(null);
  const revision = useRef(0);
  const plan = review?.result;
  const invalidateReview = () => {
    revision.current += 1;
    setReview(null);
  };

  const run = useMutation({
    mutationFn: (args: { dry: boolean; scope: Scope; revision: number }) =>
      api.undo(project, branch, { ...args.scope, dry_run: args.dry }),
    onSuccess: (res, args) => {
      // A slow response must not restore a review invalidated by an edit.
      if (args.revision === revision.current) {
        setReview({
          result: res,
          scope: {
            from_lsn: res.from_lsn,
            to_lsn: res.to_lsn,
            ...(args.scope.actor ? { actor: args.scope.actor } : {}),
          },
        });
      }
      if (!res.dry_run) {
        track("first_undo");
        qc.invalidateQueries({ queryKey: ["entries", project, branch] });
        qc.invalidateQueries({ queryKey: ["history-asc", project, branch] });
        qc.invalidateQueries({ queryKey: ["branch", project, branch] });
        qc.invalidateQueries({ queryKey: ["tt-info", project, branch] });
      }
    },
  });
  const runMut = run.mutate;

  // The agent lens can stage an actor's writes: fill the form and dry-run
  // immediately, so "undo agent:executor" is one click over there.
  useEffect(() => {
    if (!request) return;
    const f = String(request.fromLSN);
    setFromLSN(f);
    setToLSN("");
    setActor(request.actor);
    revision.current += 1;
    setReview(null);
    runMut({
      dry: true,
      scope: { from_lsn: request.fromLSN, actor: request.actor },
      revision: revision.current,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [request?.nonce]);

  if (readOnly) return null;
  return (
    <Panel title="06 · Undo — append-only compensations" anchor="undo">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[11px] text-brand-muted">from lsn</span>
        <TextInput
          value={fromLSN}
          onChange={(value) => {
            invalidateReview();
            setFromLSN(value);
          }}
          width="w-24"
          placeholder="990"
        />
        <span className="font-mono text-[11px] text-brand-muted">
          to lsn (opt)
        </span>
        <TextInput
          value={toLSN}
          onChange={(value) => {
            invalidateReview();
            setToLSN(value);
          }}
          width="w-24"
          placeholder="head"
        />
        <span className="font-mono text-[11px] text-brand-muted">
          actor (opt)
        </span>
        <Select
          value={actor}
          onChange={(value) => {
            invalidateReview();
            setActor(value);
          }}
          options={[
            { value: "", label: "any" },
            ...actors.map((a) => ({ value: a, label: a })),
          ]}
        />
        <Button
          disabled={!fromLSN || run.isPending}
          onClick={() => {
            invalidateReview();
            run.mutate({
              dry: true,
              scope: {
                from_lsn: Number(fromLSN),
                ...(toLSN ? { to_lsn: Number(toLSN) } : {}),
                ...(actor ? { actor } : {}),
              },
              revision: revision.current,
            });
          }}
        >
          dry run
        </Button>
      </div>
      {run.isError && (
        <div className="mt-2">
          <ErrorNote error={run.error} />
        </div>
      )}
      {plan && (
        <div className="mt-3 border-t border-brand-edge pt-3">
          <p className="font-mono text-xs text-brand-text-darker">
            lsn {plan.from_lsn}–{plan.to_lsn} · {plan.compensations}{" "}
            compensations ·{" "}
            <span className={plan.conflicts ? "text-red-400" : ""}>
              {plan.conflicts} conflicts
            </span>{" "}
            ·{" "}
            <span className={plan.unrecoverable ? "text-red-400" : ""}>
              {plan.unrecoverable} unrecoverable
            </span>
            {!plan.dry_run && (
              <span className="text-brand-primary">
                {" "}
                — applied: {plan.restored ?? 0} restored, {plan.deleted ?? 0}{" "}
                deleted
              </span>
            )}
          </p>
          {plan.dry_run &&
            plan.compensations > 0 &&
            plan.unrecoverable === 0 && (
              <div className="mt-2">
                <p className="mb-2 font-mono text-xs text-brand-muted">
                  Reviewed actor: {review?.scope.actor || "all actors"}. Applies
                  only through LSN {plan.to_lsn}; later writes are excluded.
                </p>
                <ConfirmButton
                  tone="danger"
                  disabled={run.isPending}
                  onConfirm={() => {
                    if (review)
                      run.mutate({
                        dry: false,
                        scope: review.scope,
                        revision: revision.current,
                      });
                  }}
                >
                  apply undo · {plan.compensations} compensations
                </ConfirmButton>
              </div>
            )}
        </div>
      )}
    </Panel>
  );
}

function Timeline({
  project,
  branch,
  onActors,
}: {
  project: string;
  branch: string;
  onActors: (actors: string[]) => void;
}) {
  const demo = useDemo();
  const [actor, setActor] = useState("");
  const [pages, setPages] = useState(1);
  const limit = 25;

  const q = useQuery({
    queryKey: ["entries", project, branch, actor, pages],
    queryFn: () =>
      api.entries(project, branch, {
        limit: limit * pages,
        ...(actor ? { actor } : {}),
      }),
    refetchInterval: 5000,
  });

  const entries = useMemo(() => q.data?.entries ?? [], [q.data]);
  const actors = useMemo(() => {
    const set = new Set<string>();
    for (const e of entries) if (e.actor) set.add(e.actor);
    return [...set].sort();
  }, [entries]);
  // Report upward only when the set actually changes — a setState here on
  // every render would loop.
  const actorsKey = actors.join("|");
  useEffect(() => {
    onActors(actorsKey ? actorsKey.split("|") : []);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [actorsKey]);

  return (
    <Panel title="05 · History — newest first">
      <div className="mb-3 flex items-center gap-2">
        <label className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
          actor
        </label>
        <Select
          value={actor}
          onChange={(v) => {
            setActor(v);
            setPages(1);
          }}
          options={[
            { value: "", label: "all" },
            ...actors.map((a) => ({ value: a, label: a })),
          ]}
        />
      </div>

      {q.isPending && <Loading what="history" />}
      {q.isError && <ErrorNote error={q.error} />}
      {q.data && entries.length === 0 && (
        <p className="text-sm text-brand-text-darker">
          {demo ? (
            <>
              No writes yet. Run the sample agent session from the project page,
              or <LocalLink>start locally</LocalLink>.
            </>
          ) : (
            <>
              No captured writes yet. Check out a branch and keep capture
              healthy while writing with your driver. CLI:{" "}
              <Mono>argon checkout</Mono> + <Mono>argon watch</Mono>.
            </>
          )}
        </p>
      )}
      {entries.length > 0 && (
        <>
          <table className="w-full text-left text-sm">
            <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
              <tr>
                <th className="py-1 font-normal">lsn</th>
                <th className="py-1 font-normal">op</th>
                <th className="py-1 font-normal">document</th>
                <th className="py-1 font-normal">actor</th>
                <th className="py-1 font-normal">at</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e: Entry) => (
                <tr
                  key={`${e.lsn}-${e.document_id ?? ""}`}
                  className="border-t border-brand-edge"
                >
                  <td className="py-1.5 font-mono text-xs">{e.lsn}</td>
                  <td className="py-1.5">
                    <span
                      className={`font-mono text-xs ${
                        e.operation === "delete"
                          ? "text-red-400"
                          : "text-brand-primary"
                      }`}
                    >
                      {e.operation}
                    </span>
                  </td>
                  <td className="py-1.5 font-mono text-xs text-brand-text-darker">
                    {e.collection
                      ? `${e.collection}/${e.document_id ?? ""}`
                      : "—"}
                  </td>
                  <td className="py-1.5">
                    {e.actor && (
                      <span
                        className="font-mono text-xs"
                        style={{ color: actorColor(e.actor) }}
                      >
                        {e.actor}
                      </span>
                    )}
                  </td>
                  <td className="py-1.5 font-mono text-xs text-brand-muted">
                    {new Date(e.timestamp).toLocaleTimeString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.data?.has_more && (
            <div className="mt-3">
              <Button onClick={() => setPages((n) => n + 1)}>load more</Button>
            </div>
          )}
        </>
      )}
    </Panel>
  );
}

function TimeTravel({ project, branch }: { project: string; branch: string }) {
  const info = useQuery({
    queryKey: ["tt-info", project, branch],
    queryFn: () => api.timeTravelInfo(project, branch),
  });
  const history = useHistory(project, branch);

  const [lsn, setLsn] = useState<number | null>(null);
  const [collection, setCollection] = useState("");
  const [playing, setPlaying] = useState(false);
  const effectiveLSN = lsn ?? info.data?.LatestLSN ?? 0;

  // The frames worth landing on: LSNs where a data entry changed state.
  // Falls back to the endpoints if nothing carries a collection yet.
  const stops = useMemo(() => {
    const s = new Set<number>();
    for (const e of history.data?.entries ?? []) {
      if (e.collection) s.add(e.lsn);
    }
    if (info.data) {
      s.add(info.data.EarliestLSN);
      s.add(info.data.LatestLSN);
    }
    return [...s].sort((a, b) => a - b);
  }, [history.data, info.data]);

  // Playback: advance to the next stop on a timer; stop at the end.
  useEffect(() => {
    if (!playing || stops.length === 0) return;
    const id = setInterval(() => {
      setLsn((cur) => {
        const at = cur ?? stops[stops.length - 1];
        const next = stops.find((s) => s > at);
        if (next === undefined) {
          setPlaying(false);
          return at;
        }
        return next;
      });
    }, 900);
    return () => clearInterval(id);
  }, [playing, stops]);

  const summary = useQuery({
    queryKey: ["tt-summary", project, branch, effectiveLSN],
    queryFn: () => api.timeTravelSummary(project, branch, effectiveLSN),
    enabled: !!info.data && info.data.EntryCount > 0,
    placeholderData: (prev) => prev, // hold the last frame while the next loads — no flicker
  });
  const docs = useQuery({
    queryKey: ["tt-docs", project, branch, effectiveLSN, collection],
    queryFn: () =>
      api.timeTravelDocs(project, branch, effectiveLSN, collection),
    enabled: !!collection && !!info.data,
    placeholderData: (prev) => prev,
  });

  if (info.isPending)
    return (
      <Panel title="04 · Time travel">
        <Loading what="time travel" />
      </Panel>
    );
  if (info.isError)
    return (
      <Panel title="04 · Time travel">
        <ErrorNote error={info.error} />
      </Panel>
    );
  const i = info.data!;
  if (i.EntryCount === 0) {
    return (
      <Panel title="04 · Time travel">
        <p className="text-sm text-brand-text-darker">
          No data entries to travel through yet.
        </p>
      </Panel>
    );
  }

  const stepTo = (dir: 1 | -1) => {
    setPlaying(false);
    setLsn((cur) => {
      const at = cur ?? i.LatestLSN;
      const next =
        dir === 1
          ? stops.find((s) => s > at)
          : [...stops].reverse().find((s) => s < at);
      return next ?? at;
    });
  };

  return (
    <Panel
      title="04 · Time travel — scrub the branch's history"
      anchor="timetravel"
    >
      <div className="flex flex-wrap items-center gap-2">
        <Button onClick={() => stepTo(-1)} disabled={effectiveLSN <= stops[0]}>
          ◀
        </Button>
        <Button
          tone="solid"
          onClick={() => {
            if (effectiveLSN >= i.LatestLSN && !playing) setLsn(stops[0]);
            setPlaying((p) => !p);
          }}
        >
          {playing ? "⏸ pause" : "▶ play"}
        </Button>
        <Button
          onClick={() => stepTo(1)}
          disabled={effectiveLSN >= i.LatestLSN}
        >
          ▶
        </Button>
        <span className="font-mono text-xs text-brand-muted">
          lsn {i.EarliestLSN}
        </span>
        <input
          aria-label="History position (LSN)"
          type="range"
          min={i.EarliestLSN}
          max={i.LatestLSN}
          value={effectiveLSN}
          onChange={(e) => {
            setPlaying(false);
            setLsn(Number(e.target.value));
          }}
          className="w-56 accent-[#96A7FF]"
        />
        <span className="font-mono text-xs text-brand-muted">
          {i.LatestLSN}
        </span>
        <Badge tone="primary">@ {effectiveLSN}</Badge>
        {effectiveLSN !== i.LatestLSN && (
          <button
            className="font-mono text-xs text-brand-muted hover:text-brand-primary"
            onClick={() => {
              setPlaying(false);
              setLsn(null);
            }}
          >
            back to head
          </button>
        )}
      </div>

      <div className="mt-4">
        {!summary.data && summary.isPending && <Loading what="state" />}
        {summary.data && (
          <div className="flex flex-wrap gap-2">
            {Object.entries(summary.data.collections)
              .sort()
              .map(([name, count]) => (
                <button
                  key={name}
                  onClick={() => setCollection(name === collection ? "" : name)}
                  className={`border px-2 py-1 font-mono text-xs ${
                    name === collection
                      ? "border-brand-primary text-brand-primary"
                      : "border-brand-edge text-brand-text-darker hover:border-brand-primary/60"
                  }`}
                >
                  {name} · {count}
                </button>
              ))}
            {Object.keys(summary.data.collections).length === 0 && (
              <p className="text-sm text-brand-text-darker">
                Empty at this LSN.
              </p>
            )}
          </div>
        )}
      </div>

      {collection && (
        <div className="mt-4">
          {!docs.data && docs.isPending && <Loading what={collection} />}
          {docs.isError && <ErrorNote error={docs.error} />}
          {docs.data && (
            <>
              <p className="mb-2 font-mono text-[11px] uppercase tracking-widest text-brand-muted">
                {collection} @ lsn {docs.data.lsn} · {docs.data.total} docs
                {docs.data.total > docs.data.documents.length &&
                  ` (showing ${docs.data.documents.length})`}
              </p>
              <Json value={docs.data.documents} />
            </>
          )}
        </div>
      )}
    </Panel>
  );
}

// The agent lens: who wrote what on this branch, and one-click undo of a
// single actor's writes. This is the "undo button for AI agents" made
// concrete — a session is a set of actors, and any one can be reverted.
function AgentLens({
  project,
  branch,
  onUndoActor,
}: {
  project: string;
  branch: string;
  onUndoActor: (fromLSN: number, actor: string) => void;
}) {
  const readOnly = useReadOnly();
  const history = useHistory(project, branch);
  const stats = useMemo(
    () => actorStats(history.data?.entries ?? []),
    [history.data],
  );

  if (history.isPending)
    return (
      <Panel title="4.5 · Agents">
        <Loading what="actors" />
      </Panel>
    );
  if (stats.length === 0) return null;

  return (
    <Panel title="4.5 · Agents — who wrote what" anchor="agents">
      <p className="mb-4 text-sm leading-6 text-brand-text-darker">
        Scripted API writes carry explicit actors. Native drivers share the
        actor configured for their branch/run. Undo requires complete, retained
        history; always inspect the dry run.
      </p>
      <div className="space-y-2">
        {stats.map((s) => (
          <div
            key={s.actor}
            className="flex flex-wrap items-center gap-3 border-l-2 pl-3"
            style={{ borderColor: actorColor(s.actor) }}
          >
            <span
              className="font-mono text-xs"
              style={{ color: actorColor(s.actor) }}
            >
              {s.actor}
            </span>
            <span className="font-mono text-[11px] text-brand-text-darker">
              {s.count} {s.count === 1 ? "write" : "writes"} · from lsn{" "}
              {s.minLSN} · {s.collections.join(", ")}
            </span>
            {!readOnly && (
              <button
                className="ml-auto font-mono text-[11px] text-brand-muted hover:text-red-400"
                onClick={() => onUndoActor(s.minLSN, s.actor)}
              >
                undo this actor →
              </button>
            )}
          </div>
        ))}
      </div>
    </Panel>
  );
}

export default function Branch() {
  const { project = "", branch = "" } = useParams();
  const [actors, setActors] = useState<string[]>([]);
  const [undoRequest, setUndoRequest] = useState<UndoRequest | null>(null);
  const undoRef = useRef<HTMLDivElement>(null);

  const requestUndo = (fromLSN: number, actor: string) => {
    setUndoRequest({ fromLSN, actor, nonce: Date.now() });
    undoRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  };

  return (
    <div className="space-y-6">
      <nav className="font-mono text-xs text-brand-muted">
        <Link to="/" className="hover:text-brand-primary">
          projects
        </Link>{" "}
        /{" "}
        <Link to={`/p/${project}`} className="hover:text-brand-primary">
          {project}
        </Link>{" "}
        / {branch}
      </nav>

      <Actions project={project} branch={branch} />
      <Diff project={project} branch={branch} />
      <TimeTravel project={project} branch={branch} />
      <AgentLens project={project} branch={branch} onUndoActor={requestUndo} />
      <Timeline project={project} branch={branch} onActors={setActors} />
      <div ref={undoRef}>
        <Undo
          key={`${project}/${branch}`}
          project={project}
          branch={branch}
          actors={actors}
          request={undoRequest}
        />
      </div>
    </div>
  );
}
