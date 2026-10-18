<template>
  <section class="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-dark-600" data-testid="account-user-quota">
    <div class="flex items-center justify-between gap-3">
      <h3 class="font-semibold">{{ t('admin.accounts.userQuota.title') }}</h3>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || loading" @click="load(true)">{{ t('common.refresh') }}</button>
    </div>
    <p class="input-hint">{{ t('admin.accounts.userQuota.description') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <p v-if="message" role="status" class="text-sm text-emerald-600 dark:text-emerald-400">{{ message }}</p>
    <p v-if="loading" class="input-hint">{{ t('common.loading') }}</p>
    <template v-if="snapshot">
      <label class="flex items-center gap-2">
        <input v-model="enabled" type="checkbox" :disabled="busy" data-testid="quota-enabled" />
        {{ t('admin.accounts.userQuota.enable') }}
      </label>
      <div class="space-y-3">
        <label class="input-label">{{ t('admin.accounts.userQuota.members') }}</label>
        <div class="flex gap-2">
          <input v-model="search" type="search" class="input" :placeholder="t('admin.accounts.userQuota.search')" @keydown.enter.prevent="searchUsers" />
          <button type="button" class="btn btn-secondary" :disabled="searching" @click="searchUsers">{{ t('common.search') }}</button>
        </div>
        <div class="flex flex-wrap gap-2">
          <button v-for="user in searchResults" :key="user.id" type="button" class="btn btn-secondary btn-sm" :disabled="busy || memberIds.includes(user.id)" @click="memberIds.push(user.id)">
            + {{ user.username || user.email }}
          </button>
        </div>
        <ul class="flex flex-wrap gap-2">
          <li v-for="id in memberIds" :key="id" class="flex items-center gap-2 rounded-lg bg-gray-100 px-3 py-1 text-sm dark:bg-dark-700">
            {{ userName(id) }}
            <button type="button" :disabled="busy" :aria-label="t('admin.accounts.userQuota.removeMember', { name: userName(id) })" @click="memberIds = memberIds.filter(value => value !== id)">×</button>
          </li>
        </ul>
        <p class="input-hint">{{ t('admin.accounts.userQuota.membersHint') }}</p>
        <label class="input-label">{{ t('admin.accounts.userQuota.fallback') }}</label>
        <select v-model="newFallback" class="input" :disabled="busy" @change="addFallback">
          <option :value="0">{{ t('admin.accounts.userQuota.addFallback') }}</option>
          <option v-for="group in fallbackOptions" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
        <ol class="space-y-1 text-sm">
          <li v-for="(id, index) in fallbackIds" :key="id" class="flex items-center gap-2">
            <span>{{ index + 1 }}. {{ groupName(id) }}</span>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || index === 0" :aria-label="t('admin.accounts.userQuota.moveUp')" @click="moveFallback(index)">↑</button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" :aria-label="t('common.remove')" @click="fallbackIds.splice(index, 1)">×</button>
          </li>
        </ol>
        <p class="input-hint">{{ t('admin.accounts.userQuota.fallbackHint') }}</p>
        <label class="input-label">{{ t('admin.accounts.userQuota.carryover') }}
          <input v-model.number="carryoverMinutes" type="number" min="10" max="1440" class="input mt-1" :disabled="busy" />
        </label>
        <button type="button" class="btn btn-primary" :disabled="busy || loading" data-testid="quota-save" @click="save">{{ t('admin.accounts.userQuota.save') }}</button>
        <p class="input-hint">{{ t('admin.accounts.userQuota.independentSave') }}</p>
      </div>

      <div v-if="snapshot.policy" class="space-y-2 border-t border-gray-200 pt-4 dark:border-dark-600">
        <div class="flex flex-wrap items-center gap-3 text-sm">
          <strong>{{ t(`admin.accounts.userQuota.states.${snapshot.estimate_state}`) }}</strong>
          <span>{{ t('admin.accounts.userQuota.estimate') }}: {{ money(snapshot.cycle?.estimate) }}</span>
        </div>
        <p class="input-hint">{{ t('admin.accounts.userQuota.resetsAt') }}: {{ date(snapshot.cycle?.resets_at) }}</p>
        <p class="input-hint">{{ t('admin.accounts.userQuota.estimateAt') }}: {{ date(snapshot.cycle?.estimate_at) }}</p>
        <p v-if="snapshot.expires_at" class="input-hint">{{ t('admin.accounts.userQuota.expiresAt') }}: {{ date(snapshot.expires_at) }}</p>
        <p v-if="snapshot.cycle?.estimate_source_cycle_id && snapshot.cycle.estimate_source_cycle_id !== snapshot.cycle.id" class="input-hint">{{ t('admin.accounts.userQuota.previousEstimate') }}</p>
        <div class="overflow-x-auto">
          <table class="w-full text-left text-sm" data-testid="quota-members">
            <thead><tr><th v-for="column in ['user', 'quota', 'used', 'reserved', 'available']" :key="column" class="p-2">{{ t(`admin.accounts.userQuota.${column}`) }}</th></tr></thead>
            <tbody><tr v-for="member in snapshot.members" :key="member.user_id" class="border-t border-gray-100 dark:border-dark-700">
              <td class="p-2">{{ userName(member.user_id) }}</td><td class="p-2">{{ money(member.quota) }}</td><td class="p-2">{{ money(member.actual_cost) }}</td><td class="p-2">{{ money(member.reserved_cost) }}</td><td class="p-2">{{ money(member.available) }}</td>
            </tr></tbody>
          </table>
        </div>
        <p class="input-hint">{{ t('admin.accounts.userQuota.settlementHint') }}</p>
      </div>

      <details v-if="snapshot.policy?.enabled && snapshot.cycle" class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
        <summary class="cursor-pointer font-medium">{{ t('admin.accounts.userQuota.initialize') }}</summary>
        <p class="input-hint">{{ t('admin.accounts.userQuota.historyHint') }}</p>
        <label class="flex items-center gap-2 text-sm"><input v-model="historyVerified" type="checkbox" :disabled="busy" />{{ t('admin.accounts.userQuota.historyVerified') }}</label>
        <textarea v-model="historyNote" class="input" minlength="8" maxlength="1000" :placeholder="t('admin.accounts.userQuota.historyNote')" :disabled="busy" />
        <label class="flex items-center gap-2 text-sm"><input v-model="paid" type="checkbox" :disabled="busy" data-testid="quota-paid" />{{ t('admin.accounts.userQuota.paid') }}</label>
        <div v-if="paid" class="space-y-3 rounded-lg bg-amber-50 p-3 dark:bg-amber-900/20">
          <p class="text-sm">{{ t('admin.accounts.userQuota.paidHint') }}</p>
          <label class="input-label">{{ t('admin.accounts.userQuota.chargedUser') }}
            <select v-model="calibrationUser" class="input" :disabled="busy" @change="loadCalibrationKeys">
              <option :value="0">{{ t('admin.accounts.userQuota.choose') }}</option><option v-for="id in snapshot.policy.member_user_ids" :key="id" :value="id">{{ userName(id) }}</option>
            </select>
          </label>
          <label class="input-label">{{ t('admin.accounts.userQuota.chargedKey') }}
            <select v-model="calibrationKey" class="input" :disabled="busy">
              <option :value="0">{{ t('admin.accounts.userQuota.choose') }}</option><option v-for="key in calibrationKeys" :key="key.id" :value="key.id">{{ key.name }} · {{ groupName(key.group_id ?? 0) }}</option>
            </select>
          </label>
          <label class="input-label">{{ t('admin.accounts.userQuota.budget') }}<input v-model.number="budget" type="number" class="input" min="0.00000001" step="0.00000001" :disabled="busy" /></label>
          <label class="input-label">{{ t('admin.accounts.userQuota.maxRequests') }}<input v-model.number="maxRequests" type="number" min="1" max="3" class="input" :disabled="busy" /></label>
          <label class="input-label">{{ t('admin.accounts.userQuota.maxOutput') }}<input v-model.number="maxOutput" type="number" min="1" max="256" class="input" :disabled="busy" /></label>
        </div>
        <button type="button" class="btn btn-secondary" :disabled="busy || !historyVerified" data-testid="quota-initialize" @click="initialize">{{ paid ? t('admin.accounts.userQuota.initializePaid') : t('admin.accounts.userQuota.initializeFree') }}</button>
        <p v-if="calibrationResult" role="status" class="text-sm">{{ t('admin.accounts.userQuota.calibrationResult', { count: calibrationResult.request_count, cost: money(calibrationResult.charged_cost) }) }} {{ t(`admin.accounts.userQuota.stops.${calibrationResult.stop_reason}`) }}</p>
      </details>

      <details v-if="snapshot.policy" class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" @toggle="onReservationsToggle">
        <summary class="cursor-pointer font-medium">{{ t('admin.accounts.userQuota.reservations') }} ({{ snapshot.uncertain_reservations }})</summary>
        <p class="input-hint">{{ t('admin.accounts.userQuota.reconcileHint') }}</p>
        <div v-for="reservation in reservations" :key="reservation.token" class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
          <p class="text-sm">{{ userName(reservation.user_id) }} · {{ money(reservation.reserved_cost) }} · {{ t(`admin.accounts.userQuota.reservationStates.${reservation.state}`) }} · {{ date(reservation.created_at) }}</p>
          <button v-if="reservation.state === 'uncertain' || new Date(reservation.lease_until).getTime() <= Date.now()" type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="reconcileToken = reservation.token">{{ t('admin.accounts.userQuota.reconcile') }}</button>
          <div v-if="reconcileToken === reservation.token" class="space-y-2">
            <textarea v-model="evidence" class="input" minlength="8" maxlength="450" :placeholder="t('admin.accounts.userQuota.evidence')" />
            <textarea v-model="reconcileNote" class="input" minlength="8" maxlength="450" :placeholder="t('admin.accounts.userQuota.reconcileNote')" />
            <label class="flex items-center gap-2 text-sm"><input v-model="noCharge" type="checkbox" />{{ t('admin.accounts.userQuota.noCharge') }}</label>
            <button type="button" class="btn btn-secondary" :disabled="busy || !noCharge" @click="reconcile">{{ t('admin.accounts.userQuota.confirmReconcile') }}</button>
          </div>
        </div>
      </details>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { accountUserQuotaAPI } from '@/api/admin/accountUserQuota'
import { adminAPI } from '@/api/admin'
import type { AdminGroup, AdminUser, ApiKey } from '@/types'
import type { AccountUserQuotaSnapshot, AccountUserQuotaReservation, AccountUserQuotaInitializationResult, InitializeAccountUserQuotaInput } from '@/types/accountUserQuota'

const props = defineProps<{ accountId: number; groups: AdminGroup[] }>()
const { t, locale } = useI18n()
const snapshot = ref<AccountUserQuotaSnapshot | null>(null)
const loading = ref(false), busy = ref(false), searching = ref(false)
const error = ref(''), message = ref(''), enabled = ref(false), version = ref(0)
const memberIds = ref<number[]>([]), fallbackIds = ref<number[]>([]), carryoverMinutes = ref(30), newFallback = ref(0)
const search = ref(''), searchResults = ref<AdminUser[]>([]), users = ref<Record<number, AdminUser>>({})
const historyVerified = ref(false), historyNote = ref(''), paid = ref(false)
const calibrationUser = ref(0), calibrationKey = ref(0), calibrationKeys = ref<ApiKey[]>([])
const budget = ref<number | ''>(''), maxRequests = ref(1), maxOutput = ref(32)
const calibrationResult = ref<AccountUserQuotaInitializationResult['calibration']>()
const reservations = ref<AccountUserQuotaReservation[]>([]), reconcileToken = ref(''), evidence = ref(''), reconcileNote = ref(''), noCharge = ref(false)
let timer: ReturnType<typeof setInterval> | undefined
let initializationAttempt: { body: string; key: string } | undefined
let active = true

const fallbackOptions = computed(() => props.groups.filter(g => g.platform === 'openai' && g.status === 'active' && !fallbackIds.value.includes(g.id)))
const userName = (id: number) => users.value[id]?.username || users.value[id]?.email || `#${id}`
const groupName = (id: number) => props.groups.find(g => g.id === id)?.name || `#${id}`
const money = (value?: number | null) => value == null ? '—' : new Intl.NumberFormat(locale.value, { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 8 }).format(value)
const date = (value?: string | null) => value ? new Date(value).toLocaleString(locale.value) : '—'
const errorMessage = (err: unknown) => {
  const response = err as { response?: { data?: { message?: string } }; message?: string }
  return response.response?.data?.message || response.message || t('admin.accounts.userQuota.failed')
}

async function load(resetForm = false) {
  if (loading.value || busy.value) return
  loading.value = true
  try {
    const state = await accountUserQuotaAPI.get(props.accountId)
    if (!active) return
    snapshot.value = state
    if (resetForm) {
      error.value = ''
      enabled.value = state.policy?.enabled ?? false
      version.value = state.policy?.version ?? 0
      memberIds.value = [...(state.policy?.member_user_ids ?? [])]
      fallbackIds.value = [...(state.policy?.fallback_group_ids ?? [])]
      carryoverMinutes.value = (state.policy?.carryover_seconds ?? 1800) / 60
      historyNote.value = state.policy?.history_note ?? ''
    }
    for (const id of state.policy?.member_user_ids ?? []) {
      if (!users.value[id]) users.value[id] = await adminAPI.users.getById(id)
    }
  } catch (err) { error.value = errorMessage(err) } finally { loading.value = false }
}

async function searchUsers() {
  if (searching.value) return
  searching.value = true
  try {
    const result = await adminAPI.users.list(1, 20, { status: 'active', search: search.value })
    searchResults.value = result.items
    for (const user of result.items) users.value[user.id] = user
  } catch (err) { error.value = errorMessage(err) } finally { searching.value = false }
}

function addFallback() {
  if (newFallback.value && !fallbackIds.value.includes(newFallback.value)) fallbackIds.value.push(newFallback.value)
  newFallback.value = 0
}
function moveFallback(index: number) {
  [fallbackIds.value[index - 1], fallbackIds.value[index]] = [fallbackIds.value[index]!, fallbackIds.value[index - 1]!]
}

async function save() {
  error.value = ''; message.value = ''
  if ((enabled.value && memberIds.value.length === 0) || !Number.isInteger(carryoverMinutes.value) || carryoverMinutes.value < 10 || carryoverMinutes.value > 1440) {
    error.value = t('admin.accounts.userQuota.invalidConfig'); return
  }
  busy.value = true
  try {
    snapshot.value = await accountUserQuotaAPI.save(props.accountId, { enabled: enabled.value, expected_version: version.value,
      member_user_ids: memberIds.value, fallback_group_ids: fallbackIds.value, carryover_seconds: carryoverMinutes.value * 60 })
    version.value = snapshot.value.policy!.version
    message.value = t('admin.accounts.userQuota.saved')
  } catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}

async function loadCalibrationKeys() {
  calibrationKey.value = 0; calibrationKeys.value = []
  const userId = calibrationUser.value
  if (!userId) return
  try {
    const result = await adminAPI.users.getUserApiKeys(userId)
    if (userId !== calibrationUser.value) return
    calibrationKeys.value = result.items.filter(key => key.status === 'active' && key.group_id && props.groups.some(g => g.id === key.group_id && g.platform === 'openai'))
  } catch (err) { error.value = errorMessage(err) }
}

async function initialize() {
  error.value = ''; message.value = ''
  if (!snapshot.value?.cycle || !historyVerified.value || [...historyNote.value.trim()].length < 8) {
    error.value = t('admin.accounts.userQuota.invalidHistory'); return
  }
  const input: InitializeAccountUserQuotaInput = { expected_version: version.value, cycle_id: snapshot.value.cycle.id,
    history_verified: true, history_note: historyNote.value.trim() }
  if (paid.value) {
    const key = calibrationKeys.value.find(key => key.id === calibrationKey.value)
    if (!key?.group_id || !Number.isFinite(budget.value) || Number(budget.value) <= 0 || !Number.isInteger(maxRequests.value) || maxRequests.value < 1 || maxRequests.value > 3 || !Number.isInteger(maxOutput.value) || maxOutput.value < 1 || maxOutput.value > 256) {
      error.value = t('admin.accounts.userQuota.invalidCalibration'); return
    }
    input.calibration = { authorize_paid: true, user_id: calibrationUser.value, group_id: key.group_id, api_key_id: key.id,
      total_budget: Number(budget.value), max_requests: maxRequests.value, max_output_tokens: maxOutput.value }
  }
  const body = JSON.stringify(input)
  if (initializationAttempt?.body !== body) initializationAttempt = { body, key: crypto.randomUUID() }
  busy.value = true
  try {
    const result = await accountUserQuotaAPI.initialize(props.accountId, input, initializationAttempt.key)
    snapshot.value = result.snapshot; calibrationResult.value = result.calibration
    message.value = t('admin.accounts.userQuota.initialized')
  } catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}

async function onReservationsToggle(event: Event) {
  if (!(event.target as HTMLDetailsElement).open) return
  try { reservations.value = await accountUserQuotaAPI.reservations(props.accountId) } catch (err) { error.value = errorMessage(err) }
}
async function reconcile() {
  error.value = ''
  if (!noCharge.value || [...evidence.value.trim()].length < 8 || [...reconcileNote.value.trim()].length < 8) {
    error.value = t('admin.accounts.userQuota.invalidEvidence'); return
  }
  busy.value = true
  try {
    await accountUserQuotaAPI.reconcile(props.accountId, reconcileToken.value, evidence.value.trim(), reconcileNote.value.trim())
    reservations.value = await accountUserQuotaAPI.reservations(props.accountId)
    reconcileToken.value = ''; evidence.value = ''; reconcileNote.value = ''; noCharge.value = false
    message.value = t('admin.accounts.userQuota.reconciled')
  } catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
  await load()
}

onMounted(() => { void load(true); timer = setInterval(() => void load(), 30000) })
onUnmounted(() => { active = false; clearInterval(timer) })
</script>
