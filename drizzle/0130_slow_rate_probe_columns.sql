ALTER TABLE "providers" ADD COLUMN "slow_rate_probe_after_first_byte_seconds" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_probe_min_tokens" integer;