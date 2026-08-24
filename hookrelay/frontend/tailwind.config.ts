import type { Config } from "tailwindcss";

// A small, deliberately restrained palette: one accent, one success, one danger,
// one warning, on a near-black canvas. Charts and badges pull from these tokens
// so the whole dashboard reads as one system.
const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}", "./lib/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        canvas: "#0a0b0e",
        surface: "#12141a",
        raised: "#191c24",
        border: "#252932",
        muted: "#8b93a7",
        ink: "#e8eaf0",
        accent: {
          DEFAULT: "#5b8cff",
          dim: "#2c447f",
        },
        ok: "#3ecf8e",
        warn: "#f5b13d",
        danger: "#f2544b",
        dead: "#b45cf0",
      },
      fontFamily: {
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
      boxShadow: {
        card: "0 1px 0 0 rgba(255,255,255,0.03) inset, 0 8px 24px -12px rgba(0,0,0,0.6)",
      },
    },
  },
  plugins: [],
};

export default config;
