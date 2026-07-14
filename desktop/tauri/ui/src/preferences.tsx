import React from "react";
import ReactDOM from "react-dom/client";
import PreferencesWindow from "./PreferencesWindow";
import "./index.css";

// The Preferences window — a SEPARATE React root from the dashboard shell (the
// popover precedent), so it never mounts `useUiBridge` (which reports
// `pane`/`sheds` and would clobber the `main` snapshot). It reports its own
// grouped-form snapshot under the `preferences` window key (read by `prefs.dump`).
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <PreferencesWindow />
  </React.StrictMode>,
);
