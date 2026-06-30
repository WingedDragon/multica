"use client";

import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { OctoBindPage } from "@multica/views/octo";

function OctoBindPageContent() {
  const searchParams = useSearchParams();
  return <OctoBindPage token={searchParams.get("token")} />;
}

export default function Page() {
  return (
    <Suspense fallback={null}>
      <OctoBindPageContent />
    </Suspense>
  );
}
