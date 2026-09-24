import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { Badge, ErrorNote, Loading, Panel } from "../ui";

export default function MergePlans() {
  const { project = "" } = useParams();
  const q = useQuery({ queryKey: ["plans", project], queryFn: () => api.mergePlans(project) });

  return (
    <div className="space-y-6">
      <nav className="font-mono text-xs text-brand-muted">
        <Link to="/" className="hover:text-brand-primary">projects</Link> /{" "}
        <Link to={`/p/${project}`} className="hover:text-brand-primary">{project}</Link> / merge plans
      </nav>

      <Panel title="03 · Merge plans — data pull requests">
        {q.isPending && <Loading what="merge plans" />}
        {q.isError && <ErrorNote error={q.error} />}
        {q.data &&
          ((q.data.plans ?? []).length === 0 ? (
            <p className="text-sm text-brand-text-darker">
              No plans yet. <span className="font-mono">argon merge preview</span> persists a
              reviewable plan.
            </p>
          ) : (
            <table className="w-full text-left text-sm">
              <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
                <tr>
                  <th className="py-1 font-normal">merge</th>
                  <th className="py-1 font-normal">changes</th>
                  <th className="py-1 font-normal">conflicts</th>
                  <th className="py-1 font-normal">status</th>
                  <th className="py-1 font-normal">created</th>
                </tr>
              </thead>
              <tbody>
                {(q.data.plans ?? []).map((plan) => (
                  <tr key={plan.id} className="border-t border-brand-edge">
                    <td className="py-2">
                      <Link
                        className="font-mono text-xs text-brand-primary hover:underline"
                        to={`/p/${project}/merges/${plan.id}`}
                      >
                        {plan.source_branch} → {plan.target_branch}
                      </Link>
                    </td>
                    <td className="py-2 font-mono text-xs">{(plan.changes ?? []).length}</td>
                    <td className="py-2 font-mono text-xs">
                      {(plan.conflicts ?? []).length > 0 ? (
                        <span className="text-red-400">{(plan.conflicts ?? []).length}</span>
                      ) : (
                        0
                      )}
                    </td>
                    <td className="py-2"><Badge>{plan.status}</Badge></td>
                    <td className="py-2 font-mono text-xs text-brand-text-darker">
                      {new Date(plan.created_at).toLocaleString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ))}
      </Panel>
    </div>
  );
}
