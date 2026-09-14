"use client";

import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { LogLevelForm } from "./_components/log-level-form";

export default function SettingsLogsPage() {
  const t = useTranslations("settings");

  return (
    <>
      <SettingsPageHeader
        title={t("logs.title")}
        description={t("logs.description")}
        icon="file-text"
      />

      <Section
        title={t("logs.section.title")}
        description={t("logs.section.description")}
        icon="file-text"
        variant="default"
      >
        <LogLevelForm />
      </Section>
    </>
  );
}
