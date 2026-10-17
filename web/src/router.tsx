// The console's pages. Every page is behind the gate (pages/gate.tsx); the
// project is in the path, so a link to a page names its project.

import { createRootRoute, createRoute, createRouter, Navigate, Outlet } from "@tanstack/react-router";
import { Shell } from "@/components/shell";
import { ErrorNotice, StandalonePage } from "@/components/states";
import { sizes, type SizeId } from "@/lib/machines";
import { pickProject, useProjects } from "@/lib/projects";
import { CreateMachine, type CreateKind } from "@/pages/create-machine";
import { Gate } from "@/pages/gate";
import { MachineDetail } from "@/pages/machine-detail";
import { MachineList } from "@/pages/machine-list";
import { Overview } from "@/pages/overview";

const rootRoute = createRootRoute({
  component: () => (
    <Gate>
      <Outlet />
    </Gate>
  ),
  notFoundComponent: () => (
    <StandalonePage title="No such page">
      <p>Nothing in the console is at this address.</p>
    </StandalonePage>
  ),
});

/** "/" opens the last project used, or the first one. */
function Home() {
  const { projects, loading, error, refetch } = useProjects();
  if (loading) return null;
  if (error) {
    return (
      <StandalonePage title="The projects did not load">
        <ErrorNotice error={error} title="ListProjects failed" onRetry={() => void refetch()} className="p-0" />
      </StandalonePage>
    );
  }
  const p = pickProject(projects);
  if (!p?.metadata?.name) {
    return (
      <StandalonePage title="No projects">
        <p>This account can see no project yet.</p>
      </StandalonePage>
    );
  }
  return <Navigate to="/projects/$project" params={{ project: p.metadata.name }} replace />;
}

const homeRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: Home });

const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$project",
  component: function Project() {
    const { project } = projectRoute.useParams();
    return (
      <Shell key={project} project={project}>
        <Outlet />
      </Shell>
    );
  },
});

const overviewRoute = createRoute({ getParentRoute: () => projectRoute, path: "/", component: Overview });

const containersRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/containers",
  component: () => <MachineList key="Container" kind="Container" />,
});
const containerRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/containers/$name",
  component: function ContainerPage() {
    const { name } = containerRoute.useParams();
    return <MachineDetail key={name} kind="Container" name={name} />;
  },
});
const vmsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/vms",
  component: () => <MachineList key="VirtualMachine" kind="VirtualMachine" />,
});
const vmRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/vms/$name",
  component: function VMPage() {
    const { name } = vmRoute.useParams();
    return <MachineDetail key={name} kind="VirtualMachine" name={name} />;
  },
});
const newRoute = createRoute({
  getParentRoute: () => projectRoute,
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

const routeTree = rootRoute.addChildren([
  homeRoute,
  projectRoute.addChildren([overviewRoute, containersRoute, containerRoute, vmsRoute, vmRoute, newRoute]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
