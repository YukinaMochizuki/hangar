import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { Shell, Topbar } from "@/components/shell";
import { currentProject, machines, sizes, tenant, type SizeId } from "@/mock/data";
import { ArtSheet } from "@/pages/art-sheet";
import { LogoSheet } from "@/pages/logo-sheet";
import { CreateMachine, type CreateKind } from "@/pages/create-machine";
import { MachineDetail } from "@/pages/machine-detail";
import { MachineList } from "@/pages/machine-list";
import { Overview } from "@/pages/overview";

const rootRoute = createRootRoute({ component: () => <Outlet /> });

const consoleRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "console",
  component: () => (
    <Shell>
      <Outlet />
    </Shell>
  ),
});

/** A page this proof of concept has not drawn yet. */
function Later({ title }: { title: string }) {
  return (
    <>
      <Topbar crumbs={[tenant.name, currentProject.displayName, title]} />
      <div className="p-8 text-[13px] text-muted">{title}: drawn in the next round.</div>
    </>
  );
}

type DetailSearch = { simulate?: "boot" };
const detailSearch = (s: Record<string, unknown>): DetailSearch => (s.simulate === "boot" ? { simulate: "boot" } : {});

function detail(kind: "Container" | "VirtualMachine") {
  return function Detail() {
    const route = kind === "Container" ? containerRoute : vmRoute;
    const { name } = route.useParams();
    const { simulate } = route.useSearch();
    const m = machines.find((x) => x.kind === kind && x.name === name);
    return m ? <MachineDetail key={name} m={m} simulate={simulate === "boot"} /> : <Later title={name} />;
  };
}

const overviewRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/",
  validateSearch: (s: Record<string, unknown>): { banner?: "host" } => (s.banner === "host" ? { banner: "host" } : {}),
  component: function Home() {
    const { banner } = overviewRoute.useSearch();
    return <Overview banner={banner} />;
  },
});
const containersRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/containers",
  validateSearch: (s: Record<string, unknown>): { empty?: boolean } => (s.empty ? { empty: true } : {}),
  component: function Containers() {
    const { empty } = containersRoute.useSearch();
    return <MachineList kind="Container" empty={empty} />;
  },
});
const containerRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/containers/$name",
  validateSearch: detailSearch,
  component: detail("Container"),
});
const vmsRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/vms",
  component: () => <MachineList kind="VirtualMachine" />,
});
const vmRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/vms/$name",
  validateSearch: detailSearch,
  component: detail("VirtualMachine"),
});

const newRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/new",
  validateSearch: (s: Record<string, unknown>): { kind?: CreateKind; size?: SizeId } => ({
    ...(s.kind === "vm" || s.kind === "container" ? { kind: s.kind } : {}),
    ...(typeof s.size === "string" && s.size in sizes ? { size: s.size as SizeId } : {}),
  }),
  component: function New() {
    const { kind, size } = newRoute.useSearch();
    return <CreateMachine key={kind} kind={kind ?? "container"} size={size ?? "m"} />;
  },
});

const artRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poc/art", component: ArtSheet });
const logoRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poc/logo", component: LogoSheet });

const routeTree = rootRoute.addChildren([
  consoleRoute.addChildren([overviewRoute, containersRoute, containerRoute, vmsRoute, vmRoute, newRoute]),
  artRoute,
  logoRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
