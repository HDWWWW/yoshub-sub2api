import { apiClient } from '../client'

export interface AccountExpiryGrace {
  minutes: number
  effective_expires_at: string | null
}

export const accountExpiryGraceAPI = {
  async get(id: number) {
    return (await apiClient.get<AccountExpiryGrace>(`/admin/accounts/${id}/expiry-grace`)).data
  },
  async save(id: number, minutes: number) {
    return (await apiClient.put<AccountExpiryGrace>(`/admin/accounts/${id}/expiry-grace`, { minutes })).data
  }
}
