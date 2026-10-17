import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { PrefsProvider } from "@/lib/prefs";
import { router } from "@/router";
import "@/styles.css";

if (new URLSearchParams(window.location.search).has("still")) {
  document.documentElement.dataset.still = "";
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <PrefsProvider>
      <RouterProvider router={router} />
    </PrefsProvider>
  </StrictMode>,
);
