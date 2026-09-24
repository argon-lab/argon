import { NativeNotice } from "../funnel";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, Branch } from "../api";
import { useDemo, useReadOnly, useNativeConnections } from "../hooks";
import {
  Badge,
  Button,
  ConfirmButton,
  Dot,
  ErrorNote,
  Loading,
  Panel,
  Select,
  TextInput,
} from "../ui";

// Order branches parent-first so the tree reads as a DAG outline.
function outline(branches: Branch[]): { branch: Branch; depth: number }[] {
  const byParent = new Map<string, Branch[]>();
  const byID = new Map(branches.map((b) => [b.id, b]));
  for (const b of branches) {
    const parent = b.parent_id && byID.has(b.parent_id) ? b.parent_id : "";
    const list = byParent.get(parent) ?? [];
    list.push(b);
    byParent.set(parent, list);
  }
  const rows: { branch: Branch; depth: number }[] = [];
  const walk = (parent: string, depth: number) => {
    for (const b of byParent.get(parent) ?? []) {
      rows.push({ branch: b, depth });
      walk(b.id, depth + 1);
    }
  };
  walk("", 0);
  return rows;
}

function ttl(expires: string): string {
  const ms = new Date(expires).getTime() - Date.now();
  if (ms <= 0) return "expired";
  const min = Math.round(ms / 60000);
  return min >= 90 ? `ttl ${Math.round(min / 60)}h` : `ttl ${min}m`;
}

function NewBranch({
  project,
  branches,
}: {
  project: string;
  branches: Branch[];
}) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [from, setFrom] = useState("main");
  const create = useMutation({
    mutationFn: () => api.createBranch(project, name.trim(), from),
    onSuccess: () => {
      setName("");
      qc.invalidateQueries({ queryKey: ["branches", project] });
    },
  });
  return (
    <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
      <TextInput value={name} onChange={setName} placeholder="new-branch" />
      <span className="font-mono text-[11px] text-brand-muted">from</span>
      <Select
        value={from}
        onChange={setFrom}
        options={branches.map((b) => ({ value: b.name, label: b.name }))}
      />
      <Button
        tone="solid"
        disabled={!name.trim() || create.isPending}
        onClick={() => create.mutate()}
      >
        create branch
      </Button>
      {create.isError && <ErrorNote error={create.error} />}
    </div>
  );
}

function Sandboxes({
  project,
  branches,
}: {
  project: string;
  branches: Branch[];
}) {
  const readOnly = useReadOnly();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["sandboxes", project],
    queryFn: () => api.sandboxes(project),
    refetchInterval: 15000,
  });
  const [from, setFrom] = useState("main");
  const [mins, setMins] = useState("60");
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["sandboxes", project] });
    qc.invalidateQueries({ queryKey: ["branches", project] });
  };
  const create = useMutation({
    mutationFn: () =>
      api.createSandbox(project, { from, ttl_minutes: Number(mins) || 60 }),
    onSuccess: refresh,
  });
  const act = useMutation({
    mutationFn: async ({
      verb,
      name,
    }: {
      verb: "extend" | "keep" | "discard";
      name: string;
    }): Promise<unknown> => {
      if (verb === "extend")
        return api.extendSandbox(project, name, Number(mins) || 60);
      if (verb === "keep") return api.keepSandbox(project, name);
      return api.discardSandbox(project, name);
    },
    onSuccess: refresh,
  });

  const boxes = q.data?.sandboxes ?? [];
  return (
    <Panel title="06 · Sandboxes — disposable agent branches">
      {q.isPending && <Loading what="sandboxes" />}
      {q.isError && <ErrorNote error={q.error} />}
      {q.data && boxes.length === 0 && (
        <p className="text-sm text-brand-text-darker">
          No live sandboxes. A sandbox forks a branch, checks it out and expires
          on a TTL.
        </p>
      )}
      {boxes.length > 0 && (
        <table className="w-full text-left text-sm">
          <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
            <tr>
              <th className="py-1 font-normal">sandbox</th>
              <th className="py-1 font-normal">expires</th>
              <th className="py-1 font-normal" />
            </tr>
          </thead>
          <tbody>
            {boxes.map((s) => (
              <tr key={s.branch.id} className="border-t border-brand-edge">
                <td className="py-2">
                  <Link
                    className="text-brand-primary hover:underline"
                    to={`/p/${project}/b/${s.branch.name}`}
                  >
                    {s.branch.name}
                  </Link>
                </td>
                <td className="py-2">
                  {s.branch.expires_at && (
                    <Badge tone="primary">{ttl(s.branch.expires_at)}</Badge>
                  )}
                </td>
                <td className="py-2">
                  {!readOnly && (
                    <span className="flex flex-wrap justify-end gap-2">
                      <Button
                        onClick={() =>
                          act.mutate({ verb: "extend", name: s.branch.name })
                        }
                        title={`extend by ${mins} minutes`}
                      >
                        extend
                      </Button>
                      <Button
                        onClick={() =>
                          act.mutate({ verb: "keep", name: s.branch.name })
                        }
                      >
                        keep
                      </Button>
                      <ConfirmButton
                        tone="danger"
                        onConfirm={() =>
                          act.mutate({ verb: "discard", name: s.branch.name })
                        }
                      >
                        discard
                      </ConfirmButton>
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {act.isError && <ErrorNote error={act.error} />}
      {!readOnly && (
        <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
          <span className="font-mono text-[11px] text-brand-muted">from</span>
          <Select
            value={from}
            onChange={setFrom}
            options={branches.map((b) => ({ value: b.name, label: b.name }))}
          />
          <span className="font-mono text-[11px] text-brand-muted">
            ttl (min)
          </span>
          <TextInput value={mins} onChange={setMins} width="w-16" />
          <Button
            tone="solid"
            disabled={create.isPending}
            onClick={() => create.mutate()}
          >
            create sandbox
          </Button>
          {create.isError && <ErrorNote error={create.error} />}
        </div>
      )}
    </Panel>
  );
}

function BranchFromPin({ onCreate }: { onCreate: (name: string) => void }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  if (!open) return <Button onClick={() => setOpen(true)}>branch</Button>;
  return (
    <span className="inline-flex items-center gap-2">
      <TextInput
        value={name}
        onChange={setName}
        placeholder="branch-name"
        width="w-32"
      />
      <Button
        tone="solid"
        disabled={!name.trim()}
        onClick={() => {
          onCreate(name.trim());
          setName("");
          setOpen(false);
        }}
      >
        create
      </Button>
      <Button onClick={() => setOpen(false)}>cancel</Button>
    </span>
  );
}

function Pins({ project, branches }: { project: string; branches: Branch[] }) {
  const native = useNativeConnections();
  const readOnly = useReadOnly();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["pins", project],
    queryFn: () => api.pins(project),
  });
  const [name, setName] = useState("");
  const [branch, setBranch] = useState("main");
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["pins", project] });
    qc.invalidateQueries({ queryKey: ["branches", project] });
    qc.invalidateQueries({ queryKey: ["sandboxes", project] });
  };
  const create = useMutation({
    mutationFn: () => api.createPin(project, { name: name.trim(), branch }),
    onSuccess: () => {
      setName("");
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (pin: string) => api.deletePin(project, pin),
    onSuccess: refresh,
  });
  const sandboxFrom = useMutation({
    mutationFn: (pin: string) => api.sandboxFromPin(project, pin),
    onSuccess: refresh,
  });
  const branchFrom = useMutation({
    mutationFn: ({ pin, newName }: { pin: string; newName: string }) =>
      api.branchFromPin(project, pin, newName),
    onSuccess: refresh,
  });

  const pins = q.data?.pins ?? [];
  const byID = new Map(branches.map((b) => [b.id, b.name]));
  return (
    <Panel title="07 · Pins — immutable datasets">
      {q.isPending && <Loading what="pins" />}
      {q.isError && <ErrorNote error={q.error} />}
      {q.data && pins.length === 0 && (
        <p className="text-sm text-brand-text-darker">
          No pins. A pin protects a named history position while it exists. Fork
          both agents from one pin to compare identical starting data.
        </p>
      )}
      {pins.length > 0 && (
        <table className="w-full text-left text-sm">
          <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
            <tr>
              <th className="py-1 font-normal">pin</th>
              <th className="py-1 font-normal">points at</th>
              <th className="py-1 font-normal">note</th>
              <th className="py-1 font-normal" />
            </tr>
          </thead>
          <tbody>
            {pins.map((pin) => (
              <tr key={pin.name} className="border-t border-brand-edge">
                <td className="py-2 font-mono text-xs text-brand-primary">
                  {pin.name}
                </td>
                <td className="py-2 font-mono text-xs">
                  {byID.get(pin.branch_id) ?? pin.branch_id} @ {pin.lsn}
                </td>
                <td className="py-2 text-xs text-brand-text-darker">
                  {pin.note}
                </td>
                <td className="py-2">
                  {!readOnly && (
                    <span className="flex flex-wrap justify-end gap-2">
                      {native && (
                        <Button onClick={() => sandboxFrom.mutate(pin.name)}>
                          sandbox
                        </Button>
                      )}
                      <BranchFromPin
                        onCreate={(newName) =>
                          branchFrom.mutate({ pin: pin.name, newName })
                        }
                      />
                      <ConfirmButton
                        tone="danger"
                        onConfirm={() => remove.mutate(pin.name)}
                      >
                        delete
                      </ConfirmButton>
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(remove.isError || sandboxFrom.isError || branchFrom.isError) && (
        <ErrorNote
          error={remove.error ?? sandboxFrom.error ?? branchFrom.error}
        />
      )}
      {!readOnly && (
        <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-brand-edge pt-4">
          <TextInput value={name} onChange={setName} placeholder="dataset-v1" />
          <span className="font-mono text-[11px] text-brand-muted">
            pins head of
          </span>
          <Select
            value={branch}
            onChange={setBranch}
            options={branches.map((b) => ({ value: b.name, label: b.name }))}
          />
          <Button
            tone="solid"
            disabled={!name.trim() || create.isPending}
            onClick={() => create.mutate()}
          >
            create pin
          </Button>
          {create.isError && <ErrorNote error={create.error} />}
        </div>
      )}
    </Panel>
  );
}

function AgentScenario({ project }: { project: string }) {
  const qc = useQueryClient();
  const run = useMutation({
    mutationFn: api.demoScenario,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["branches", project] });
      qc.invalidateQueries({ queryKey: ["plans", project] });
    },
  });
  return (
    <Panel title="00 · Run an agent session" anchor="scenario">
      <p className="text-sm text-brand-text-darker">
        Two agents fork the same baseline pin. Planner proposes $44 and the
        script accepts it into main; executor proposes $1 on a separate branch.
        Review that conflict, undo executor on its branch, or undo the accepted
        merge on main.
      </p>
      <div className="mt-3 flex flex-wrap items-center gap-3">
        <Button
          tone="solid"
          disabled={run.isPending}
          onClick={() => run.mutate()}
        >
          run agent session
        </Button>
        {run.data && (
          <span className="font-mono text-xs text-brand-primary">
            {run.data.hint}
          </span>
        )}
      </div>
      {run.data && (
        <div className="mt-4 space-y-2 text-sm">
          <p>
            Shared pin:{" "}
            <strong className="text-brand-text">
              {run.data.pin ?? "baseline"}
            </strong>
          </p>
          <div className="flex flex-wrap gap-4">
            {(run.data.branches ?? [run.data.branch]).map((name) => (
              <Link
                className="text-brand-primary underline"
                key={name}
                to={`/p/${project}/b/${name}`}
              >
                {name === run.data.accepted_branch
                  ? "Planner (accepted)"
                  : "Executor (review conflict)"}{" "}
                →
              </Link>
            ))}
            <Link
              className="text-brand-primary underline"
              to={`/p/${project}/b/main`}
            >
              Main (accepted data) →
            </Link>
          </div>
        </div>
      )}
      {run.isError && (
        <div className="mt-2">
          <ErrorNote error={run.error} />
        </div>
      )}
    </Panel>
  );
}

export default function Project() {
  const { project = "" } = useParams();
  const readOnly = useReadOnly();
  const demo = useDemo();
  const native = useNativeConnections();
  const branches = useQuery({
    queryKey: ["branches", project],
    queryFn: () => api.branches(project),
    refetchInterval: 5000,
  });
  const ingesters = useQuery({
    queryKey: ["ingesters"],
    queryFn: api.ingesters,
    refetchInterval: 5000,
  });
  const plans = useQuery({
    queryKey: ["plans", project],
    queryFn: () => api.mergePlans(project),
  });

  const capture = new Map(
    (ingesters.data?.capture ?? []).map((c) => [c.branch_id, c]),
  );
  const list = branches.data?.branches ?? [];

  return (
    <div className="space-y-6">
      <nav className="font-mono text-xs text-brand-muted">
        <Link to="/" className="hover:text-brand-primary">
          projects
        </Link>{" "}
        / {project}
        <Link
          to={`/p/${project}/merges`}
          className="ml-4 text-brand-primary hover:underline"
        >
          merge plans{plans.data ? ` (${(plans.data.plans ?? []).length})` : ""}
        </Link>
      </nav>

      {demo && <AgentScenario project={project} />}

      <Panel title="02 · Branches">
        <p className="mb-4 text-sm leading-6 text-brand-text-darker">
          LSN is a history position. HEAD is the latest recorded change; BASE is
          the fork point. A live database needs healthy capture before its
          writes are reviewable.
        </p>
        {ingesters.isError && <ErrorNote error={ingesters.error} />}
        {branches.isPending && <Loading what="branches" />}
        {branches.isError && <ErrorNote error={branches.error} />}
        {branches.data && (
          <table className="w-full text-left text-sm">
            <thead className="font-mono text-[11px] uppercase tracking-widest text-brand-muted">
              <tr>
                <th className="py-1 font-normal">branch</th>
                <th className="py-1 font-normal">head</th>
                <th className="py-1 font-normal">base</th>
                <th className="py-1 font-normal">state</th>
              </tr>
            </thead>
            <tbody>
              {outline(list).map(({ branch: b, depth }) => (
                <tr key={b.id} className="border-t border-brand-edge">
                  <td className="py-2" style={{ paddingLeft: depth * 18 }}>
                    <span className="font-mono text-xs text-brand-muted">
                      {depth > 0 ? "└ " : ""}
                    </span>
                    <Link
                      className="text-brand-primary hover:underline"
                      to={`/p/${project}/b/${b.name}`}
                    >
                      {b.name}
                    </Link>
                  </td>
                  <td className="py-2 font-mono text-xs">{b.head_lsn}</td>
                  <td className="py-2 font-mono text-xs text-brand-text-darker">
                    {b.base_lsn}
                  </td>
                  <td className="py-2">
                    <span className="flex items-center gap-2">
                      {b.state === "live" && (
                        <span className="flex items-center gap-1 font-mono text-[11px] text-brand-text-darker">
                          <Dot on /> live
                        </span>
                      )}
                      {capture.has(b.id) && (
                        <Badge>{`capture: ${capture.get(b.id)!.state}`}</Badge>
                      )}
                      {b.state === "live" && !capture.has(b.id) && (
                        <Badge>capture not running</Badge>
                      )}
                      {b.expires_at && (
                        <Badge tone="primary">{ttl(b.expires_at)}</Badge>
                      )}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {!readOnly && branches.data && (
          <NewBranch project={project} branches={list} />
        )}
      </Panel>

      {native ? (
        <Sandboxes project={project} branches={list} />
      ) : (
        <Panel title="Native database access">
          <NativeNotice />
        </Panel>
      )}
      <Pins project={project} branches={list} />
    </div>
  );
}
