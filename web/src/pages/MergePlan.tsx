import { LocalLink, track } from "../funnel";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useReadOnly } from "../hooks";
import {
  Badge,
  ConfirmButton,
  ErrorNote,
  Json,
  Loading,
  Panel,
  Select,
} from "../ui";

export default function MergePlan() {
  const { project = "", id = "" } = useParams();
  const readOnly = useReadOnly();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["plan", id],
    queryFn: () => api.mergePlan(id),
  });
  const [strategy, setStrategy] = useState("");

  const apply = useMutation({
    mutationFn: () => api.mergeApply(id, strategy || undefined),
    onSuccess: () => {
      track("first_merge");
      qc.invalidateQueries({ queryKey: ["plan", id] });
      qc.invalidateQueries({ queryKey: ["plans", project] });
      qc.invalidateQueries({ queryKey: ["branches", project] });
    },
  });

  const conflicts = q.data?.conflicts ?? [];
  const canApply = q.data?.status === "pending";

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
        /{" "}
        <Link to={`/p/${project}/merges`} className="hover:text-brand-primary">
          merge plans
        </Link>{" "}
        / {id.slice(-8)}
      </nav>

      {q.isPending && <Loading what="plan" />}
      {q.isError && <ErrorNote error={q.error} />}
      {q.data && (
        <>
          <Panel
            title={`Plan · ${q.data.source_branch} → ${q.data.target_branch}`}
          >
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <Badge>{q.data.status}</Badge>
              {q.data.strategy && <Badge>strategy: {q.data.strategy}</Badge>}
              <span className="font-mono text-xs text-brand-text-darker">
                source head {q.data.source_head} · target head{" "}
                {q.data.target_head} · base {q.data.base_lsn}
              </span>
              {q.data.applied_at && (
                <span className="font-mono text-xs text-brand-muted">
                  applied {new Date(q.data.applied_at).toLocaleString()}
                </span>
              )}
            </div>

            {!readOnly && canApply && (
              <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
                {conflicts.length > 0 && (
                  <>
                    <span className="font-mono text-[11px] text-brand-muted">
                      {conflicts.length} conflicts — pick a strategy
                    </span>
                    <Select
                      value={strategy}
                      onChange={setStrategy}
                      options={[
                        { value: "", label: "none (refuse on conflict)" },
                        { value: "theirs", label: "theirs (source wins)" },
                        { value: "ours", label: "ours (target wins)" },
                      ]}
                    />
                  </>
                )}
                <ConfirmButton
                  tone="solid"
                  disabled={apply.isPending}
                  onConfirm={() => apply.mutate()}
                >
                  apply merge
                </ConfirmButton>
                <span className="font-mono text-xs text-brand-muted">
                  applies this reviewed plan; refuses stale heads
                </span>
              </div>
            )}
            {apply.isSuccess && (
              <p className="mt-2 font-mono text-xs text-brand-primary">
                applied {apply.data.applied} changes
                {apply.data.conflicts_resolved > 0 &&
                  `, ${apply.data.conflicts_resolved} conflicts resolved`}
              </p>
            )}
            {apply.isSuccess && (
              <p className="mt-3 text-sm">
                <LocalLink>
                  Run this review with your own local database →
                </LocalLink>
              </p>
            )}
            {apply.isError && (
              <div className="mt-2">
                <ErrorNote error={apply.error} />
              </div>
            )}
            {(readOnly || !canApply) && !apply.isSuccess && (
              <p className="mt-3 font-mono text-xs text-brand-muted">
                CLI equivalent: argon merge apply {q.data.id}
              </p>
            )}
          </Panel>

          <Panel title={`Changes · ${(q.data.changes ?? []).length}`}>
            <div className="space-y-2">
              {(q.data.changes ?? []).map((ch, i) => (
                <details key={i} className="border border-brand-edge">
                  <summary className="cursor-pointer px-3 py-2 font-mono text-xs">
                    <span
                      className={
                        ch.delete ? "text-red-400" : "text-brand-primary"
                      }
                    >
                      {ch.delete ? "delete" : "put"}
                    </span>{" "}
                    {ch.collection}/{ch.document_id}
                  </summary>
                  {ch.document && (
                    <div className="border-t border-brand-edge p-2">
                      <Json value={ch.document} />
                    </div>
                  )}
                </details>
              ))}
              {(q.data.changes ?? []).length === 0 && (
                <p className="text-sm text-brand-text-darker">
                  Nothing to merge.
                </p>
              )}
            </div>
          </Panel>

          {conflicts.length > 0 && (
            <Panel
              title={`Conflicts · ${conflicts.length} — never resolved silently`}
            >
              <div className="space-y-3">
                {conflicts.map((cf, i) => (
                  <div key={i} className="border border-red-400/40">
                    <p className="border-b border-brand-edge px-3 py-2 font-mono text-xs text-red-400">
                      {cf.collection}/{cf.document_id}
                    </p>
                    <div className="grid gap-2 p-2 md:grid-cols-3">
                      <div>
                        <p className="mb-1 font-mono text-[11px] text-brand-muted">
                          base
                        </p>
                        <Json value={cf.base ?? null} />
                      </div>
                      <div>
                        <p className="mb-1 font-mono text-[11px] text-brand-muted">
                          ours (target)
                        </p>
                        <Json value={cf.ours ?? null} />
                      </div>
                      <div>
                        <p className="mb-1 font-mono text-[11px] text-brand-muted">
                          theirs (source)
                        </p>
                        <Json value={cf.theirs ?? null} />
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            </Panel>
          )}
        </>
      )}
    </div>
  );
}
