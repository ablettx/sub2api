import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUserQuotaPanel from '../AccountUserQuotaPanel.vue'
import { accountUserQuotaAPI } from '@/api/admin/accountUserQuota'
import type { AccountUserQuotaSnapshot } from '@/types/accountUserQuota'

vi.mock('@/api/admin/accountUserQuota', () => ({ accountUserQuotaAPI: { get: vi.fn(), save: vi.fn(), initialize: vi.fn(), reservations: vi.fn(), reconcile: vi.fn() } }))
vi.mock('@/api/admin', () => ({ adminAPI: { users: {
  getById: vi.fn(async (id: number) => ({ id, username: `User ${id}` })),
  list: vi.fn(async () => ({ items: [{ id: 4, username: 'User 4' }] })), getUserApiKeys: vi.fn(async () => ({ items: [] }))
} } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh-CN' } }) }))

const state = (): AccountUserQuotaSnapshot => ({
  policy: { account_id: 7, enabled: true, version: 3, member_user_ids: [1, 2, 3], fallback_group_ids: [], carryover_seconds: 1800, history_note: '' },
  cycle: { id: 9, starts_at: '2026-10-15T02:00:00Z', resets_at: '2026-10-22T02:00:00Z', ready: true, version: 1, account_stats_cost: 300,
    estimate: 1500, estimate_at: '2026-10-18T02:00:00Z', estimate_source_cycle_id: 9, observed_at: '2026-10-18T02:00:00Z', used_percent: 20, missing_since: null },
  estimate_state: 'current', members: [{ user_id: 1, quota: 500, actual_cost: 450, reserved_cost: 8, remaining: 50, available: 42 }], uncertain_reservations: 0
})
const wrappers: ReturnType<typeof mount>[] = []
const open = async () => {
  const wrapper = mount(AccountUserQuotaPanel, { props: { accountId: 7, groups: [] } })
  wrappers.push(wrapper); await flushPromises(); return wrapper
}
beforeEach(() => {
  vi.useFakeTimers(); vi.clearAllMocks()
  vi.mocked(accountUserQuotaAPI.get).mockResolvedValue(state())
  vi.mocked(accountUserQuotaAPI.save).mockImplementation(async (_id, input) => ({ ...state(), policy: { ...state().policy!, ...input, version: 4 } }))
  vi.mocked(accountUserQuotaAPI.initialize).mockResolvedValue({ snapshot: state() })
})
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()); vi.useRealTimers() })

describe('AccountUserQuotaPanel', () => {
  it('displays actual charges and in-flight availability from the server', async () => {
    const wrapper = await open()
    const row = wrapper.get('[data-testid="quota-members"]').text()
    expect(row).toContain('450.00'); expect(row).toContain('500.00'); expect(row).toContain('8.00'); expect(row).toContain('42.00')
    expect(accountUserQuotaAPI.initialize).not.toHaveBeenCalled()
  })

  it('saves independently with the expected version and retains historical charges', async () => {
    const wrapper = await open()
    await wrapper.get('[data-testid="quota-enabled"]').setValue(false)
    await wrapper.get('[data-testid="quota-save"]').trigger('click'); await flushPromises()
    expect(accountUserQuotaAPI.save).toHaveBeenCalledWith(7, { enabled: false, expected_version: 3, member_user_ids: [1, 2, 3], fallback_group_ids: [], carryover_seconds: 1800 })
    expect(wrapper.get('[data-testid="quota-members"]').text()).toContain('450.00')
  })

  it('refreshes a lower estimate without discarding a pending configuration or changing its expected version', async () => {
    const wrapper = await open()
    await wrapper.get('[data-testid="quota-enabled"]').setValue(false)
    const lower = state(); lower.policy!.version = 4; lower.cycle!.estimate = 1200
    lower.members[0] = { ...lower.members[0]!, quota: 400, remaining: 0, available: 0 }
    vi.mocked(accountUserQuotaAPI.get).mockResolvedValue(lower)
    await vi.advanceTimersByTimeAsync(30000); await flushPromises()
    expect(wrapper.get('[data-testid="quota-members"]').text()).toContain('400.00')
    expect(wrapper.get('[data-testid="quota-members"]').text()).toContain('450.00')
    expect((wrapper.get('[data-testid="quota-enabled"]').element as HTMLInputElement).checked).toBe(false)
    vi.mocked(accountUserQuotaAPI.save).mockRejectedValue({ response: { data: { message: 'configuration conflict' } } })
    await wrapper.get('[data-testid="quota-save"]').trigger('click'); await flushPromises()
    expect(vi.mocked(accountUserQuotaAPI.save).mock.calls[0]?.[1].expected_version).toBe(3)
    expect(wrapper.get('[role="alert"]').text()).toContain('configuration conflict')
  })

  it('imports history without paid calibration and keeps the same idempotency key on retry', async () => {
    const wrapper = await open()
    const details = wrapper.findAll('details')[0]!
    await details.find('input[type="checkbox"]').setValue(true)
    await details.get('textarea').setValue('已核对当前账号周期内全部实际扣费收据')
    await wrapper.get('[data-testid="quota-initialize"]').trigger('click'); await flushPromises()
    await wrapper.get('[data-testid="quota-initialize"]').trigger('click'); await flushPromises()
    const calls = vi.mocked(accountUserQuotaAPI.initialize).mock.calls
    expect(calls).toHaveLength(2); expect(calls[0]![1].calibration).toBeUndefined(); expect(calls[0]![2]).toBe(calls[1]![2])
    expect(calls[0]![1]).toMatchObject({ expected_version: 3, cycle_id: 9, history_verified: true })
  })

  it('requires an explicit paying identity and positive budget for paid calibration', async () => {
    const wrapper = await open()
    const details = wrapper.findAll('details')[0]!
    await details.find('input[type="checkbox"]').setValue(true)
    await details.get('textarea').setValue('已核对当前账号周期内全部实际扣费收据')
    await wrapper.get('[data-testid="quota-paid"]').setValue(true)
    await wrapper.get('[data-testid="quota-initialize"]').trigger('click'); await flushPromises()
    expect(accountUserQuotaAPI.initialize).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('invalidCalibration')
  })
})
