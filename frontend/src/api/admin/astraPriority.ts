import { apiClient } from '../client'

export interface AstraPrioritySettings { enabled: boolean }

export const astraPriorityAPI = {
  async get(signal?: AbortSignal): Promise<AstraPrioritySettings> {
    return (await apiClient.get<AstraPrioritySettings>('/admin/accounts/astra-priority', { signal })).data
  },
  async save(enabled: boolean): Promise<AstraPrioritySettings> {
    return (await apiClient.put<AstraPrioritySettings>('/admin/accounts/astra-priority', { enabled })).data
  }
}
