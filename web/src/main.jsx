import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { NuqsAdapter, enableHistorySync } from "nuqs/adapters/react";
import { App } from "./App.jsx";
import "./style.css";

// The router pushes paths with the History API directly. Without this, nuqs
// would not notice a navigation that also changed the query string, and a
// link like /dashboard/activity?tool=grep would land on the page with its
// filters still showing the last page's.
enableHistorySync();

createRoot(document.getElementById("app")).render(
  <StrictMode>
    <NuqsAdapter>
      <App />
    </NuqsAdapter>
  </StrictMode>,
);
