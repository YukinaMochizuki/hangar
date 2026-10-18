// The console's pages. Every page is behind the gate (pages/gate.tsx); the
// project is in the path, so a link to a page names its project.

import { createRootRoute, createRoute, createRouter, Navigate, Outlet } from "@tanstack/react-router";
import { Shell } from "@/components/shell";
import { ErrorNotice, StandalonePage } from "@/components/states";
import { pickProject, useProjects } from "@/lib/projects";
import { CreateMachine, type CreateKind } from "@/pages/create-machine";
import { DiskDetail } from "@/pages/disk-detail";
import { DiskList } from "@/pages/disk-list";
import { Gate } from "@/pages/gate";
import { ImageDetail } from "@/pages/image-detail";
import { ImageList } from "@/pages/image-list";
import { MachineDetail } from "@/pages/machine-detail";
import { MachineList } from "@/pages/machine-list";
import { ProjectMembers, TenantMembers } from "@/pages/members";
import { Overview } from "@/pages/overview";
import { TenantOverview } from "@/pages/tenant-overview";
import { AddressDetail, AddressList } from "@/pages/addresses";
import { NetworkDetail, NetworkList } from "@/pages/networks";
import { PublicEndpointDetail, PublicEndpointList } from "@/pages/public-endpoints";
import { SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";

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
      <Shell key={project} place={{ kind: "project", name: project }}>
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
export const newRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/new",
  // size names a MachineSize; the form chooses the first size when it names
  // none the platform lists.
  validateSearch: (s: Record<string, unknown>): { kind?: CreateKind; size?: string } => ({
    ...(s.kind === "vm" || s.kind === "container" ? { kind: s.kind } : {}),
    ...(typeof s.size === "string" && s.size !== "" ? { size: s.size } : {}),
  }),
  component: function New() {
    const { kind, size } = newRoute.useSearch();
    return <CreateMachine key={kind} kind={kind ?? "container"} size={size} />;
  },
});

const disksRoute = createRoute({ getParentRoute: () => projectRoute, path: "/disks", component: DiskList });
const diskRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/disks/$name",
  component: function DiskPage() {
    const { name } = diskRoute.useParams();
    return <DiskDetail key={name} name={name} />;
  },
});
const imagesRoute = createRoute({ getParentRoute: () => projectRoute, path: "/images", component: ImageList });
const imageRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/images/$name",
  component: function ImagePage() {
    const { name } = imageRoute.useParams();
    return <ImageDetail key={name} name={name} />;
  },
});

// The network group: a list and an object's page for each kind.
const networksRoute = createRoute({ getParentRoute: () => projectRoute, path: "/networks", component: NetworkList });
const networkRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/networks/$name",
  component: function NetworkPage() {
    const { name } = networkRoute.useParams();
    return <NetworkDetail key={name} name={name} />;
  },
});
const securityGroupsRoute = createRoute({ getParentRoute: () => projectRoute, path: "/security-groups", component: SecurityGroupList });
const securityGroupRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/security-groups/$name",
  component: function SecurityGroupPage() {
    const { name } = securityGroupRoute.useParams();
    return <SecurityGroupDetail key={name} name={name} />;
  },
});
const addressesRoute = createRoute({ getParentRoute: () => projectRoute, path: "/addresses", component: AddressList });
const addressRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/addresses/$name",
  component: function AddressPage() {
    const { name } = addressRoute.useParams();
    return <AddressDetail key={name} name={name} />;
  },
});
const endpointsRoute = createRoute({ getParentRoute: () => projectRoute, path: "/public-endpoints", component: PublicEndpointList });
const endpointRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "/public-endpoints/$name",
  component: function PublicEndpointPage() {
    const { name } = endpointRoute.useParams();
    return <PublicEndpointDetail key={name} name={name} />;
  },
});
const networkRoutes = [
  networksRoute,
  networkRoute,
  securityGroupsRoute,
  securityGroupRoute,
  addressesRoute,
  addressRoute,
  endpointsRoute,
  endpointRoute,
];

const membersRoute = createRoute({ getParentRoute: () => projectRoute, path: "/members", component: ProjectMembers });

// A tenant's own pages, apart from any project's.
const tenantRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/tenants/$tenant",
  component: function Tenant() {
    const { tenant } = tenantRoute.useParams();
    return (
      <Shell key={tenant} place={{ kind: "tenant", name: tenant }}>
        <Outlet />
      </Shell>
    );
  },
});
const tenantOverviewRoute = createRoute({ getParentRoute: () => tenantRoute, path: "/", component: TenantOverview });
const tenantMembersRoute = createRoute({ getParentRoute: () => tenantRoute, path: "/members", component: TenantMembers });

const routeTree = rootRoute.addChildren([
  homeRoute,
  projectRoute.addChildren([
    overviewRoute,
    containersRoute,
    containerRoute,
    vmsRoute,
    vmRoute,
    newRoute,
    disksRoute,
    diskRoute,
    imagesRoute,
    imageRoute,
    membersRoute,
    ...networkRoutes,
  ]),
  tenantRoute.addChildren([tenantOverviewRoute, tenantMembersRoute]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
