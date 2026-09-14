CREATE TABLE "provider_schedule_logs" (
	"id" serial PRIMARY KEY NOT NULL,
	"execution_time" timestamp with time zone NOT NULL,
	"executed_by" varchar(50) NOT NULL,
	"dry_run" boolean DEFAULT false NOT NULL,
	"total_providers" integer NOT NULL,
	"analyzed_providers" integer NOT NULL,
	"affected_providers" integer NOT NULL,
	"decisions" jsonb NOT NULL,
	"summary" jsonb,
	"created_at" timestamp with time zone DEFAULT now()
);
--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "base_weight" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "base_priority" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "last_schedule_time" timestamp with time zone;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "enable_auto_schedule" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "schedule_time" varchar(5) DEFAULT '02:00';--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "min_sample_size" integer DEFAULT 10;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "schedule_window_hours" integer DEFAULT 24;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "enable_realtime_schedule" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "schedule_interval_seconds" integer DEFAULT 30;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "exploration_rate" integer DEFAULT 15;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "circuit_recovery_weight_percent" integer DEFAULT 30;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "circuit_recovery_observation_count" integer DEFAULT 10;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "max_weight_adjustment_percent" integer DEFAULT 10;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "short_term_window_minutes" integer DEFAULT 60;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "medium_term_window_minutes" integer DEFAULT 360;--> statement-breakpoint
ALTER TABLE "system_settings" ADD COLUMN "long_term_window_minutes" integer DEFAULT 1440;--> statement-breakpoint
CREATE INDEX "idx_schedule_logs_execution_time" ON "provider_schedule_logs" USING btree ("execution_time" DESC NULLS LAST);--> statement-breakpoint
CREATE INDEX "idx_schedule_logs_created_at" ON "provider_schedule_logs" USING btree ("created_at" DESC NULLS LAST);