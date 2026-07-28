import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import ModelBenchmarksPage from '@/features/model-benchmarks';

function ProtectedModelBenchmarks() {
  return (
    <ProjectGuard>
      <RouteGuard>
        <ModelBenchmarksPage />
      </RouteGuard>
    </ProjectGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/model-benchmarks/')({
  component: ProtectedModelBenchmarks,
});
