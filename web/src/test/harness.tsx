// Renders console components against an in-memory API: each service is
// served by handlers in the test, and every call is recorded, in order, by
// method name and request, so a test can say which RPCs were sent and what
// they asked for.

import { createRouterTransport, type ConnectRouter } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { PlaceProvider, type Place } from "@/components/shell";

export type Call = { method: string; request: unknown };

export type Api = {
  transport: ReturnType<typeof createRouterTransport>;
  /** The methods called, in order. */
  calls: string[];
  /** The calls with their requests, in order. */
  requests: Call[];
  /** The query client the providers use, to make queries ask again. */
  client: QueryClient;
};

/**
 * An API whose routes routes() adds. Every call goes through an interceptor
 * that records it before the handler runs; methods no route serves answer
 * Unimplemented.
 */
export function fakeApi(routes: (router: ConnectRouter) => void): Api {
  const calls: string[] = [];
  const requests: Call[] = [];
  const transport = createRouterTransport(routes, {
    router: {
      interceptors: [
        (next) => async (req) => {
          calls.push(req.method.name);
          if (!req.stream) requests.push({ method: req.method.name, request: req.message });
          return next(req);
        },
      ],
    },
  });
  // No retries: a test's failure is the answer it set, at once.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return { transport, calls, requests, client };
}

/** The providers the console's pages need, over api, in the project named or the place given. */
export function withApi(api: Api, at: string | Place = "demo") {
  const place: Place = typeof at === "string" ? { kind: "project", name: at } : at;
  return function Providers({ children }: { children: ReactNode }) {
    return (
      <TransportProvider transport={api.transport}>
        <QueryClientProvider client={api.client}>
          <PlaceProvider value={place}>{children}</PlaceProvider>
        </QueryClientProvider>
      </TransportProvider>
    );
  };
}

/**
 * Gives jsdom's <dialog> the modal calls it lacks, so a page's dialogs open
 * and close in tests as they do in a browser.
 */
export function polyfillDialog() {
  const proto = HTMLDialogElement.prototype;
  proto.showModal ??= function (this: HTMLDialogElement) {
    this.open = true;
  };
  proto.close ??= function (this: HTMLDialogElement) {
    this.open = false;
    this.dispatchEvent(new Event("close"));
  };
}

/** A promise that never settles: a call still in flight. */
export function never<T>(): Promise<T> {
  return new Promise<T>(() => {});
}

/** A promise settled from outside, for a call the test answers when it chooses. */
export function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}
