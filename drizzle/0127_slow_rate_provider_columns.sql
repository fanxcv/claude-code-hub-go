ALTER TABLE "providers" ADD COLUMN "slow_rate_monitor_enabled" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_window_seconds" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_min_samples" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_ratio_per_mille" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_penalty_step" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_penalty_max" integer;