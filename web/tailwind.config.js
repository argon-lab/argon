/** @type {import('tailwindcss').Config} */
// Brand tokens shared with argon-website (noble-gas identity).
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        "brand-dark": "#0B0E14", // page background
        "brand-surface": "#111521", // panels
        "brand-edge": "#222838", // 1px borders
        "brand-muted": "#8F9BB3", // secondary labels
        "brand-primary": "#96A7FF", // argon glow
        "brand-secondary": "#6D7FE0", // deeper glow (hover / fills)
        "brand-text": "#E8EBF2",
        "brand-text-darker": "#9AA3B5",
      },
      fontFamily: {
        sans: ["Inter", "system-ui", "sans-serif"],
        mono: ["ui-monospace", "JetBrains Mono", "SFMono-Regular", "Menlo", "monospace"],
      },
    },
  },
  plugins: [],
};
