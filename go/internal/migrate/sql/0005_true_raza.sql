ALTER TABLE "message_request" ADD COLUMN "original_model" varchar(128);--> statement-breakpoint
ALTER TABLE "message_request" ADD COLUMN "user_agent" varchar(512);--> statement-breakpoint
ALTER TABLE "message_request" ADD COLUMN "messages_count" integer;