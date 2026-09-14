"use client";

import { UiSessionGate } from "@/components/ui-session-gate";
import { SessionMessagesClient } from "./_components/session-messages-client";

export default function SessionMessagesPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard">
      <SessionMessagesClient />
    </UiSessionGate>
  );
}
