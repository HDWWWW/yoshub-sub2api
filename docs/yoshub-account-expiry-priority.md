# Automatic account expiry priority

Release: `0.2.19-yoshub.expiry-priority.3`, retaining the new-account 99% quota
defaults and filtered bulk concurrency controls.

Enable `gateway.scheduling.account_expiry_priority_enabled` or set
`GATEWAY_SCHEDULING_ACCOUNT_EXPIRY_PRIORITY_ENABLED=true` in the application environment.
The default is disabled; Yoshub production and local deployments enable it explicitly.

The account expiry worker runs at startup and every minute. It rewrites the visible
account priority using the account management **Expires at** field (`accounts.expires_at`):

- Unexpired accounts come first, from earliest expiry to latest, starting at priority 1.
- Identical expiry instants have identical priorities and keep normal load balancing.
- Accounts without an expiry follow dated accounts, retaining their relative priority order.
- Expired accounts are placed last; the existing per-account auto-pause option still decides
  whether to stop scheduling them. Deleted accounts are excluded.

Adding/importing accounts, changing/removing expiry, renewing an account, or the passage
of its expiry automatically triggers the appropriate ordering on the next minute's scan.
While enabled, manual priority edits are overwritten. OAuth access-token expiration and
quota-window reset times are not account expiration and are not used for this ordering.

Priority changes and scheduler cache notifications commit in one SQL statement. Only changed
accounts are written/notified. Concurrent expiry or priority edits are left for the next scan.
No schema migration or account credential change is required.

This controls the scheduler's priority input. For models without a dedicated policy, normal scheduling picks the smallest
available priority, then falls back when accounts are busy or unavailable. Existing sticky
sessions, previous-response bindings, model/group restrictions, and quota/health checks remain
in effect. The optional advanced weighted scheduler still treats priority as one score factor;
The GPT-6 Astra policy below makes numeric priority a strict ordering for that model.

Disable the environment flag and restart the application to stop automatic priority updates.
The last assigned priorities remain editable; old manual values are not automatically restored.

## GPT-6 Astra priority consumption

Available in `0.2.19-yoshub.astra-priority.1`, retaining the happy-first.3 behavior.
`gateway.scheduling.openai_astra_priority_enabled` defaults to `true`. Production and
local releases also set `GATEWAY_SCHEDULING_OPENAI_ASTRA_PRIORITY_ENABLED=true` explicitly.

For OpenAI requests using `gpt-6-astra`, the existing `gpt-6` alias, or recognized
provider-prefixed/dated Astra variants, selection tries lower numeric account priorities
first. With expiry ranking enabled, this consumes earlier-expiring accounts first.
The same selection engine applies whether the advanced scheduler is enabled or disabled.

- Model/group/capability, quota, health, privacy and profit eligibility checks still apply.
- An ordinary session binding or a higher load/cost score cannot select a later priority
  while an earlier tier has an eligible account with available concurrency.
- Within a priority tier, compact/subscription preferences, scoring and soft affinity apply.
  Top-K does not discard the other accounts in that tier or the later fallback tiers.
- Busy accounts and accounts rejected by a fresh check fall through to the next candidate.
  When all candidates are busy, the wait plan starts at the earliest eligible tier. Existing
  concurrency/DB probe budgets remain bounded; cached full accounts do not consume slot probes.
- Non-movable `previous_response_id` chains retain their owning account. Movable WebSocket
  requests use their full input to rebuild context if selection changes the owner. An already
  established WebSocket keeps its account until a new scheduling decision is needed.
- Guardian parent affinity is preserved. Other model families keep their existing policy.

Selection diagnostics use `schedule_layer=priority_first`; required previous-response and
guardian selections keep their original diagnostic layers. Advanced scheduler metrics count
these selections as load-balanced selections.

Set `GATEWAY_SCHEDULING_OPENAI_ASTRA_PRIORITY_ENABLED=false` and recreate only the application
to restore the previous Astra selection policy. This switch does not rewrite account priorities
or change the expiry worker. No schema migration is introduced.

## Account threshold override

New OpenAI accounts default to `credentials.account_scheduling_threshold = 99`.
Explicit imported values are preserved, including 100 (disable). Existing accounts
and explicit legacy per-window Extra settings are not rewritten. The default no
longer populates the separate temporary per-window pause settings.

An account override reaching its threshold turns `schedulable` off persistently.
Quota snapshot updates check this inside the same transaction; scheduler selection
also rechecks persisted usage under a row lock to reject stale snapshots. The
scheduling change and outbox notification commit together. Quota resets and usage
refreshes never turn scheduling back on; an administrator must do so manually.
Enabling while usage remains at the threshold stops it again on the next check.
No temporary-unschedulable deadline is created. An old threshold cooldown is
cleared when converted; unrelated error cooldowns remain intact.

Platform-default thresholds and model-specific Fable limits keep their existing
temporary behavior. Usage comes from upstream snapshots, so a threshold stops new
selections after the snapshot is observed; already running requests may finish.
