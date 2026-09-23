-- 日期: 2026-09-23
-- 功能: 协议正常收尾但生成未完成的请求不计入供应商可用性
-- response.incomplete、finish_reason=length、stop_reason=max_tokens、finishReason=MAX_TOKENS
-- 均由请求输出额度触发：有用量仍须结算，但不是成功，也不是供应商故障。
-- 仅复制 0137 的函数定义并在状态码成功判断之前加入精确 token 排除，保留断流失败判据。
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
  IF error_message = 'request_incomplete' THEN
    RETURN 'excluded';
  END IF;
  -- 0137 的断流判据不变：即使 HTTP 是 200，客户端得到残流仍是渠道故障。
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
