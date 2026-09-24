import { useQuery } from "@tanstack/react-query";
import { api } from "./api";

export function useMeta() {
  return useQuery({ queryKey: ["meta"], queryFn: api.meta });
}

// Write actions disappear when the server says it is read-only — the
// hosted demo serves some projects look-but-don't-touch.
export function useReadOnly(): boolean {
  const meta = useMeta();
  return meta.data?.read_only ?? false;
}

export function useDemo(): boolean {
  const meta = useMeta();
  return meta.data?.demo ?? false;
}

// In demo mode the session cookie is the identity. App owns the expiry timer,
// 401 recovery and explicit renewal; never silently swap projects in the background.
export function useDemoSession() {
  const demo = useDemo();
  return useQuery({
    queryKey: ["demo-session"],
    queryFn: api.demoSession,
    enabled: demo,
    staleTime: Infinity,
    retry: 2,
  });
}

// Do not expose native actions before capability metadata arrives.
export function useNativeConnections(): boolean {
  const meta = useMeta();
  return (
    !!meta.data && !meta.data.demo && meta.data.native_connections !== false
  );
}
