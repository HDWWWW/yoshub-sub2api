export default {
  title: 'Risk detection', group: 'Monitored group', chooseGroup: 'Select an OpenAI group', enabled: 'Enable risk detection for this group',
  scope: 'Monitors schedulable OpenAI accounts in this group. Capacity = current concurrency / account concurrency limit. Monitoring continues on the server when this panel is closed.',
  multiTitle: 'Two or more accounts: capacity anomaly',
  multiHint: 'Uses the first two effective priorities, including expiry order, manual overrides and group reversal. Both conditions must remain true continuously. Equal priorities, missing samples or interrupted conditions reset the timer. Session affinity and model restrictions can also cause this pattern; this rule does not prove an upstream restriction.',
  firstThreshold: 'First account below (%)', secondThreshold: 'Second account at least (%)', duration: 'Sustain duration (seconds)',
  singleTitle: 'One account: scheduled test message',
  singleHint: 'Runs every 300 seconds by default. Checks only the visible text of a complete reply for a case-sensitive substring. Network errors, rate limits and incomplete replies do not count as content mismatches. Tests consume upstream quota and are not billed to downstream users.',
  interval: 'Test interval (seconds)', model: 'Model (custom supported)', effort: 'Reasoning effort', upstreamDefault: 'Upstream default', prompt: 'Test message', expected: 'Reply must contain',
  stopHint: 'Only the anomalous account is made unschedulable; active requests continue. There is no automatic recovery. Account scheduling is shared across its groups. Re-enable it manually in the account list.',
  status: 'Detection status', active: 'Enabled', disabled: 'Disabled', stale: 'No recent sample. Check service status.', first: 'First account', second: 'Second account',
  since: 'Anomaly since:', nextProbe: 'Next test:', history: 'Recent automatic stops', saved: 'Configuration saved', loadError: 'Could not load risk detection', saveError: 'Could not save risk detection'
}
