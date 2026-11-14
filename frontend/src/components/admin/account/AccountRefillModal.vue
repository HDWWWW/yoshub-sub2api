<template>
  <BaseDialog :show="show" :title="t('accountRefill.title')" width="extra-wide" :close-on-escape="!busy" :show-close-button="!busy" @close="close">
    <div v-if="loading" class="py-12 text-center" role="status">{{ t('common.loading') }}</div>
    <div v-else-if="view && form" class="space-y-5">
      <label class="block"><span class="input-label">{{ t('accountRefill.ruleGroup') }}</span>
        <select :value="selectedGroup" class="input" data-testid="refill-rule-group" :disabled="busy || dirty || fenduduDirty || !!manualPending" @change="switchGroup(Number(($event.target as HTMLSelectElement).value))">
          <option :value="0" disabled>{{ t('accountRefill.chooseGroup') }}</option>
          <option v-for="group in ruleGroups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }}) · {{ t(view.rules?.find(r => r.group_id === group.id)?.enabled ? 'accountRefill.ruleOn' : 'accountRefill.ruleOff') }}</option>
        </select>
        <span class="input-hint">{{ t(dirty ? 'accountRefill.groupSaveFirst' : 'accountRefill.groupRulesHint') }}</span>
      </label>
      <div v-if="!isFendudu" class="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <div v-for="item in walletItems" :key="item.key" class="rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-700">
          <div class="text-xs text-gray-500 dark:text-dark-300">{{ t(`accountRefill.${item.key}`) }}</div>
          <div class="mt-1 text-2xl font-semibold tabular-nums text-gray-900 dark:text-white">{{ number(item.value) }} <span class="text-xs font-normal text-gray-500">{{ t('accountRefill.points') }}</span></div>
        </div>
      </div>
      <p v-if="!isFendudu" class="text-xs text-gray-500">{{ t('accountRefill.snapshot') }} {{ date(view.identity_at) }}</p>
      <div class="rounded-lg bg-gray-50 px-4 py-3 text-sm dark:bg-dark-800" aria-live="polite">{{ view.message || t('accountRefill.unconfigured') }}</div>
      <nav ref="tabsNav" class="flex gap-1 overflow-x-auto border-b border-gray-200 dark:border-dark-700" :aria-label="t('accountRefill.title')">
        <button v-for="tab in tabs" :key="tab" type="button" class="whitespace-nowrap border-b-2 px-4 py-3 text-sm" :class="activeTab === tab ? 'border-primary-500 text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500'" :aria-pressed="activeTab === tab" :disabled="busy" @click="activeTab = tab">{{ t(`accountRefill.${tab}`) }}</button>
      </nav>
      <form v-admin-read-only-form id="refill-settings" class="space-y-5" @submit.prevent="save">
        <fieldset :disabled="busy" class="space-y-5">
          <div v-show="activeTab === 'connection'" class="space-y-5">
            <label class="block"><span class="input-label">{{ t('accountRefill.token') }} · {{ t(view.token_configured ? 'accountRefill.configured' : 'accountRefill.unconfigured') }}</span>
              <input v-model="token" type="password" autocomplete="new-password" class="input" placeholder="rst_…" data-testid="refill-token" />
              <span class="input-hint">{{ t('accountRefill.tokenHint') }}</span>
            </label>
            <dl class="grid gap-4 rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800 sm:grid-cols-2">
              <div><dt class="text-gray-500">{{ t('accountRefill.keyLabel') }}</dt><dd class="mt-1">{{ view.key ? `${view.key.label} / ${view.key.scope}` : t('accountRefill.unknown') }}</dd></div>
              <div><dt class="text-gray-500">{{ t('accountRefill.keyExpiry') }}</dt><dd class="mt-1">{{ date(view.key?.expires_at) }} <span :class="keyDays !== null && keyDays < 7 ? 'text-amber-600' : 'text-gray-500'">{{ keyRemaining }}</span></dd></div>
            </dl>
            <div><p class="input-label">{{ t('accountRefill.deliveryGroups') }}</p><div class="grid gap-3 sm:grid-cols-2"><label v-for="group in openAIGroups" :key="group.id" class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-700"><input v-model="form.delivery_group_ids" type="checkbox" :value="group.id" :disabled="group.id === form.group_id" />{{ group.name }} (#{{ group.id }})</label></div><p class="input-hint">{{ t('accountRefill.groupsHint') }}</p></div>
            <p class="rounded-lg bg-primary-50 p-4 text-sm leading-6 text-primary-800 dark:bg-primary-900/20 dark:text-primary-200">{{ t('accountRefill.importHint', { minutes: form.import_expiry_grace_minutes }) }}</p>
          </div>
          <div v-show="activeTab === 'rules'" class="space-y-5">
            <div class="flex flex-wrap gap-6"><label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" data-testid="refill-enabled" />{{ t('accountRefill.enabled') }}</label><label class="flex items-center gap-2 text-sm"><input v-model="form.monitor_enabled" type="checkbox" />{{ t('accountRefill.monitor_enabled') }}</label></div>
            <p class="text-sm leading-6 text-amber-700 dark:text-amber-300">{{ t('accountRefill.enableHint') }}</p>
            <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <label class="flex items-center gap-2 text-sm font-medium"><input v-model="form.immediate_failure_retry" type="checkbox" data-testid="refill-immediate-failure-retry" />{{ t('accountRefill.immediateFailureRetry') }}</label>
              <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('accountRefill.immediateFailureRetryHint') }}</p>
            </div>
            <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <label class="flex items-center gap-2 text-sm font-medium"><input v-model="form.timeout_retry_enabled" type="checkbox" data-testid="refill-timeout-retry-enabled" />{{ t('accountRefill.timeoutRetryEnabled') }}</label>
              <label class="block sm:max-w-xs"><span class="input-label">{{ t('accountRefill.timeoutRetryMinutes') }}</span><input v-model.number="form.timeout_retry_minutes" type="number" min="1" max="1440" step="1" required class="input" data-testid="refill-timeout-retry-minutes" /></label>
              <label class="flex items-center gap-2 text-sm"><input v-model="form.timeout_retry_notify" type="checkbox" data-testid="refill-timeout-retry-notify" />{{ t('accountRefill.timeoutRetryNotify') }}</label>
              <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('accountRefill.timeoutRetryNotifyHint') }}</p>
              <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('accountRefill.timeoutRetryHint') }}</p>
            </div>
            <div class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <div class="flex flex-wrap items-center justify-between gap-3"><span class="text-sm font-medium">{{ t('accountRefill.dailyTasks') }}</span><button type="button" class="btn btn-secondary" :disabled="busy || (form.daily_schedules?.length || 0) >= 20" data-testid="refill-daily-add" @click="addDailySchedule">{{ t('accountRefill.dailyAdd') }}</button></div>
              <div v-for="(daily, index) in form.daily_schedules" :key="daily.id" class="space-y-3 border-t border-gray-100 pt-3 dark:border-dark-700" data-testid="refill-daily-row">
                <div class="flex items-center justify-between gap-3"><label class="flex items-center gap-2 text-sm"><input v-model="daily.enabled" type="checkbox" data-testid="refill-daily-enabled" />{{ t('accountRefill.dailyTask', { number: index + 1 }) }}</label><button type="button" class="text-sm text-red-600" :disabled="busy" data-testid="refill-daily-remove" @click="form.daily_schedules?.splice(index, 1)">{{ t('common.delete') }}</button></div>
                <label class="block"><span class="input-label">{{ t('accountRefill.dailyDayMode') }}</span><select v-model="daily.day_mode" class="input" data-testid="refill-daily-day-mode"><option value="">{{ t('accountRefill.dailyEveryDay') }}</option><option value="china_workday">{{ t('accountRefill.dailyChinaWorkday') }}</option></select></label>
                <p v-if="daily.day_mode === 'china_workday'" class="input-hint">{{ t('accountRefill.dailyWorkdayHint') }}</p>
                <div class="grid gap-3 sm:grid-cols-3">
                  <label><span class="input-label">{{ t('accountRefill.dailyTime') }}</span><input v-model="daily.time" type="time" required class="input" data-testid="refill-daily-time" /></label>
                  <label><span class="input-label">{{ t('accountRefill.plan') }}</span><select v-model="daily.plan" class="input" data-testid="refill-daily-plan"><option value="">{{ t('accountRefill.dailyFollowPlan') }}</option><option value="pro5x">Pro5x</option><option value="pro20x">Pro20x</option></select></label>
                  <label><span class="input-label">{{ t('accountRefill.dailyQuantity') }}</span><input v-model.number="daily.quantity" type="number" min="1" max="100" step="1" required class="input" data-testid="refill-daily-quantity" /></label>
                </div>
                <div v-if="dailyStatus(daily.id)" class="space-y-1 text-xs text-gray-500" data-testid="refill-daily-status"><p>{{ t('accountRefill.dailyNext') }}: {{ date(dailyStatus(daily.id)?.next_at) }}</p><p>{{ dailyStatus(daily.id)?.message }}</p></div>
              </div>
              <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('accountRefill.dailyHint') }}</p>
              <div v-if="form.daily_schedules?.some(daily => daily.day_mode === 'china_workday')" class="space-y-1 text-xs text-gray-500 dark:text-gray-400" data-testid="refill-workday-calendar">
                <template v-if="view.workday_calendar">
                  <p>{{ t('accountRefill.calendarYears') }}: {{ view.workday_calendar.years.join('、') || '—' }} · {{ t('accountRefill.calendarSyncedAt') }}: {{ date(view.workday_calendar.synced_at) }}</p>
                  <p>{{ view.workday_calendar.message }}</p>
                </template>
                <p v-else>{{ t('accountRefill.calendarPending') }}</p>
              </div>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <label><span class="input-label">{{ t('accountRefill.mode') }}</span><select v-model="form.mode" class="input"><option v-for="mode in ['capacity', 'fixed', 'interval']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label v-if="!form.rotation_enabled"><span class="input-label">{{ t('accountRefill.plan') }}</span><select v-model="form.plan" class="input"><option value="pro5x">Pro5x</option><option value="pro20x">Pro20x</option></select></label>
            </div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.rotation_enabled" type="checkbox" data-testid="refill-rotation-enabled" @change="initializeRotation" />{{ t('accountRefill.rotationEnabled') }}</label>
            <div v-if="form.rotation_enabled" class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <p class="text-sm text-gray-500">{{ t('accountRefill.rotationHint') }}</p>
              <div v-for="(step, index) in form.rotation" :key="index" class="flex flex-wrap items-end gap-3" data-testid="refill-rotation-step">
                <span class="pb-2 text-sm tabular-nums">{{ index + 1 }}.</span>
                <label class="min-w-32 flex-1"><span class="input-label">{{ t('accountRefill.plan') }}</span><select v-model="step.plan" class="input" :data-testid="`refill-rotation-plan-${index}`"><option value="pro20x">Pro20x</option><option value="pro5x">Pro5x</option></select></label>
                <label class="w-28"><span class="input-label">{{ t('accountRefill.rotationCount') }}</span><input v-model.number="step.count" class="input" type="number" min="1" max="100" step="1" :data-testid="`refill-rotation-count-${index}`" /></label>
                <button type="button" class="btn btn-secondary" :disabled="index === 0" :aria-label="t('accountRefill.rotationUp')" @click="moveRotation(index)">↑</button>
                <button type="button" class="btn btn-secondary" :disabled="form.rotation.length <= 1" @click="form.rotation.splice(index, 1)">{{ t('accountRefill.rotationRemove') }}</button>
              </div>
              <button type="button" class="btn btn-secondary" data-testid="refill-rotation-add" :disabled="form.rotation.length >= 20" @click="form.rotation.push({ plan: 'pro5x', count: 1 })">{{ t('accountRefill.rotationAdd') }}</button>
              <p class="text-sm font-medium" data-testid="refill-rotation-preview">{{ rotationPreview }} → {{ t('accountRefill.rotationRepeat') }}</p>
              <p v-if="view.config.rotation_enabled" class="text-sm text-gray-500">{{ t('accountRefill.rotationNext') }}: {{ view.next_plan }} · {{ t('accountRefill.rotationPosition', { position: view.rotation_cursor + 1 }) }}</p>
            </div>
            <div class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800"><p>{{ t('accountRefill.quote') }}: <strong>{{ number(quote?.price) }}</strong> {{ t('accountRefill.points') }}</p><p v-if="quote" class="mt-2 text-gray-500">{{ t('accountRefill.terms') }}: {{ quote.terms }}</p></div>
            <p class="text-sm leading-6 text-gray-500">{{ t(form.mode === 'interval' ? 'accountRefill.intervalHint' : form.mode === 'fixed' ? 'accountRefill.fixedHint' : 'accountRefill.ruleHint') }}</p>
            <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><label v-for="field in ruleFields" :key="field.key"><span class="input-label">{{ t(`accountRefill.${field.key}`) }}</span><input v-model.number="form[field.key]" type="number" :min="field.min" :max="field.max" :step="field.step || 1" class="input" :data-testid="`refill-${field.key}`" /></label></div>
            <p v-if="form.mode === 'capacity'" class="input-hint">{{ t('accountRefill.capacityObservationHint') }}</p>
            <div v-if="form.mode === 'capacity'" class="rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <label class="flex items-center gap-2 text-sm font-medium"><input v-model="form.forecast_enabled" type="checkbox" data-testid="refill-forecast-enabled" />{{ t('accountRefill.forecastEnabled') }}</label>
              <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('accountRefill.forecastEnabledHint') }}</p>
            </div>
            <div v-if="form.mode !== 'interval'" class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
              <p class="text-sm font-medium">{{ t('accountRefill.planQuotaTitle') }}</p>
              <p class="input-hint">{{ t('accountRefill.planQuotaHint') }}</p>
              <div v-for="plan in ['pro5x', 'pro20x']" :key="plan" class="space-y-3">
                <label class="flex items-center gap-2 text-sm"><input type="checkbox" :checked="!!form.plan_quota_thresholds?.[plan]" :data-testid="`refill-quota-${plan}-enabled`" @change="togglePlanQuota(plan)" />{{ plan === 'pro5x' ? 'Pro5x' : 'Pro20x' }} · {{ t('accountRefill.planQuotaOverride') }}</label>
                <div v-if="form.plan_quota_thresholds?.[plan]" class="grid gap-3 sm:grid-cols-2">
                  <label><span class="input-label">{{ t('accountRefill.min_remaining_5h') }}</span><input v-model.number="form.plan_quota_thresholds[plan]!.min_remaining_5h" type="number" min="0" max="100" step="0.1" class="input" :data-testid="`refill-quota-${plan}-5h`" /></label>
                  <label><span class="input-label">{{ t('accountRefill.min_remaining_7d') }}</span><input v-model.number="form.plan_quota_thresholds[plan]!.min_remaining_7d" type="number" min="0" max="100" step="0.1" class="input" :data-testid="`refill-quota-${plan}-7d`" /></label>
                </div>
              </div>
            </div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.window_enabled" type="checkbox" />{{ t('accountRefill.window_enabled') }}</label>
            <div v-if="form.window_enabled" class="grid gap-4 sm:grid-cols-2"><label><span class="input-label">{{ t('accountRefill.window_start') }}</span><input v-model="form.window_start" type="time" class="input" /></label><label><span class="input-label">{{ t('accountRefill.window_end') }}</span><input v-model="form.window_end" type="time" class="input" /></label></div>
            <p class="input-hint">{{ t('accountRefill.windowHint') }}</p>
          </div>
          <div v-show="activeTab === 'delivery'" class="space-y-5">
            <label class="block"><span class="input-label">{{ t('admin.accounts.poolModeRetryCount') }}</span><input v-model.number="form.delivery.same_account_retry_count" data-testid="refill-same-account-retries" type="number" min="0" :max="MAX_SAME_ACCOUNT_RETRY_COUNT" step="1" required class="input" /><p class="input-hint">{{ t('admin.accounts.sameAccountRetryCountHint', { default: DEFAULT_SAME_ACCOUNT_RETRY_COUNT, max: MAX_SAME_ACCOUNT_RETRY_COUNT }) }}</p></label>
            <OpenAIPrivacyControl :model-value="form.delivery.privacy_enabled !== false" @update:model-value="form.delivery.privacy_enabled = $event" />
            <div class="flex items-center justify-between gap-4">
              <div>
                <p id="refill-flatten-namespaces-label" class="input-label mb-0">{{ t('admin.accounts.openai.flattenNamespaces') }}</p>
                <p id="refill-flatten-namespaces-description" class="input-hint">{{ t('admin.accounts.openai.flattenNamespacesDesc') }}</p>
              </div>
              <Toggle v-admin-read-only-control
                :model-value="form.delivery.openai_responses_flatten_namespaces === true"
                data-testid="refill-flatten-namespaces-toggle"
                aria-labelledby="refill-flatten-namespaces-label"
                aria-describedby="refill-flatten-namespaces-description"
                @update:model-value="form.delivery.openai_responses_flatten_namespaces = $event"
              />
            </div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.push_enabled" type="checkbox" />{{ t('accountRefill.push_enabled') }}</label><p class="input-hint">{{ t('accountRefill.pushHint') }}</p>
            <div class="grid gap-4 sm:grid-cols-2">
              <label><span class="input-label">{{ t('accountRefill.ws_mode') }}</span><select v-model="form.delivery.ws_mode" class="input"><option v-for="mode in ['off', 'ctx_pool', 'http_bridge', 'passthrough']" :key="mode" :value="mode">{{ t(`accountRefill.${mode}`) }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.fingerprint_mode') }}</span><select v-model="form.delivery.fingerprint_mode" class="input" data-testid="refill-fingerprint-mode"><option v-for="option in codexFingerprintModeOptions" :key="option.value" :value="option.value">{{ option.label }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.proxy_mode') }}</span><select v-model="form.delivery.proxy_mode" class="input" @change="form.delivery.proxy_id = null"><option v-for="mode in ['direct', 'specified', 'random', 'ovh_ipv6']" :key="mode" :value="mode">{{ t(mode === 'ovh_ipv6' ? 'ovhIPv6.mode.ovh_ipv6' : `accountRefill.${mode}`) }}</option></select></label>
              <div v-if="form.delivery.proxy_mode === 'specified'"><span class="input-label">{{ t('accountRefill.proxy_id') }}</span><ProxySelector :model-value="form.delivery.proxy_id" :proxies="manualProxies" :disabled="busy" @update:model-value="form.delivery.proxy_id = $event; if ($event === null) form.delivery.proxy_mode = 'direct'" /><p class="input-hint">{{ t('ovhIPv6.refillSpecifiedHint') }}</p></div>
              <label v-for="plan in ['pro5x', 'pro20x']" :key="plan"><span class="input-label">{{ plan === 'pro5x' ? 'Pro5x' : 'Pro20x' }} · {{ t('accountRefill.import_concurrency') }}</span><input v-if="form.plan_import_concurrency" v-model.number="form.plan_import_concurrency[plan]" type="number" min="1" max="10000" step="1" class="input" :data-testid="`refill-concurrency-${plan}`" /></label>
              <label v-for="field in deliveryFields" :key="field.key"><span class="input-label">{{ t(`accountRefill.${field.key}`) }}</span><input v-model.number="form.delivery[field.key]" type="number" min="0" :max="field.max" :step="field.step" class="input" /></label>
              <label><span class="input-label">{{ t('accountRefill.status') }}</span><select v-model="form.delivery.status" class="input"><option value="active">{{ t('accountRefill.active') }}</option><option value="inactive">{{ t('accountRefill.inactive') }}</option></select></label>
              <label><span class="input-label">{{ t('accountRefill.expiry_mode') }}</span><select v-model="form.delivery.expiry_mode" class="input"><option value="rental">{{ t('accountRefill.rental') }}</option><option value="fixed">{{ t('accountRefill.fixedExpiry') }}</option></select></label>
              <label v-if="form.delivery.expiry_mode === 'fixed'"><span class="input-label">{{ t('accountRefill.fixed_expires_at') }}</span><input v-model="fixedExpiry" type="datetime-local" class="input" /></label>
            </div>
            <div class="flex flex-wrap gap-6"><label class="flex items-center gap-2 text-sm"><input v-model="form.delivery.schedulable" type="checkbox" />{{ t('accountRefill.schedulable') }}</label><label class="flex items-center gap-2 text-sm"><input v-model="form.delivery.auto_pause_on_expired" type="checkbox" />{{ t('accountRefill.auto_pause_on_expired') }}</label></div>
            <label class="block"><span class="input-label">{{ t('accountRefill.import_expiry_grace_minutes') }}</span><input v-model.number="form.import_expiry_grace_minutes" type="number" min="0" max="1440" step="1" class="input" data-testid="refill-import-grace" /></label>
            <p class="input-hint">{{ t('accountRefill.expiryHint') }}</p>
            <label class="block"><span class="input-label">{{ t('accountRefill.name_template') }}</span><input v-model="form.delivery.name_template" class="input" maxlength="256" /></label>
            <label class="block"><span class="input-label">{{ t('accountRefill.notes_template') }}</span><textarea v-model="form.delivery.notes_template" class="input" rows="2" maxlength="2000"></textarea></label>
            <p class="input-hint">{{ t('accountRefill.templateHint') }} <code v-pre>{name} {email} {id} {plan_type} {push_time} {expire_time}</code></p>
            <label class="block"><span class="input-label">{{ t('accountRefill.model_mapping') }}</span><textarea v-model="mapping" class="input font-mono text-sm" rows="4" spellcheck="false" placeholder='{"model-a": "model-b"}'></textarea></label>
          </div>
          <div v-show="activeTab === 'notifications'" class="space-y-5">
            <p class="text-sm leading-6 text-gray-500">{{ t('accountRefill.notificationsHint') }}</p>
            <label class="block"><span class="input-label">{{ t('accountRefill.wecomWebhook') }} <span v-if="view.webhook_configured" class="text-green-600">{{ t('accountRefill.configured') }}</span></span><input v-model="webhook" type="password" autocomplete="new-password" class="input" data-testid="refill-webhook" :placeholder="t('accountRefill.webhookPlaceholder')" /></label>
            <p class="input-hint">{{ t('accountRefill.webhookHint') }}</p>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.notifications.refill_started" type="checkbox" data-testid="refill-notify-start" />{{ t('accountRefill.notifyStarted') }}</label>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('accountRefill.notifyStartedHint') }}</p>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.notifications.refill_needed" type="checkbox" data-testid="refill-notify-needed" />{{ t('accountRefill.notifyNeeded') }}</label>
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('accountRefill.notifyNeededHint') }}</p>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.notifications.low_balance" type="checkbox" data-testid="refill-notify-balance" />{{ t('accountRefill.notifyLowBalance') }}</label>
            <label class="block"><span class="input-label">{{ t('accountRefill.lowBalanceThreshold') }}</span><input v-model.number="form.notifications.low_balance_threshold" type="number" min="0" max="10000000" step="0.01" class="input" data-testid="refill-balance-threshold" /></label>
            <p class="input-hint">{{ t('accountRefill.lowBalanceHint') }}</p>
            <div v-if="view.notification_status?.at" class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800" role="status"><p>{{ view.notification_status.message }}</p><p class="mt-1 text-xs text-gray-500">{{ date(view.notification_status.at) }}</p></div>
          </div>
        </fieldset>
      </form>
      <div v-if="activeTab === 'manual' || isFendudu" class="space-y-4">
        <p class="font-medium" data-testid="manual-refill-provider">{{ t('accountRefill.manualProvider', { provider: manualProviderName }) }}</p>
        <p v-if="manualStorageFailed" class="text-sm text-amber-700" role="alert">{{ t('accountRefill.manualStorageError') }}</p>
        <p v-if="otherProviderPending" class="text-sm text-amber-700" data-testid="manual-other-provider-pending">{{ t('accountRefill.otherProviderPending', { provider: t(isFendudu ? 'accountRefill.primaryName' : 'accountRefill.fenduduName') }) }}</p>
        <details v-if="isFendudu" :open="!view.fendudu?.token_configured || fenduduDirty" class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700" data-testid="fendudu-connection">
          <summary class="cursor-pointer text-sm font-medium">{{ t('accountRefill.fenduduConnectionSettings') }} · {{ t(view.fendudu?.token_configured ? 'accountRefill.configured' : 'accountRefill.unconfigured') }}</summary>
          <label class="block"><span class="input-label">{{ t('accountRefill.fenduduToken') }} · {{ t(view.fendudu?.token_configured ? 'accountRefill.configured' : 'accountRefill.unconfigured') }}</span><input v-model="fenduduToken" type="password" autocomplete="new-password" class="input" placeholder="rvak_…" :disabled="busy" data-testid="fendudu-token" /><span class="input-hint">{{ t('accountRefill.tokenHint') }}</span></label>
          <label class="block"><span class="input-label">{{ t('accountRefill.fenduduConnection') }}</span><input v-model="fenduduConnectionID" class="input" placeholder="s2e_…" :disabled="busy" data-testid="fendudu-connection-id" /><span class="input-hint">{{ t('accountRefill.fenduduConnectionHint') }}</span></label>
          <button v-admin-write type="button" class="btn btn-secondary" :disabled="busy || dirty || conflict || !!manualPending" data-testid="fendudu-save" @click="saveFendudu">{{ t('accountRefill.fenduduSave') }}</button>
          <p class="text-xs text-gray-500">{{ t('accountRefill.fenduduUpdated') }} {{ date(view.fendudu?.checked_at) }}</p>
        </details>
        <p class="text-sm leading-6 text-gray-500">{{ t(isFendudu ? 'accountRefill.fenduduHint' : 'accountRefill.manualHint') }}</p>
        <label class="block"><span class="input-label">{{ t('accountRefill.plan') }}</span><select v-model="manualPlan" class="input" :disabled="busy || !!manualPending" data-testid="manual-refill-plan"><option value="pro5x">Pro5x</option><option value="pro20x">Pro20x</option></select></label>
        <div class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800">
          <template v-if="isFendudu"><p>{{ t('accountRefill.fenduduAvailable') }}: <strong>{{ number(fenduduProduct?.available_now) }}</strong> {{ t('accountRefill.fenduduUnit') }}</p><p v-if="fenduduProduct?.blocked_reason || fenduduProduct?.expired" class="mt-2 text-amber-700">{{ fenduduProduct.blocked_reason || t('accountRefill.expired') }}</p></template>
          <p v-else>{{ t('accountRefill.quote') }}: <strong>{{ number(manualQuote?.price) }}</strong> {{ t('accountRefill.points') }}</p>
          <p v-if="!isFendudu && manualQuote" class="mt-2 text-gray-500">{{ t('accountRefill.terms') }}: {{ manualQuote.terms }}</p>
        </div>
        <div class="rounded-lg border border-gray-200 p-4 text-sm dark:border-dark-700" data-testid="manual-refill-settings">
          <p class="font-medium">{{ t('accountRefill.manualSavedSettings') }}</p>
          <dl class="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div><dt class="text-gray-500">{{ t('accountRefill.deliveryGroups') }}</dt><dd>{{ manualGroups }}</dd></div>
            <div><dt class="text-gray-500">{{ t('accountRefill.import_concurrency') }}</dt><dd data-testid="refill-manual-concurrency">{{ view.config.plan_import_concurrency?.[manualPlan] ?? view.config.import_concurrency }}</dd></div>
            <div><dt class="text-gray-500">{{ t('admin.accounts.privacyProtection') }}</dt><dd>{{ t(view.config.delivery.privacy_enabled !== false ? 'common.enabled' : 'common.disabled') }}</dd></div>
            <div><dt class="text-gray-500">{{ t('admin.accounts.openai.flattenNamespaces') }}</dt><dd data-testid="refill-manual-flatten-namespaces">{{ t(view.config.delivery.openai_responses_flatten_namespaces === true ? 'common.enabled' : 'common.disabled') }}</dd></div>
            <div><dt class="text-gray-500">{{ t('accountRefill.proxy_mode') }}</dt><dd>{{ t(view.config.delivery.proxy_mode === 'ovh_ipv6' ? 'ovhIPv6.mode.ovh_ipv6' : `accountRefill.${view.config.delivery.proxy_mode}`) }} {{ view.config.delivery.proxy_mode === 'specified' ? `#${view.config.delivery.proxy_id}` : '' }}</dd></div>
            <div><dt class="text-gray-500">{{ t('accountRefill.import_expiry_grace_minutes') }}</dt><dd>{{ view.config.import_expiry_grace_minutes }}</dd></div>
            <div><dt class="text-gray-500">{{ t('accountRefill.priority') }} / {{ t('accountRefill.load_factor') }}</dt><dd>{{ view.config.delivery.priority }} / {{ view.config.delivery.load_factor }}</dd></div>
            <div><dt class="text-gray-500">{{ t('accountRefill.rate_multiplier') }}</dt><dd>{{ view.config.delivery.rate_multiplier }}</dd></div>
          </dl>
          <button type="button" class="mt-3 text-primary-600 underline" @click="activeTab = 'delivery'">{{ t('accountRefill.manualReviewSettings') }}</button>
        </div>
        <p v-if="dirty || conflict || (isFendudu && fenduduDirty)" class="text-sm text-amber-700">{{ t('accountRefill.manualSaveFirst') }}</p>
        <p v-if="!manualTokenConfigured || !view.config.push_enabled" class="text-sm text-amber-700">{{ t('accountRefill.manualConfigureFirst') }}</p>
        <p v-if="manualPending" class="text-sm text-amber-700" role="status">{{ t('accountRefill.manualUncertain', { provider: manualProviderName }) }}</p>
        <div class="flex flex-wrap gap-3">
          <button v-admin-write type="button" class="btn btn-primary" :disabled="manualDisabled" data-testid="manual-refill-submit" @click="submitManual()">{{ t(manualPending ? 'accountRefill.manualRetry' : isFendudu ? 'accountRefill.fenduduSubmit' : 'accountRefill.manualSubmit', { provider: manualProviderName }) }}</button>
          <button v-admin-write type="button" class="btn btn-secondary" :disabled="manualForceDisabled" data-testid="manual-refill-force" @click="submitManual(true)">{{ t('accountRefill.manualForce', { provider: manualProviderName }) }}</button>
        </div>
        <p class="text-xs leading-relaxed text-amber-700 dark:text-amber-300">{{ t(isFendudu ? 'accountRefill.fenduduForceHint' : 'accountRefill.manualForceHint') }}</p>
      </div>
      <div v-if="activeTab === 'progress'" class="space-y-5">
        <div class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800">
          <p>{{ view.lease_sync_message || t('accountRefill.leaseSyncHint') }}</p>
          <p class="mt-1 text-xs text-gray-500">{{ t('accountRefill.leaseSyncAt') }}: {{ date(view.lease_sync_at) }}</p>
        </div>
        <div v-if="view.capacity_observation && view.config.mode === 'capacity'" class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800" aria-live="polite" data-testid="refill-current-observation">
          <p>{{ t('accountRefill.capacityObservation') }}: {{ t(`accountRefill.observation_${view.capacity_observation.status}`) }}</p>
          <p class="mt-1 tabular-nums">{{ t(view.capacity_observation.no_capacity ? 'accountRefill.observationNoCapacity' : 'accountRefill.observationProgress', { elapsed: view.capacity_observation.sustained_seconds, required: view.capacity_observation.required_seconds, utilization: number(view.capacity_observation.utilization_percent) }) }}</p>
          <p class="mt-1 text-xs text-gray-500">{{ t('accountRefill.lastSample') }}: {{ date(view.capacity_observation.last_sample_at) }}</p>
        </div>
        <div v-if="view.config.mode === 'capacity'" class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-800" aria-live="polite" data-testid="refill-forecast-observation">
          <p>{{ t('accountRefill.forecastObservation') }}: {{ t(`accountRefill.observation_${view.config.forecast_enabled === false ? 'disabled' : view.forecast_observation?.status || 'waiting'}`) }}</p>
          <template v-if="view.config.forecast_enabled !== false && view.forecast_observation && !view.evaluation.error">
            <p class="mt-1 tabular-nums">{{ t('accountRefill.forecastCapacity', { count: number(view.evaluation.forecast_accounts), current: number(view.evaluation.capacity), projected: number(view.evaluation.forecast_capacity), pending: number(view.evaluation.pending_capacity), inUse: number(view.evaluation.in_use) }) }}</p>
            <p v-if="forecastAccounts.length" class="mt-1 break-words text-xs text-gray-500">{{ t('accountRefill.forecastAccounts') }}: {{ forecastAccounts.map(account => `#${account.id} ${account.name}`).join('、') }}</p>
            <p class="mt-1 tabular-nums">{{ t(view.forecast_observation.no_capacity ? 'accountRefill.observationNoCapacity' : 'accountRefill.forecastProgress', { elapsed: view.forecast_observation.sustained_seconds, required: view.forecast_observation.required_seconds, utilization: number(view.forecast_observation.utilization_percent) }) }}</p>
          </template>
          <p class="mt-1 text-xs text-gray-500">{{ t('accountRefill.forecastHint') }}</p>
          <p v-if="view.config.forecast_enabled !== false && view.forecast_observation" class="mt-1 text-xs text-gray-500">{{ t('accountRefill.lastSample') }}: {{ date(view.forecast_observation.last_sample_at) }}</p>
        </div>
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-4"><div v-for="item in usageItems" :key="item.key" class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800"><div class="text-xs text-gray-500">{{ t(`accountRefill.${item.key}`) }}</div><div class="mt-1 font-semibold tabular-nums">{{ number(item.value) }}</div></div></div>
        <p class="text-xs leading-5 text-gray-500">{{ t('accountRefill.progressHint') }}</p>
        <p v-if="view.evaluation.timed_out_pending" class="text-sm text-amber-700 dark:text-amber-300" data-testid="refill-timeout-ignored">{{ t('accountRefill.timeoutIgnored', { count: view.evaluation.timed_out_pending }) }}</p>
        <div class="flex flex-wrap gap-5 text-sm"><span>{{ t('accountRefill.nextCheck') }}: {{ date(view.next_check_at) }}</span><span v-if="view.config.mode === 'interval'">{{ t('accountRefill.nextInterval') }}: {{ date(view.next_interval_at) }}</span></div>
        <div>
          <div class="overflow-x-auto">
            <table class="w-full min-w-[800px] text-left text-sm" data-testid="refill-jobs">
              <thead class="border-b border-gray-200 text-xs text-gray-500 dark:border-dark-700">
                <tr>
                  <th class="p-2">{{ t('accountRefill.task') }}</th>
                  <th class="p-2">{{ t('accountRefill.reason') }}</th>
                  <th class="p-2">{{ t('accountRefill.status') }}</th>
                  <th class="p-2">{{ t('accountRefill.points') }}</th>
                  <th class="p-2">{{ t('accountRefill.elapsed') }}</th>
                  <th class="p-2">{{ t('accountRefill.created') }}</th>
                  <th class="p-2">{{ t('common.actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="job in pagedJobs" :key="job.id" class="border-b border-gray-100 align-top dark:border-dark-800">
                  <td class="p-2">
                    <span class="font-medium">{{ job.plan }}</span><span v-if="job.provider === 'fendudu'" class="ml-2 text-xs text-gray-500">{{ t('accountRefill.fenduduName') }}</span>
                    <div class="max-w-48 break-all text-xs text-gray-500">{{ job.supplier_id || t('accountRefill.supplierIDPending') }}</div>
                    <div v-if="job.account_id" class="text-xs">{{ t('accountRefill.account') }} #{{ job.account_id }}</div>
                    <div v-if="job.provider === 'fendudu'" class="mt-1 text-xs" data-testid="fendudu-supplier-expiry">
                      {{ t('accountRefill.supplierReclaimAt') }}：{{ job.supplier_expires_at ? date(new Date(job.supplier_expires_at * 1000).toISOString()) : t('accountRefill.supplierExpiryPending') }}
                    </div>
                  </td>
                  <td class="min-w-56 max-w-80 break-words p-2" data-testid="refill-job-reason">
                    <p class="font-medium">{{ job.reason || t('accountRefill.reasonUnknown') }}</p>
                    <p v-if="job.reason_detail" class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-300">{{ job.reason_detail }}</p>
                  </td>
                  <td class="max-w-80 p-2">
                    <span class="font-medium">{{ stateLabel(job.status) }}</span>
                    <div v-if="job.supplier_status" class="mt-1 text-xs text-gray-500" data-testid="refill-supplier-status">{{ t('accountRefill.supplierStatus') }}: {{ supplierStateLabel(job.supplier_status) }} · {{ job.supplier_status }}</div>
                    <div v-if="job.supplier_error" class="mt-1 break-words text-xs text-amber-700 dark:text-amber-300">{{ t('accountRefill.supplierError') }}: {{ job.supplier_error }}</div>
                    <div v-if="job.error" class="mt-1 text-xs text-amber-700 dark:text-amber-300">{{ job.error }}</div>
                    <button v-admin-write v-if="['blocked', 'import_failed'].includes(job.status) && !(job.provider === 'fendudu' && !job.supplier_id && job.submission_started_at)" type="button" class="mt-2 text-xs text-primary-600 underline" :disabled="busy" :title="t('accountRefill.retryHint')" @click="retry(job.id)">{{ t('accountRefill.retry') }}</button>
                    <div v-if="job.provider === 'fendudu' && job.status === 'blocked' && !job.supplier_id && job.submission_started_at" class="mt-2 space-y-2"><label class="block text-xs">{{ t('accountRefill.fenduduOrderID') }}<input v-model="reconcileIDs[job.id]" class="input mt-1" placeholder="rvc_…" :disabled="busy" :data-testid="`fendudu-order-${job.id}`" /></label><button type="button" class="btn btn-secondary text-xs" :disabled="busy || !reconcileIDs[job.id]?.trim()" @click="reconcileFendudu(job)">{{ t('accountRefill.fenduduReconcile') }}</button></div>
                    <details v-if="job.status === 'blocked' && !job.supplier_id" class="mt-2 text-xs">
                      <summary class="cursor-pointer text-amber-700">{{ t('accountRefill.dismiss') }}</summary>
                      <label class="mt-2 flex gap-2"><input v-model="confirmedJobs" type="checkbox" :value="job.id" />{{ t('accountRefill.dismissHint') }}</label>
                      <button type="button" class="btn btn-secondary mt-2 text-xs" :disabled="busy || !confirmedJobs.includes(job.id)" @click="dismiss(job.id)">{{ t('accountRefill.dismiss') }}</button>
                    </details>
                  </td>
                  <td v-if="job.provider === 'fendudu'" class="p-2 tabular-nums">1 {{ t('accountRefill.fenduduUnit') }}<div class="mt-1 text-xs text-gray-500">{{ t('accountRefill.fenduduPrepaid') }}</div></td>
                  <td v-else class="p-2 tabular-nums">{{ number(job.price) }}<div class="mt-1 whitespace-nowrap text-xs text-gray-500">{{ t(`accountRefill.${job.budget_released ? 'amountReleased' : job.supplier_status === 'succeeded' ? 'amountCharged' : 'amountHeld'}`) }}</div></td>
                  <td class="whitespace-nowrap p-2 tabular-nums">{{ elapsed(job) }}</td>
                  <td class="whitespace-nowrap p-2 text-xs">{{ date(job.created_at) }}</td>
                  <td class="p-2">
                    <button type="button" class="btn btn-secondary whitespace-nowrap text-xs" data-testid="refill-download" :disabled="!!downloadReason(job) || downloading.has(job.id)" :title="t(downloadReason(job) || 'accountRefill.downloadHint')" @click="download(job)">{{ t(downloading.has(job.id) ? 'accountRefill.downloading' : 'accountRefill.downloadJSON') }}</button>
                  </td>
                </tr>
              </tbody>
            </table>
            <p v-if="!view.jobs.length" class="py-6 text-center text-sm text-gray-500">{{ t('accountRefill.noJobs') }}</p>
          </div>
          <Pagination v-if="view.jobs.length" v-model:page="jobsPage" :total="view.jobs.length" :page-size="jobsPageSize" :show-page-size-selector="false" data-testid="refill-jobs-pagination" />
        </div>
        <h4 class="text-sm font-medium">{{ t('accountRefill.evaluation') }}</h4>
        <p class="text-xs text-gray-500">{{ t('accountRefill.evaluationLocalHint') }} {{ t('accountRefill.evaluationUpdated') }}: {{ date(view.evaluation.checked_at) }}</p>
        <p v-if="view.evaluation.error" class="text-sm text-amber-700" role="alert">{{ view.evaluation.error }}</p>
        <div class="overflow-x-auto"><table class="w-full text-left text-sm"><thead class="border-b text-xs text-gray-500 dark:border-dark-700"><tr><th class="p-2">{{ t('accountRefill.account') }}</th><th class="p-2">{{ t('accountRefill.status') }}</th><th class="p-2">{{ t('accountRefill.sourceExpiry') }}</th><th class="p-2">{{ t('accountRefill.localExpiry') }}</th><th class="p-2">{{ t('accountRefill.remaining5h') }}</th><th class="p-2">{{ t('accountRefill.remaining7d') }}</th></tr></thead><tbody><tr v-for="account in view.evaluation.accounts" :key="account.id" class="border-b border-gray-100 dark:border-dark-800"><td class="max-w-48 break-all p-2">#{{ account.id }} {{ account.name }}<div v-if="account.plan" class="text-xs text-gray-500">{{ account.plan === 'pro5x' ? 'Pro5x' : 'Pro20x' }}</div></td><td class="p-2">{{ stateLabel(account.state) }}</td><td class="whitespace-nowrap p-2 text-xs">{{ date(account.source_expires_at) }}<div class="mt-1 text-xs text-gray-500">{{ t(`accountRefill.expirySource_${account.expiry_source || 'local'}`) }}</div></td><td class="whitespace-nowrap p-2 text-xs">{{ date(account.expires_at) }}</td><td class="p-2">{{ account.remaining_5h == null ? t('accountRefill.quotaUnknown') : `${number(account.remaining_5h)}%` }}<div v-if="account.min_remaining_5h != null" class="text-xs text-gray-500">{{ t('accountRefill.quotaThreshold', { percent: number(account.min_remaining_5h) }) }}</div></td><td class="p-2">{{ account.remaining_7d == null ? t('accountRefill.quotaUnknown') : `${number(account.remaining_7d)}%` }}<div v-if="account.min_remaining_7d != null" class="text-xs text-gray-500">{{ t('accountRefill.quotaThreshold', { percent: number(account.min_remaining_7d) }) }}</div></td></tr></tbody></table><p v-if="!view.evaluation.accounts?.length" class="py-4 text-center text-sm text-gray-500">{{ t('accountRefill.noAccounts') }}</p></div>
      </div>
      <p v-if="conflict" class="text-sm text-amber-700" role="alert">{{ t('accountRefill.conflict') }}</p>
    </div>
    <p v-if="error" class="mt-3 text-sm text-red-600 dark:text-red-400" role="alert">{{ error }}</p>
    <template #footer>
      <span class="mr-auto text-xs text-gray-500">{{ dirty ? t('accountRefill.dirty') : '' }}</span>
      <button type="button" class="btn btn-secondary" :disabled="loading" @click="refresh(false)">{{ t('accountRefill.refresh') }}</button>
      <button type="button" class="btn btn-secondary" :disabled="busy || !view || dirty" :title="t('accountRefill.checkHint')" @click="check">{{ t('accountRefill.check') }}</button>
      <button v-admin-write type="submit" form="refill-settings" class="btn btn-primary" :disabled="busy || !dirty || conflict || !form" data-testid="refill-save">{{ t(busy ? 'accountRefill.saving' : 'accountRefill.save') }}</button>
    </template>
  </BaseDialog>
  <TotpStepUpDialog :controller="downloadStepUp" />
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getCodexFingerprintModeOptions } from '@/utils/codexFingerprintMode'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import Toggle from '@/components/common/Toggle.vue'
import { DEFAULT_SAME_ACCOUNT_RETRY_COUNT, MAX_SAME_ACCOUNT_RETRY_COUNT } from '@/components/account/sameAccountRetry'
import OpenAIPrivacyControl from '@/components/account/OpenAIPrivacyControl.vue'
import ProxySelector from '@/components/common/ProxySelector.vue'
import { accountRefillAPI, type RefillConfig, type RefillView, type RefillJob, type ManualRefillRequest } from '@/api/admin/accountRefill'
import { getAllIncludingInactive } from '@/api/admin/groups'
import type { AdminGroup, Proxy } from '@/types'
import { useAppStore } from '@/stores/app'
import { saveAs } from 'file-saver'
import { useStepUp, isStepUpBlocked, isStepUpCancelled, stepUpBlockReason } from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'

const props = defineProps<{ show: boolean; initialTab?: 'connection' | 'manual' | 'fendudu'; proxies?: Proxy[] }>()
const emit = defineEmits<{ close: [] }>()
const { t, te } = useI18n()
const codexFingerprintModeOptions = computed(() => getCodexFingerprintModeOptions(t))
const app = useAppStore()
const tabs = ['connection', 'rules', 'delivery', 'manual', 'fendudu', 'notifications', 'progress'] as const
const activeTab = ref<typeof tabs[number]>('connection')
const tabsNav = ref<HTMLElement | null>(null)
const view = ref<RefillView | null>(null)
const form = ref<RefillConfig | null>(null)
const selectedGroup = ref(0)
const switchingGroup = ref(false)
const groups = ref<AdminGroup[]>([])
const manualProxies = computed(() => (props.proxies || []).filter(proxy => !proxy.managed_ipv6))
const confirmedJobs = ref<string[]>([])
const downloading = ref(new Set<string>())
const downloadStepUp = useStepUp()
const jobsPageSize = 10
const jobsPage = ref(1)
const pagedJobs = computed(() => view.value?.jobs.slice((jobsPage.value - 1) * jobsPageSize, jobsPage.value * jobsPageSize) || [])
const token = ref(''), mapping = ref('{}'), saved = ref(''), error = ref('')
const webhook = ref('')
const fenduduToken = ref(''), fenduduConnectionID = ref('')
const reconcileIDs = ref<Record<string, string>>({})
const isFendudu = computed(() => activeTab.value === 'fendudu')
const fenduduDirty = computed(() => fenduduToken.value !== '' || fenduduConnectionID.value !== (view.value?.fendudu?.connection_id || ''))
const manualTokenConfigured = computed(() => isFendudu.value ? view.value?.fendudu?.token_configured : view.value?.token_configured)
const fenduduProduct = computed(() => view.value?.fendudu?.products?.find(p => p.product === manualPlan.value))
const manualProviderName = computed(() => t(isFendudu.value ? 'accountRefill.fenduduName' : 'accountRefill.primaryName'))
const manualRequests = ref<ManualRefillRequest[]>([])
const manualPending = computed(() => manualRequests.value.find(request => request.group_id === selectedGroup.value && (request.provider === 'fendudu') === isFendudu.value))
const otherProviderPending = computed(() => manualRequests.value.some(request => request.group_id === selectedGroup.value && (request.provider === 'fendudu') !== isFendudu.value))
const manualPlans = ref({ primary: 'pro5x', fendudu: 'pro5x' })
const manualPlan = computed({
  get: () => manualPending.value?.plan || manualPlans.value[isFendudu.value ? 'fendudu' : 'primary'],
  set: value => { manualPlans.value[isFendudu.value ? 'fendudu' : 'primary'] = value }
})
const legacyManualStorageKey = 'yoshub:manual-refill:pending'
const manualStorageKey = 'yoshub:manual-refill:pending:v2'
const manualStorageFailed = ref(false)
const notifiedManualImports = new Set<string>()
const loading = ref(false), busy = ref(false), conflict = ref(false), now = ref(Date.now())
let revision = 0, generation = 0, fetching = false
let timer: ReturnType<typeof setInterval> | undefined
const openAIGroups = computed(() => groups.value.filter(g => g.platform === 'openai' && g.status === 'active'))
const ruleGroups = computed(() => groups.value.filter(g => g.platform === 'openai'))
const rotationPreview = computed(() => form.value?.rotation?.map(step => `${step.plan === 'pro20x' ? 'Pro20x' : 'Pro5x'} × ${step.count}`).join(' → '))
const dirty = computed(() => !!form.value && (JSON.stringify(form.value) !== saved.value || token.value !== '' || webhook.value !== '' || mapping.value !== JSON.stringify(form.value.delivery.model_mapping || {}, null, 2)))
const walletItems = computed(() => [{ key: 'balance', value: view.value?.wallet?.balance }, { key: 'held', value: view.value?.wallet?.held }, { key: 'available', value: view.value?.wallet?.available ?? view.value?.catalog?.available }])
const usageItems = computed(() => {
  const v = view.value
  return [{ key: 'spent', value: v?.daily.spent_points }, { key: 'reserved', value: v?.daily.reserved_points }, { key: 'count', value: v?.daily.count }, { key: 'budget', value: v?.config.daily_max_points }, { key: 'ready', value: v?.evaluation.ready }, { key: 'expiring', value: v?.evaluation.expiring }, { key: 'pending', value: v?.evaluation.pending }, { key: 'needed', value: v?.evaluation.needed }]
})
const quote = computed(() => view.value?.catalog?.items.find(q => q.service === 'refill' && q.plan === (form.value?.rotation_enabled ? (dirty.value ? form.value.rotation[0]?.plan : view.value?.next_plan) : form.value?.plan) && q.enabled))
const manualQuote = computed(() => view.value?.catalog?.items.find(q => q.service === 'refill' && q.plan === manualPlan.value && q.enabled))
const manualGroups = computed(() => view.value?.config.delivery_group_ids.map(id => groups.value.find(g => g.id === id)?.name || `#${id}`).join('、'))
const manualDisabled = computed(() => busy.value || !view.value || manualStorageFailed.value || (!manualPending.value && (dirty.value || conflict.value || (isFendudu.value && fenduduDirty.value) || !manualTokenConfigured.value || !view.value.config.push_enabled)))
const manualForceDisabled = computed(() => busy.value || !view.value || manualStorageFailed.value || dirty.value || conflict.value || (isFendudu.value && fenduduDirty.value) || !manualTokenConfigured.value || !view.value.config.push_enabled)
const keyDays = computed(() => { const at = view.value?.key?.expires_at; return at && !at.startsWith('0001') ? (Date.parse(at) - now.value) / 86400000 : null })
const keyRemaining = computed(() => keyDays.value === null ? '' : keyDays.value <= 0 ? t('accountRefill.expired') : t('accountRefill.keyRemaining', { days: Math.floor(keyDays.value * 10) / 10 }))
const fixedExpiry = computed({ get: () => form.value?.delivery.fixed_expires_at ? new Date(form.value.delivery.fixed_expires_at * 1000 + 8 * 3600000).toISOString().slice(0, 16) : '', set: v => { if (form.value) form.value.delivery.fixed_expires_at = v ? Math.floor(Date.parse(`${v}:00+08:00`) / 1000) : null } })
type NumericKey = { [K in keyof RefillConfig]-?: RefillConfig[K] extends number ? K : never }[keyof RefillConfig]
const ruleFields = computed<{ key: NumericKey; min: number; max: number; step?: number }[]>(() => [
  ...(['min_ready', 'max_ready'] as const).map(key => ({ key, min: 1, max: 100 })),
  { key: 'lead_minutes', min: 0, max: 1440 },
  ...(['min_remaining_5h', 'min_remaining_7d'] as const).map(key => ({ key, min: 0, max: 100, step: 0.1 })),
  ...(form.value?.mode === 'capacity' ? [{ key: 'target_concurrency_percent' as const, min: 1, max: 100 }, { key: 'concurrency_sustain_seconds' as const, min: 1, max: 3600 }] : []),
  ...(form.value?.mode === 'interval' ? [{ key: 'interval_minutes' as const, min: 1, max: 10080 }, { key: 'interval_quantity' as const, min: 1, max: 100 }] : []),
  { key: 'check_seconds', min: 5, max: 3600 }, { key: 'cooldown_seconds', min: 5, max: 86400 }, { key: 'max_in_flight', min: 1, max: 20 }, { key: 'daily_max_count', min: 1, max: 1000 }, { key: 'daily_max_points', min: 0.01, max: 1e7, step: 0.01 }, { key: 'max_unit_price', min: 0, max: 1e7, step: 0.01 }
])
const deliveryFields = [{ key: 'priority', max: 2147483647, step: 1 }, { key: 'load_factor', max: 10000, step: 1 }, { key: 'rate_multiplier', max: 1000, step: 0.01 }] as const
const number = (v: number | null | undefined) => v == null ? '—' : v.toLocaleString(undefined, { maximumFractionDigits: 8 })
const forecastAccounts = computed(() => view.value?.evaluation.accounts.filter(account => account.forecast_loss) || [])
const date = (v: string | null | undefined) => !v || v.startsWith('0001') ? '—' : new Date(v).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })
const stateLabel = (state: string) => te(`accountRefill.states.${state}`) ? t(`accountRefill.states.${state}`) : state
const supplierStateLabel = (state: string) => ['queued', 'processing', 'succeeded', 'failed', 'review'].includes(state) ? stateLabel(state) : t('accountRefill.states.supplier_unknown')
function downloadReason(job: RefillJob) {
  if (job.provider === 'fendudu') return 'accountRefill.fenduduDownload'
  if (job.status === 'delivery_expired' || (job.expires_at != null && job.expires_at * 1000 <= now.value)) return 'accountRefill.downloadExpired'
  if (!job.supplier_id || job.supplier_status !== 'succeeded' || !job.result_ready) return 'accountRefill.downloadNotReady'
  return ''
}
async function download(job: RefillJob) {
  if (!job.supplier_id || downloadReason(job) || downloading.value.has(job.id)) return
  const groupID = selectedGroup.value
  const current = generation
  downloading.value.add(job.id)
  try {
    const data = await downloadStepUp.run(() => accountRefillAPI.download(job.id, groupID))
    if (current !== generation) return
    saveAs(new Blob([data], { type: 'application/json;charset=utf-8' }), `sub2api-refill-${job.supplier_id.replace(/[^a-zA-Z0-9._-]/g, '_')}.json`)
    app.showSuccess(t('accountRefill.downloaded'))
  } catch (e) {
    if (current !== generation || isStepUpCancelled(e)) return
    error.value = isStepUpBlocked(e) ? t(stepUpBlockReason(e) === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN' ? 'stepUp.adminApiKeyForbidden' : 'stepUp.notEnabled') : message(e)
  } finally { downloading.value.delete(job.id) }
}
const elapsed = (job: RefillJob) => { const seconds = Math.max(0, Math.floor(((job.finished_at ? Date.parse(job.finished_at) : now.value) - Date.parse(job.created_at)) / 1000)); return `${Math.floor(seconds / 60)}m ${seconds % 60}s` }
function initializeRotation() { if (form.value?.rotation_enabled && !form.value.rotation?.length) form.value.rotation = [{ plan: 'pro20x', count: 1 }, { plan: 'pro5x', count: 2 }] }
function togglePlanQuota(plan: string) {
  if (!form.value) return
  const thresholds = form.value.plan_quota_thresholds ??= {}
  if (thresholds[plan]) delete thresholds[plan]
  else thresholds[plan] = { min_remaining_5h: form.value.min_remaining_5h, min_remaining_7d: form.value.min_remaining_7d }
}
function moveRotation(index: number) { if (form.value && index > 0) { const rows = form.value.rotation; [rows[index - 1], rows[index]] = [rows[index]!, rows[index - 1]!] } }
async function switchGroup(id: number) {
  if (!id || busy.value || dirty.value || manualPending.value) return
  const current = ++generation
  busy.value = true; switchingGroup.value = true; error.value = ''
  try { const data = await accountRefillAPI.get(id); if (current === generation) accept(data, true) }
  catch (e) { if (current === generation) error.value = message(e) }
  finally { if (current === generation) { busy.value = false; switchingGroup.value = false } }
}
function accept(data: RefillView, reset: boolean) {
  if (!reset && data.config.group_id !== selectedGroup.value) return
  data.config.delivery.same_account_retry_count ??= DEFAULT_SAME_ACCOUNT_RETRY_COUNT
  data.config.delivery.privacy_enabled ??= true
  data.config.delivery.openai_responses_flatten_namespaces ??= false
  data.config.forecast_enabled ??= true
  data.config.immediate_failure_retry ??= true
  data.config.timeout_retry_enabled ??= true
  data.config.timeout_retry_notify ??= true
  data.config.timeout_retry_minutes ??= 10
  data.config.daily_schedules ??= data.config.daily_schedule ? [{ id: 'legacy', plan: '', ...data.config.daily_schedule }] : []
  for (const daily of data.config.daily_schedules) daily.day_mode ??= ''
  delete data.config.daily_schedule
  data.config.rotation ??= []
  data.config.rotation_enabled ??= false
  data.config.plan_quota_thresholds ??= {}
  data.config.notifications.refill_needed ??= false
  data.config.plan_import_concurrency ??= {}
  for (const plan of ['pro5x', 'pro20x']) data.config.plan_import_concurrency[plan] ??= data.config.import_concurrency
  if (!reset && data.revision < revision) return
  const previousJobs = view.value?.config.group_id === data.config.group_id
    ? new Map(view.value.jobs.map(job => [job.id, job])) : null
  for (const job of data.jobs) {
    if ((!job.manual_request_id && job.reason !== '手动补号') || job.status !== 'imported' || !job.account_id) continue
    const key = `${data.config.group_id}:${job.id}`
    if (notifiedManualImports.has(key)) continue
    notifiedManualImports.add(key)
    const previous = previousJobs?.get(job.id)
    const pendingRequest = manualRequests.value.some(request => request.group_id === data.config.group_id && request.request_id === job.manual_request_id)
    if ((previous && (previous.status !== 'imported' || !previous.account_id)) || pendingRequest) {
      app.showSuccess(t('accountRefill.manualImported', { plan: job.plan, account: job.account_id }))
    }
  }
  if (!reset && view.value && Date.parse(data.evaluation.checked_at) < Date.parse(view.value.evaluation.checked_at)) data.evaluation = view.value.evaluation
  if (!reset && dirty.value && data.revision !== revision) conflict.value = true
  if (reset || !dirty.value) {
    selectedGroup.value = data.config.group_id
    form.value = JSON.parse(JSON.stringify(data.config))
    mapping.value = JSON.stringify(data.config.delivery.model_mapping || {}, null, 2)
    saved.value = JSON.stringify(form.value)
    revision = data.revision
    conflict.value = false
  }
  if (!view.value || view.value.config.group_id !== data.config.group_id) {
    jobsPage.value = 1
    confirmedJobs.value = []
  } else {
    jobsPage.value = Math.min(jobsPage.value, Math.max(1, Math.ceil(data.jobs.length / jobsPageSize)))
  }
  if (reset || !fenduduDirty.value) fenduduConnectionID.value = data.fendudu?.connection_id || ''
  view.value = data
  if (reset && isFendudu.value && !manualPending.value && !data.fendudu?.products?.some(p => p.product === manualPlan.value && p.available_now > 0 && !p.expired && !p.blocked_reason)) {
    manualPlan.value = data.fendudu?.products?.find(p => ['pro5x', 'pro20x'].includes(p.product) && p.available_now > 0 && !p.expired && !p.blocked_reason)?.product || manualPlan.value
  }
  const currentPending = manualPending.value
  const confirmed = manualRequests.value.filter(request => request.group_id === data.config.group_id && data.jobs.some(job => job.manual_request_id === request.request_id && job.provider === request.provider))
  for (const request of confirmed) clearManualRequest(request)
  if (!reset && currentPending && confirmed.includes(currentPending) && ['manual', 'fendudu'].includes(activeTab.value)) {
    activeTab.value = 'progress'; jobsPage.value = 1
  }
  now.value = Date.now()
}
function message(e: unknown) { const v = e as { message?: string; response?: { data?: { message?: string; error?: string } } }; return v.response?.data?.message || v.response?.data?.error || v.message || t('accountRefill.error') }
async function refresh(reset: boolean) {
  if (fetching || switchingGroup.value) return
  const current = generation
  fetching = true
  try { const data = await accountRefillAPI.get(selectedGroup.value || undefined); if (current === generation) { accept(data, reset); error.value = '' } }
  catch { if (current === generation) error.value = t('accountRefill.loadError') }
  finally { fetching = false }
}
function addDailySchedule() {
  if (!form.value || busy.value || (form.value.daily_schedules?.length || 0) >= 20) return
  form.value.daily_schedules ??= []
  form.value.daily_schedules.push({ id: crypto.randomUUID(), enabled: false, time: '08:00', quantity: 1, plan: form.value.plan, day_mode: '' })
}
function dailyStatus(id: string) {
  return view.value?.daily_schedule_statuses?.[id] || (id === 'legacy' ? view.value?.daily_schedule_status : undefined)
}
async function save() {
  if (!form.value || busy.value || conflict.value) return
  if (!Number.isInteger(form.value.timeout_retry_minutes) || form.value.timeout_retry_minutes! < 1 || form.value.timeout_retry_minutes! > 1440) {
    error.value = t('accountRefill.timeoutRetryInvalid'); activeTab.value = 'rules'; return
  }
  if ((form.value.daily_schedules?.length || 0) > 20 || form.value.daily_schedules?.some(daily => !/^([01]\d|2[0-3]):[0-5]\d$/.test(daily.time) || !Number.isInteger(daily.quantity) || daily.quantity < 1 || daily.quantity > 100 || !['', 'pro5x', 'pro20x'].includes(daily.plan))) {
    error.value = t('accountRefill.dailyInvalid'); activeTab.value = 'rules'; return
  }
  let parsed: unknown
  try { parsed = JSON.parse(mapping.value); if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object' || Object.values(parsed).some(v => typeof v !== 'string')) throw new Error() }
  catch { error.value = t('accountRefill.mappingError'); activeTab.value = 'delivery'; return }
  busy.value = true; error.value = ''
  try {
    const config = JSON.parse(JSON.stringify(form.value)) as RefillConfig
    config.delivery.model_mapping = parsed as Record<string, string>
    const data = await accountRefillAPI.save(config, token.value, revision, webhook.value)
    token.value = ''; webhook.value = ''; accept(data, true); app.showSuccess(t('accountRefill.saved'))
  } catch (e) { error.value = message(e) }
  finally { busy.value = false }
}
async function saveFendudu() {
  if (busy.value || dirty.value || conflict.value || manualPending.value) return
  busy.value = true; error.value = ''; generation++
  try {
    const data = await accountRefillAPI.configureFendudu(selectedGroup.value, fenduduToken.value, fenduduConnectionID.value, revision)
    fenduduToken.value = ''; accept(data, true); app.showSuccess(t('accountRefill.saved'))
  } catch (e) { error.value = message(e) }
  finally { busy.value = false }
}
async function reconcileFendudu(job: RefillJob) {
  const id = reconcileIDs.value[job.id]?.trim()
  if (busy.value || !id) return
  busy.value = true; error.value = ''; generation++
  try { accept(await accountRefillAPI.reconcileFendudu(job.id, selectedGroup.value, id), false); delete reconcileIDs.value[job.id] }
  catch (e) { error.value = message(e) }
  finally { busy.value = false }
}
async function check() { if (busy.value || dirty.value) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.check(selectedGroup.value), false) } catch (e) { error.value = message(e) } finally { busy.value = false } }
async function retry(id: string) { if (busy.value) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.retry(id, selectedGroup.value), false) } catch (e) { error.value = message(e) } finally { busy.value = false } }
async function dismiss(id: string) { if (busy.value || !confirmedJobs.value.includes(id)) return; busy.value = true; error.value = ''; try { accept(await accountRefillAPI.dismiss(id, selectedGroup.value), false); confirmedJobs.value = confirmedJobs.value.filter(v => v !== id) } catch (e) { error.value = message(e) } finally { busy.value = false } }
function persistManualRequests(requests: ManualRefillRequest[]) {
  // Write all suppliers before removing the legacy slot. A failed write must
  // never discard an uncertain request or allow a new purchase without a receipt.
  sessionStorage.setItem(manualStorageKey, JSON.stringify(requests))
  sessionStorage.removeItem(legacyManualStorageKey)
  manualRequests.value = requests
}
function clearManualRequest(request: ManualRefillRequest) {
  const remaining = manualRequests.value.filter(item => item.request_id !== request.request_id)
  try { persistManualRequests(remaining) }
  catch { manualStorageFailed.value = true; error.value = t('accountRefill.manualStorageError') }
}
function restoreManualRequests() {
  manualRequests.value = []; manualStorageFailed.value = false
  try {
    const raw = sessionStorage.getItem(manualStorageKey)
    const requests: unknown = raw ? JSON.parse(raw) : []
    if (!Array.isArray(requests)) throw new Error('Invalid manual request storage')
    const legacy = sessionStorage.getItem(legacyManualStorageKey)
    if (legacy) requests.push(JSON.parse(legacy))
    for (const request of requests) {
      if (!request || typeof request !== 'object' || typeof request.request_id !== 'string' || !['pro5x', 'pro20x'].includes(request.plan) || !Number.isInteger(request.revision) || (request.group_id != null && (!Number.isInteger(request.group_id) || request.group_id <= 0)) || (request.provider != null && request.provider !== 'fendudu') || (request.force != null && typeof request.force !== 'boolean')) throw new Error('Invalid manual request storage')
      if (!manualRequests.value.some(item => item.request_id === request.request_id)) manualRequests.value.push(request as ManualRefillRequest)
    }
  } catch { manualStorageFailed.value = true; error.value = t('accountRefill.manualStorageError') }
}
async function submitManual(force = false) {
  if (!['manual', 'fendudu'].includes(activeTab.value) || (force ? manualForceDisabled.value : manualDisabled.value)) return
  const request = (!force && manualPending.value) || { request_id: `${Date.now()}:${crypto.randomUUID()}`, plan: manualPlan.value, revision, group_id: selectedGroup.value, ...(isFendudu.value ? { provider: 'fendudu' as const } : {}), ...(force ? { force: true } : {}) }
  const remaining = manualRequests.value.filter(item => item.group_id !== request.group_id || item.provider !== request.provider)
  try { persistManualRequests([...remaining, request]) }
  catch { error.value = t('accountRefill.manualStorageError'); return }
  busy.value = true; error.value = ''; generation++
  try {
    const data = await accountRefillAPI.manual(request)
    accept(data, false); clearManualRequest(request); activeTab.value = 'progress'; jobsPage.value = 1
    if (!data.jobs.some(job => job.manual_request_id === request.request_id && job.status === 'imported' && job.account_id)) {
      app.showSuccess(t('accountRefill.manualAccepted'))
    }
  } catch (e) {
    const failure = e as { status?: number; response?: { status?: number } }
    const status = failure.status ?? failure.response?.status
    if (status === 400 || status === 401 || status === 403) clearManualRequest(request)
    error.value = message(e)
  } finally { busy.value = false }
}
watch([activeTab, loading], async () => {
  await nextTick()
  tabsNav.value?.querySelector<HTMLElement>('[aria-pressed="true"]')?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
})
function close() { if (!busy.value) emit('close') }
watch(() => props.show, async show => {
  const current = ++generation
  if (timer) clearInterval(timer)
  token.value = ''; fenduduToken.value = ''; fenduduConnectionID.value = ''; reconcileIDs.value = {}
  webhook.value = ''
  downloadStepUp.onCancel()
  if (!show) return
  loading.value = true; error.value = ''; form.value = null; view.value = null; activeTab.value = props.initialTab || 'connection'
  restoreManualRequests()
  try {
    const [initial, allGroups] = await Promise.all([accountRefillAPI.get(), getAllIncludingInactive()])
    const firstGroup = initial.rules?.[0]?.group_id || allGroups.find(g => g.platform === 'openai' && g.status === 'active')?.id
    let data = !initial.config.group_id && firstGroup ? await accountRefillAPI.get(firstGroup) : initial
    // Pre-group legacy requests belong to the default rule, not whichever
    // supplier/group the administrator happens to view next.
    for (const request of manualRequests.value) request.group_id ??= data.config.group_id
    const pending = manualRequests.value.find(request => (request.provider === 'fendudu') === isFendudu.value)
    if (['manual', 'fendudu'].includes(activeTab.value) && pending?.group_id && pending.group_id !== data.config.group_id) data = await accountRefillAPI.get(pending.group_id)
    if (current === generation) { groups.value = allGroups; accept(data, true) }
  }
  catch { if (current === generation) error.value = t('accountRefill.loadError') }
  finally { if (current === generation) loading.value = false }
  if (current === generation) timer = setInterval(() => { now.value = Date.now(); void refresh(false) }, 15000)
}, { immediate: true })
onBeforeUnmount(() => { generation++; downloadStepUp.onCancel(); token.value = ''; fenduduToken.value = ''; webhook.value = ''; if (timer) clearInterval(timer) })
</script>
