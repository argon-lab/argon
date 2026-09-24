import { useEffect, useState } from "react";
const EVENTS = [
  "demo_entered",
  "first_diff",
  "first_merge",
  "first_undo",
  "quickstart_opened",
] as const;
type Event = (typeof EVENTS)[number];
const CHOICE = "argon.analytics";
export const QUICKSTART = "https://argonlabs.tech/quickstart";
export function track(event: Event) {
  try {
    if (
      !EVENTS.includes(event) ||
      navigator.doNotTrack === "1" ||
      localStorage.getItem(CHOICE) !== "yes"
    )
      return;
    const key = `argon.funnel.${event}`;
    if (sessionStorage.getItem(key)) return;
    sessionStorage.setItem(key, "1");
    void fetch("/api/v1/events", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ event }),
      keepalive: true,
      credentials: "omit",
    }).catch(() => {});
  } catch {
    /* Restricted browser storage must not break the console. */
  }
}
export function LocalLink({
  children = "Continue locally →",
}: {
  children?: React.ReactNode;
}) {
  return (
    <a
      className="text-brand-primary underline underline-offset-4"
      href={QUICKSTART}
      onClick={() => track("quickstart_opened")}
    >
      {children}
    </a>
  );
}
export function NativeNotice() {
  return (
    <p className="text-sm leading-6 text-brand-text-darker">
      The hosted demo supports sample-data reviews. Native MongoDB connections
      and physical sandboxes require your local engine. <LocalLink />
    </p>
  );
}
export function FunnelChoice() {
  const [enabled, setEnabled] = useState(false);
  useEffect(() => {
    try {
      setEnabled(localStorage.getItem(CHOICE) === "yes");
    } catch {}
  }, []);
  return (
    <label className="mt-8 flex items-start gap-2 text-xs leading-6 text-brand-text-darker">
      <input
        type="checkbox"
        className="mt-1.5"
        checked={enabled}
        onChange={(e) => {
          setEnabled(e.target.checked);
          try {
            localStorage.setItem(CHOICE, e.target.checked ? "yes" : "no");
          } catch {}
          if (e.target.checked) track("demo_entered");
        }}
      />
      Share anonymous completion counts (no documents or identifiers). Optional;
      honors Do Not Track.{" "}
      <a href="https://argonlabs.tech/privacy" className="text-brand-primary">
        Privacy
      </a>
    </label>
  );
}
