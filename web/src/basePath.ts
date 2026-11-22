// The path the console is served under, e.g. "/p/acme" behind a reverse
// proxy, or "" at the root. The engine writes it into the page as
// <base href>; without one (the Vite dev server) the console is at the root.
export const basePath: string = (() => {
  try {
    if (!document.querySelector("base[href]")) return "";
    return new URL(document.baseURI).pathname.replace(/\/+$/, "");
  } catch {
    return "";
  }
})();
