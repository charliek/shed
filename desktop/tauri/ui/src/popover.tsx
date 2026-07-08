import React from "react";
import ReactDOM from "react-dom/client";
import TrayPopover from "./TrayPopover";
import "./index.css";

// The popover's native macOS vibrancy material follows the OS appearance, so its
// TOKENS must too — otherwise dark text lands on a dark-frosted material (unreadable)
// on a dark-mode Mac. The dashboard uses a manual toggle in its header chrome; the
// popover has no chrome for one, so it follows `prefers-color-scheme` to stay in step
// with its material. (Set before render; the window mounts hidden, so no visible flash.)
const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");
const applyMode = () => {
  document.documentElement.dataset.mode = darkQuery.matches ? "dark" : "light";
};
applyMode();
darkQuery.addEventListener("change", applyMode);

// The macOS menu-bar popover — a SEPARATE React root from the dashboard shell, so
// it never mounts `useUiBridge` (which reports `pane`/`sheds` and would clobber the
// `main` snapshot). It reports its own compact rows under the `popover` window key.
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <TrayPopover />
  </React.StrictMode>,
);
