import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import Playground from '@/features/playground';

function ProtectedPlayground() {
  return (
    <RouteGuard
      requiredScopes={['write_requests', 'read_channels']}
      scopeLevel="any"
      requireOwner={true}
    >
      <ProjectGuard>
        <Playground />
      </ProjectGuard>
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/playground/')({
  component: ProtectedPlayground,
});
