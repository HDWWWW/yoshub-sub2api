export default {
  title: 'Auto Fast', group: 'Rule group', chooseGroup: 'Select an OpenAI group', enabled: 'Enable Auto Fast for this group',
  scope: 'Uses the supplier reclaim time, falling back to account expiry when no reclaim time is recorded. Checks every 10 seconds and keeps running after closing this panel.',
  stagesTitle: 'Fast by time stage',
  stagesHint: 'Order times from largest to smallest. The latest entered stage takes precedence: turn Fast on below its 7D usage threshold, and off at or above it. A 30-minute stage can turn Fast off after a 45-minute stage turned it on.',
  minutes: 'Minutes remaining at most', usedBelow: '7D used below (%)', addStage: 'Add stage', removeStage: 'Remove stage {index}',
  finalMinutes: 'Always force Fast in the final minutes', finalHint: 'Must be shorter than the last regular stage. Forces Fast regardless of 7D usage until reclaim or expiry.',
  maxAge: '7D data validity (seconds)', maxAgeHint: 'Regular stages preserve the current state when data is missing or stale and wait for fresh data. No test messages are sent.',
  restoreHint: 'An account can be managed by one rule only. Its Fast flag applies to all its groups. Rules may override manual changes while enabled. Disabling Auto Fast, leaving the group or leaving the time window restores the prior state.',
  invalid: 'Stage times must decrease, the final time must be smaller, usage must be 0–100%, and data validity must be 30–86400 seconds.',
  conflict: 'This group configuration changed on another page. Reload it before editing to avoid overwriting other changes.', reload: 'Reload configuration (discard unsaved edits)',
  saved: 'Saved; takes effect on the next check', status: 'Current status', active: 'Enabled', disabled: 'Disabled', total: 'Observing {count} accounts',
  checked: 'Last checked:', stale: 'Waiting for a check, or recent status is unavailable. Refresh shortly.', managed: 'Managed', unmanaged: 'Unmanaged',
  supplier: 'Supplier reclaim time', imported: 'Imported reclaim time', local: 'Account expiry', unknown: 'Missing or stale data', currentStage: 'Current stage: at most {minutes} minutes remaining',
  truncated: 'Showing the first 200 accounts; rules apply to all eligible accounts in the group.', history: 'Recent Fast changes', loadError: 'Failed to load Auto Fast', saveError: 'Failed to save Auto Fast'
}
