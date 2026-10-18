import { apiClient } from '../client'
import type {
  AccountUserQuotaSnapshot, SaveAccountUserQuotaInput, InitializeAccountUserQuotaInput,
  AccountUserQuotaInitializationResult, AccountUserQuotaReservation
} from '@/types/accountUserQuota'

const path = (accountId: number) => `/admin/accounts/${accountId}/user-quota`

export const accountUserQuotaAPI = {
  async get(accountId: number): Promise<AccountUserQuotaSnapshot> {
    return (await apiClient.get<AccountUserQuotaSnapshot>(path(accountId))).data
  },
  async save(accountId: number, input: SaveAccountUserQuotaInput): Promise<AccountUserQuotaSnapshot> {
    return (await apiClient.put<AccountUserQuotaSnapshot>(path(accountId), input)).data
  },
  async initialize(accountId: number, input: InitializeAccountUserQuotaInput, idempotencyKey: string): Promise<AccountUserQuotaInitializationResult> {
    return (await apiClient.post<AccountUserQuotaInitializationResult>(`${path(accountId)}/initialize`, input, {
      headers: { 'Idempotency-Key': idempotencyKey }, timeout: 125000
    })).data
  },
  async reservations(accountId: number): Promise<AccountUserQuotaReservation[]> {
    return (await apiClient.get<AccountUserQuotaReservation[]>(`${path(accountId)}/reservations`)).data
  },
  async reconcile(accountId: number, token: string, evidence: string, note: string): Promise<void> {
    await apiClient.post(`${path(accountId)}/reservations/${encodeURIComponent(token)}/reconcile`, {
      confirmed_no_charge: true, evidence, note
    })
  }
}
