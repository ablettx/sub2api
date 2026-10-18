export interface AccountUserQuotaPolicy {
  account_id: number
  enabled: boolean
  version: number
  member_user_ids: number[]
  fallback_group_ids: number[]
  carryover_seconds: number
  history_note: string
  history_verified_at?: string | null
}

export interface AccountUserQuotaMember {
  user_id: number
  quota: number
  actual_cost: number
  reserved_cost: number
  remaining: number
  available: number
}

export interface AccountUserQuotaCycle {
  id: number
  starts_at: string
  resets_at: string
  ready: boolean
  version: number
  account_stats_cost: number
  estimate: number | null
  estimate_at: string | null
  estimate_source_cycle_id: number | null
  observed_at: string | null
  used_percent: number | null
  missing_since: string | null
}

export interface AccountUserQuotaSnapshot {
  policy: AccountUserQuotaPolicy | null
  cycle: AccountUserQuotaCycle | null
  estimate_state: 'current' | 'carryover' | 'initializing' | 'unavailable'
  expires_at?: string | null
  members: AccountUserQuotaMember[]
  uncertain_reservations: number
}

export interface SaveAccountUserQuotaInput {
  enabled: boolean
  expected_version: number
  member_user_ids: number[]
  fallback_group_ids: number[]
  carryover_seconds: number
}

export interface InitializeAccountUserQuotaInput {
  expected_version: number
  cycle_id: number
  history_verified: boolean
  history_note: string
  calibration?: {
    authorize_paid: boolean
    user_id: number
    group_id: number
    api_key_id: number
    total_budget: number
    max_requests: number
    max_output_tokens: number
  }
}

export interface AccountUserQuotaInitializationResult {
  snapshot: AccountUserQuotaSnapshot
  calibration?: {
    run_id: string
    budget: number
    request_count: number
    charged_cost: number
    pending_reconciliation: boolean
    stop_reason: string
  }
}

export interface AccountUserQuotaReservation {
  token: string
  user_id: number
  api_key_id: number
  cycle_id: number
  reserved_cost: number
  state: 'reserved' | 'sent' | 'settled' | 'released' | 'uncertain'
  lease_until: string
  created_at: string
}
