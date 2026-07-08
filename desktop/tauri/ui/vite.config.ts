import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

// Vite config for the Tauri frontend. `target: safari15` keeps esbuild from
// emitting JS newer than WebKitGTK ~2.38 (the shipped Linux WebView); the app is
// self-contained (a strict CSP forbids network requests — all data flows over the
// IPC socket / Tauri events).
export default defineConfig({
  plugins: [react()],
  clearScreen: false,
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: { port: 5173, strictPort: true },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "safari15",
    // Two entries: the dashboard shell (index.html) + the macOS menu-bar popover
    // (popover.html → TrayPopover). The popover is a SEPARATE webview so it never
    // mounts the shell's useUiBridge (which would clobber the `main` snapshot).
    rollupOptions: {
      input: {
        main: fileURLToPath(new URL("./index.html", import.meta.url)),
        popover: fileURLToPath(new URL("./popover.html", import.meta.url)),
      },
    },
  },
});
