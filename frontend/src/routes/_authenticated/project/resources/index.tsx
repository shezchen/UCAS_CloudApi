import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import CampusResourcesPage from '@/features/campus-resources';

function ProtectedCampusResources() {
  return (
    <ProjectGuard>
      <RouteGuard>
        <CampusResourcesPage />
      </RouteGuard>
    </ProjectGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/resources/')({
  component: ProtectedCampusResources,
});
