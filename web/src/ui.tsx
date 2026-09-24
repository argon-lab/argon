// Small shared pieces of the spec-sheet look: 1px borders, mono labels,
// status dots, zero decoration.

import { ReactNode, useEffect, useState } from "react";

export function Panel({
  title,
  children,
  anchor,
}: {
  title?: string;
  children: ReactNode;
  anchor?: string; // named target for the guided tour to scroll to + highlight
}) {
  return (
    <section
      data-tour={anchor}
      className="border border-brand-edge bg-brand-surface transition-shadow"
    >
      {title && (
        <header className="border-b border-brand-edge px-4 py-2 font-mono text-xs uppercase tracking-widest text-brand-muted">
          {title}
        </header>
      )}
      <div className="p-4">{children}</div>
    </section>
  );
}

export function Tile({ size = 32 }: { size?: number }) {
  return (
    <span
      aria-label="Argon, element 18"
      className="relative inline-flex select-none items-center justify-center border border-brand-primary/60 bg-brand-primary/5 font-mono text-brand-primary"
      style={{ width: size, height: size }}
    >
      <span className="absolute left-1 top-0.5 text-[7px] text-brand-muted">
        18
      </span>
      <span className="text-sm leading-none">Ar</span>
    </span>
  );
}

export function Dot({ on }: { on: boolean }) {
  return (
    <span
      className={`inline-block h-1.5 w-1.5 rounded-full ${on ? "bg-brand-primary" : "bg-brand-edge"}`}
    />
  );
}

export function Badge({
  children,
  tone = "muted",
}: {
  children: ReactNode;
  tone?: "muted" | "primary";
}) {
  return (
    <span
      className={`border px-1.5 py-0.5 font-mono text-[11px] ${
        tone === "primary"
          ? "border-brand-primary/60 text-brand-primary"
          : "border-brand-edge text-brand-muted"
      }`}
    >
      {children}
    </span>
  );
}

export function Mono({ children }: { children: ReactNode }) {
  return <span className="font-mono text-brand-text-darker">{children}</span>;
}

export function Loading({ what }: { what: string }) {
  return <p className="font-mono text-xs text-brand-muted">loading {what}…</p>;
}

export function ErrorNote({ error }: { error: unknown }) {
  return (
    <p className="font-mono text-xs text-red-400">
      {error instanceof Error ? error.message : String(error)}
    </p>
  );
}

// Countdown renders "in 54m" and ticks once a minute.
export function Countdown({ to }: { to: string }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => tick((n) => n + 1), 30_000);
    return () => clearInterval(id);
  }, []);
  const ms = new Date(to).getTime() - Date.now();
  if (ms <= 0) return <span>now</span>;
  const min = Math.round(ms / 60_000);
  return <span>in {min >= 90 ? `${Math.round(min / 60)}h` : `${min}m`}</span>;
}

// Stable soft color per actor so timelines read at a glance.
const actorPalette = [
  "#96A7FF",
  "#7FD1AE",
  "#E8B87F",
  "#D18FD1",
  "#8FC7E8",
  "#E88F8F",
];
export function actorColor(actor: string): string {
  let h = 0;
  for (let i = 0; i < actor.length; i++)
    h = (h * 31 + actor.charCodeAt(i)) >>> 0;
  return actorPalette[h % actorPalette.length];
}

export function Json({ value }: { value: unknown }) {
  return (
    <pre className="overflow-x-auto border border-brand-edge bg-brand-dark p-2 font-mono text-[11px] leading-relaxed text-brand-text-darker">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}

// --- action primitives (hidden entirely in read-only mode) ---

export function Button({
  children,
  onClick,
  tone = "quiet",
  disabled,
  title,
}: {
  children: ReactNode;
  onClick: () => void;
  tone?: "quiet" | "solid" | "danger";
  disabled?: boolean;
  title?: string;
}) {
  const tones = {
    quiet:
      "border-brand-edge text-brand-text-darker hover:border-brand-primary hover:text-brand-primary",
    solid:
      "border-brand-primary bg-brand-primary text-brand-dark hover:bg-brand-secondary",
    danger: "border-red-400/50 text-red-400 hover:border-red-400",
  }[tone];
  return (
    <button
      className={`border px-3 py-1.5 font-mono text-xs disabled:cursor-not-allowed disabled:opacity-40 ${tones}`}
      onClick={onClick}
      disabled={disabled}
      title={title}
    >
      {children}
    </button>
  );
}

// Two-step inline confirmation — no native dialogs (they block the tab
// and read as foreign chrome). Click once to arm, again to fire.
export function ConfirmButton({
  children,
  onConfirm,
  tone = "danger",
  disabled,
}: {
  children: ReactNode;
  onConfirm: () => void;
  tone?: "quiet" | "solid" | "danger";
  disabled?: boolean;
}) {
  const [armed, setArmed] = useState(false);
  if (!armed) {
    return (
      <Button tone={tone} disabled={disabled} onClick={() => setArmed(true)}>
        {children}
      </Button>
    );
  }
  return (
    <span className="inline-flex items-center gap-2">
      <Button
        tone={tone}
        disabled={disabled}
        onClick={() => {
          setArmed(false);
          onConfirm();
        }}
      >
        confirm?
      </Button>
      <Button onClick={() => setArmed(false)}>cancel</Button>
    </span>
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  width = "w-44",
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  width?: string;
}) {
  return (
    <input
      className={`border border-brand-edge bg-brand-dark px-2 py-1.5 font-mono text-xs text-brand-text placeholder:text-brand-muted focus:border-brand-primary focus:outline-none ${width}`}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
    />
  );
}

export function Select({
  value,
  onChange,
  options,
}: {
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
}) {
  return (
    <select
      className="border border-brand-edge bg-brand-dark px-2 py-1.5 font-mono text-xs"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  );
}
