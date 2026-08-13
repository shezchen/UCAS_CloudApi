import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import RequestDetailGlobalPage from '@/features/requests/components/request-detail-global-page';

function ProtectedRequestDetailGlobal() {
  return (
    <RouteGuard requiredScopes={['read_requests']} scopeLevel="system">
      <RequestDetailGlobalPage />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/requests/$requestId')({
  component: ProtectedRequestDetailGlobal,
});
