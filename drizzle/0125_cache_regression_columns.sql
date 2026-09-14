ALTER TABLE "message_request" ADD COLUMN "cache_regressed" boolean;--> statement-breakpoint
ALTER TABLE "message_request" ADD COLUMN "prev_cache_read_tokens" integer;