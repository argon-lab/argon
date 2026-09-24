import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useReadOnly, useDemo } from "../hooks";
import { Button, ErrorNote, Loading, Mono, Panel, TextInput } from "../ui";

export default function Projects() {
  const q = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const readOnly = useReadOnly();
  const demo = useDemo();
  const qc = useQueryClient();
  const [name, setName] = useState("");

  const create = useMutation({
    mutationFn: () => api.createProject(name.trim()),
    onSuccess: () => {
      setName("");
      qc.invalidateQueries({ queryKey: ["projects"] });
    },
  });

  return (
    <Panel title="01 · Projects">
      {q.isPending && <Loading what="projects" />}
      {q.isError && <ErrorNote error={q.error} />}
      {q.data &&
        ((q.data.projects ?? []).length === 0 ? (
          <p className="text-sm text-brand-text-darker">
            No projects yet. Create one below, or import an existing database
            with <Mono>argon import database</Mono>.
          </p>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
              <tr>
                <th className="py-1 font-normal">name</th>
                <th className="py-1 font-normal">created</th>
                <th className="py-1 font-normal">id</th>
              </tr>
            </thead>
            <tbody>
              {(q.data.projects ?? []).map((p) => (
                <tr key={p.id} className="border-t border-brand-edge">
                  <td className="py-2">
                    <Link className="text-brand-primary hover:underline" to={`/p/${p.name}`}>
                      {p.name}
                    </Link>
                  </td>
                  <td className="py-2 font-mono text-xs text-brand-text-darker">
                    {new Date(p.created_at).toLocaleString()}
                  </td>
                  <td className="py-2 font-mono text-xs text-brand-muted">{p.id}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}

      {!readOnly && !demo && (
        <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
          <TextInput value={name} onChange={setName} placeholder="new-project" />
          <Button
            tone="solid"
            disabled={!name.trim() || create.isPending}
            onClick={() => create.mutate()}
          >
            create project
          </Button>
          {create.isError && <ErrorNote error={create.error} />}
        </div>
      )}
    </Panel>
  );
}
