/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: ["class", '[data-mode="dark"]'],
  theme: {
    extend: {
      // The linen theme's --shed-* CSS vars, surfaced as Tailwind color tokens so
      // components use `bg-shed-bg` / `text-shed-text-muted` etc. (the vars live in
      // index.css and carry the light/dark values).
      colors: {
        shed: {
          bg: "var(--shed-bg)",
          "bg-sidebar": "var(--shed-bg-sidebar)",
          surface: "var(--shed-surface)",
          "surface-hover": "var(--shed-surface-hover)",
          inset: "var(--shed-inset)",
          border: "var(--shed-border)",
          "border-strong": "var(--shed-border-strong)",
          text: "var(--shed-text)",
          "text-secondary": "var(--shed-text-secondary)",
          "text-muted": "var(--shed-text-muted)",
          accent: "var(--shed-accent)",
          "accent-hover": "var(--shed-accent-hover)",
          "accent-fg": "var(--shed-accent-fg)",
          "accent-subtle": "var(--shed-accent-subtle)",
          "accent-border": "var(--shed-accent-border)",
          ok: "var(--shed-ok)",
          attention: "var(--shed-attention)",
          danger: "var(--shed-danger)",
          "deny-bg": "var(--shed-deny-bg)",
        },
      },
      borderRadius: { shed: "var(--shed-radius)" },
      boxShadow: { shed: "var(--shed-shadow)" },
      fontFamily: {
        sans: ["-apple-system", "BlinkMacSystemFont", '"SF Pro Text"', "system-ui", "sans-serif"],
        mono: ["ui-monospace", "SFMono-Regular", "monospace"],
      },
    },
  },
  plugins: [],
};
