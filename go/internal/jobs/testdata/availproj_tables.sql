\restrict QaSPeImD1TiZA6nF4EkKQJgXebXQfWjJVYmxampLz1WPotckmz6zS3wqu66rMca
CREATE TABLE public.avail_bucket_1m (
    provider_id integer NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    success_cnt integer DEFAULT 0 NOT NULL,
    failure_cnt integer DEFAULT 0 NOT NULL,
    excluded_cnt integer DEFAULT 0 NOT NULL,
    latency_cnt integer DEFAULT 0 NOT NULL,
    latency_sum_ms bigint DEFAULT 0 NOT NULL,
    last_request_at timestamp with time zone
);
CREATE TABLE public.avail_current (
    provider_id integer NOT NULL,
    state text DEFAULT 'unknown'::text NOT NULL,
    availability double precision DEFAULT 0 NOT NULL,
    request_count integer DEFAULT 0 NOT NULL,
    last_request_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.outbox_events (
    id bigint NOT NULL,
    event_id uuid DEFAULT gen_random_uuid() NOT NULL,
    event_type text NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id bigint NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    attempts integer DEFAULT 0 NOT NULL,
    last_error text
);
CREATE SEQUENCE public.outbox_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
ALTER SEQUENCE public.outbox_events_id_seq OWNED BY public.outbox_events.id;
CREATE TABLE public.proj_applied_requests (
    request_id bigint NOT NULL,
    event_id uuid NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);
ALTER TABLE ONLY public.outbox_events ALTER COLUMN id SET DEFAULT nextval('public.outbox_events_id_seq'::regclass);
ALTER TABLE ONLY public.avail_bucket_1m
    ADD CONSTRAINT avail_bucket_1m_pkey PRIMARY KEY (provider_id, bucket_start);
ALTER TABLE ONLY public.avail_current
    ADD CONSTRAINT avail_current_pkey PRIMARY KEY (provider_id);
ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_event_id_key UNIQUE (event_id);
ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.proj_applied_requests
    ADD CONSTRAINT proj_applied_requests_pkey PRIMARY KEY (request_id);
CREATE INDEX idx_avail_bucket_1m_time ON public.avail_bucket_1m USING btree (bucket_start DESC);
CREATE INDEX idx_outbox_events_unpublished ON public.outbox_events USING btree (id) WHERE (published_at IS NULL);
\unrestrict QaSPeImD1TiZA6nF4EkKQJgXebXQfWjJVYmxampLz1WPotckmz6zS3wqu66rMca
