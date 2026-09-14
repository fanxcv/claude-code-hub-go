DROP TABLE "provider_schedule_logs" CASCADE;--> statement-breakpoint
ALTER TABLE "providers" DROP COLUMN "base_weight";--> statement-breakpoint
ALTER TABLE "providers" DROP COLUMN "base_priority";--> statement-breakpoint
ALTER TABLE "providers" DROP COLUMN "last_schedule_time";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "enable_auto_schedule";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "schedule_time";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "min_sample_size";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "schedule_window_hours";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "enable_realtime_schedule";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "schedule_interval_seconds";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "exploration_rate";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "circuit_recovery_weight_percent";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "circuit_recovery_observation_count";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "max_weight_adjustment_percent";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "short_term_window_minutes";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "medium_term_window_minutes";--> statement-breakpoint
ALTER TABLE "system_settings" DROP COLUMN "long_term_window_minutes";