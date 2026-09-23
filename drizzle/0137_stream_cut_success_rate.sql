-- 日期: 2026-09-23
-- 功能: 上游中途断流的请求在可用性投影里改判失败
--
-- 背景：v1.9.30 起，`upstream_truncated` 且**未见到协议终止标记**的流式请求会在结算时落
-- `error_message = 'upstream_stream_cut'`（见 go/internal/dataplane/streamErrorMessage），
-- 于是 message_request 侧与 usage_ledger.is_success 都记为失败（后者由 DB 触发器按
-- error_message 是否为空算）。
--
-- 为什么还要改本函数：可用性投影（avail_bucket_1m / avail_current）的成功率**不看
-- error_message**，而是走本函数，且它先按状态码判成功：
--     COALESCE(last_status_code, status_code) BETWEEN 200 AND 399 ⇒ 'success'
-- 而被切断的流上游确实回过 HTTP 200（正文发到一半才断），链上末条也记 200，故投影仍算成功
-- ⇒ 可用性页会把一个正在切断 1% 流的渠道显示成全健康。这正是用户困惑「渠道明明是好的」
-- 的机器侧成因。
--
-- 改法：在按状态码判成功之前，先按错误文案显式改判 failure。取 COALESCE(last_error_message,
-- error_message, '') 与文件内既有的 normalized_error 口径同源（两者都可能在链上或行上）。
-- 位置在排除类判据（404/499/resource_not_found/配额文案/has_matched_rule）之后，故
-- 「被排除」的语义仍然优先，不受本改动影响。
--
-- 为什么是改判而不是只依赖 is_success：投影与 is_success 是两个消费面，只改一处会让
-- 「账本说失败、可用性说成功」，正是本次要消掉的分叉。
CREATE OR REPLACE FUNCTION fn_compute_message_request_success_rate_outcome(
  blocked_by varchar,
  status_code integer,
  error_message text,
  provider_chain jsonb
)
RETURNS varchar AS $$
DECLARE
  last_reason text;
  last_status_code integer;
  last_error_message text;
  normalized_error text;
  has_matched_rule boolean := false;
BEGIN
  IF NOT fn_is_message_request_finalized(blocked_by, status_code, provider_chain, error_message) THEN
    RETURN NULL;
  END IF;
  IF blocked_by IS NOT NULL THEN
    RETURN 'excluded';
  END IF;
  IF provider_chain IS NOT NULL
     AND jsonb_typeof(provider_chain) = 'array'
     AND jsonb_array_length(provider_chain) > 0
     AND jsonb_typeof(provider_chain -> -1) = 'object' THEN
    last_reason := provider_chain -> -1 ->> 'reason';
    IF (provider_chain -> -1 ? 'statusCode')
       AND jsonb_typeof(provider_chain -> -1 -> 'statusCode') = 'number' THEN
      last_status_code := (provider_chain -> -1 ->> 'statusCode')::integer;
    END IF;
    last_error_message := provider_chain -> -1 ->> 'errorMessage';
    has_matched_rule := jsonb_typeof(provider_chain -> -1 -> 'errorDetails') = 'object'
      AND (provider_chain -> -1 -> 'errorDetails' ? 'matchedRule');
  END IF;
  IF has_matched_rule THEN
    RETURN 'excluded';
  END IF;
  IF COALESCE(last_status_code, status_code) = 404
     OR (COALESCE(last_status_code, status_code) = 499
         AND last_reason IS DISTINCT FROM 'client_abort_no_first_byte') THEN
    RETURN 'excluded';
  END IF;
  IF last_reason IN (
    'resource_not_found', 'concurrent_limit_failed', 'hedge_loser_cancelled',
    'hedge_loser_billed', 'client_error_non_retryable', 'client_abort'
  ) THEN
    RETURN 'excluded';
  END IF;
  normalized_error := lower(COALESCE(last_error_message, error_message, ''));
  IF normalized_error LIKE '%no available provider%'
     OR normalized_error LIKE '%insufficient quota%'
     OR normalized_error LIKE '%quota exceeded%'
     OR normalized_error LIKE '%rate limit%'
     OR normalized_error LIKE '%rate_limit%'
     OR normalized_error LIKE '%concurrency limit%'
     OR normalized_error LIKE '%concurrent limit%'
     OR normalized_error LIKE '%limit exceeded%' THEN
    RETURN 'excluded';
  END IF;
  -- 上游在正文中途断流：HTTP 是 200，但客户端拿到的是残流，必须算渠道侧失败。
  IF COALESCE(last_error_message, error_message, '') = 'upstream_stream_cut' THEN
    RETURN 'failure';
  END IF;
  IF last_reason IN ('request_success', 'retry_success', 'hedge_winner')
     OR COALESCE(last_status_code, status_code) BETWEEN 200 AND 399 THEN
    RETURN 'success';
  END IF;
  IF last_reason IN (
    'session_reuse', 'initial_selection', 'hedge_triggered', 'hedge_launched',
    'client_restriction_filtered', 'http2_fallback'
  ) AND last_status_code IS NULL
    AND COALESCE(last_error_message, error_message, '') = '' THEN
    RETURN NULL;
  END IF;
  RETURN 'failure';
END;
$$ LANGUAGE plpgsql IMMUTABLE;
