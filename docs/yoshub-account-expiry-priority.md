# Automatic account expiry priority

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

This controls the existing scheduler's priority input. Normal scheduling picks the smallest
available priority, then falls back when accounts are busy or unavailable. Existing sticky
sessions, previous-response bindings, model/group restrictions, and quota/health checks remain
in effect. The optional advanced weighted scheduler still treats priority as one score factor;
Yoshub production uses normal scheduling with the advanced and low-upstream-rate modes disabled.

Disable the environment flag and restart the application to stop automatic priority updates.
The last assigned priorities remain editable; old manual values are not automatically restored.
