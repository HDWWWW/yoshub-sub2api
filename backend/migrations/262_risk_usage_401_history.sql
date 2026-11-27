-- Allow the next eligible 401 to claim the group's first alert when the prior
-- release used that claim for a quota read. Keep the audit trail and counts.
-- Only match a known first event in the same group; unrelated incidents stay open.
WITH usage_events AS (
    SELECT rule.key AS group_key, event->>'id' AS event_id
    FROM settings s
    CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(s.value::jsonb->'rules') = 'object' THEN s.value::jsonb->'rules' ELSE '{}'::jsonb END) AS rule
    CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(rule.value->'events') = 'array' THEN rule.value->'events' ELSE '[]'::jsonb END) AS event
    WHERE s.key = 'account_risk_detection_v1'
      AND event->>'kind' = 'account_401'
      AND event->>'action' = 'observed'
      AND event->>'reason' IN ('用量探测收到上游 401', '查询账号用量收到上游 401')
)
UPDATE account_risk_401_incidents i
SET recovered_at = i.last_401_at
FROM usage_events e
WHERE i.group_id::text = e.group_key AND i.first_event_id = e.event_id
  AND (i.recovered_at IS NULL OR i.recovered_at < i.last_401_at);
