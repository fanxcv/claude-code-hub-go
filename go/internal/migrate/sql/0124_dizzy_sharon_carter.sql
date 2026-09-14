ALTER TABLE "providers" ADD COLUMN "circuit_breaker_release_increment" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "circuit_breaker_max_open_count" integer;