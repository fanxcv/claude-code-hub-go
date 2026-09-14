CREATE TABLE "sensitive_words" (
	"id" serial PRIMARY KEY NOT NULL,
	"word" varchar(255) NOT NULL,
	"match_type" varchar(20) DEFAULT 'contains' NOT NULL,
	"description" text,
	"is_enabled" boolean DEFAULT true NOT NULL,
	"created_at" timestamp with time zone DEFAULT now(),
	"updated_at" timestamp with time zone DEFAULT now()
);
--> statement-breakpoint
ALTER TABLE "message_request" ADD COLUMN "blocked_by" varchar(50);--> statement-breakpoint
ALTER TABLE "message_request" ADD COLUMN "blocked_reason" text;--> statement-breakpoint
CREATE INDEX "idx_sensitive_words_enabled" ON "sensitive_words" USING btree ("is_enabled","match_type");--> statement-breakpoint
CREATE INDEX "idx_sensitive_words_created_at" ON "sensitive_words" USING btree ("created_at");