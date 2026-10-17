import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { makeTransport, shouldRetry } from "@/lib/api";
import { applyInitialTheme, PrefsProvider } from "@/lib/prefs";
import { router } from "@/router";
import "@/styles.css";

// ?still stops every animation, for screenshots.
if (new URLSearchParams(window.location.search).has("still")) {
  document.documentElement.dataset.still = "";
}
applyInitialTheme();

const transport = makeTransport();
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 2000, refetchOnWindowFocus: true, retry: shouldRetry },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <PrefsProvider>
          <RouterProvider router={router} />
        </PrefsProvider>
      </QueryClientProvider>
    </TransportProvider>
  </StrictMode>,
);
