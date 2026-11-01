# Automatic account expiry priority

Release: `0.2.19-yoshub.expiry-priority.3`, retaining the new-account 99% quota
defaults and filtered bulk concurrency controls.

Enable `gateway.scheduling.account_expiry_priority_enabled` or set
`GATEWAY_SCHEDULING_ACCOUNT_EXPIRY_PRIORITY_ENABLED=true` in the application environment.
The default is disabled; Yoshub production and local deployments enable it explicitly.

From `0.2.19-yoshub.native-compat.1`, expiry ordering is calculated on private
candidate copies for each scheduling decision. It never writes `accounts.priority`.
Native admin requests, responses and exports retain the exact configured priority.

- Accounts with an effective scheduling expiry in the future come first, earliest first.
- Identical expiry instants share a tier for normal load balancing.
- Undated accounts follow, retaining their relative manual priority.
- Expired accounts come last; auto-pause still obeys the per-account option.
- From `native-expiry.1`, imports store the actual deadline in native `expires_at`;
  supplier reclaim time remains separate. Unconverted legacy records still use
  internal grace. Only the importing key receives the original delivery timestamp
  through a compatibility view, which is never used by scheduling.

The normal and advanced OpenAI schedulers, legacy fallback, cached candidates and
mixed-platform gateway use this internal rank. Refreshes never mutate shared snapshots.
The expiry worker now only pauses accounts, using the same effective deadline as selection.
Manual priority changes are no longer overwritten, including during external delivery.
Disabling the expiry flag restores selection from the native priorities immediately
on application restart. No schema change or OAuth credential change is required.

Existing sticky/previous-response bindings, group/model eligibility and health/quota
checks retain their behavior. The Astra policy below treats the internal rank as a
strict tier; advanced scheduling for other models retains its scoring policy.

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

The account page now exposes a global "6a priority fill" switch at
`GET/PUT /api/v1/admin/accounts/astra-priority`. An explicitly saved `enabled`
value is persisted in settings and overrides the environment default. Switching
off restores the previous Astra selection policy for new scheduling decisions;
it does not rewrite account priorities or move active requests. The serving
instance applies changes immediately; other instances observe them within a
five-second cache interval. A temporary settings read failure preserves the last
known choice. With no saved value, the existing environment configuration remains
the default. No restart or schema migration is required for this switch.

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
