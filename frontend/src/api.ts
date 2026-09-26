export class ApiError extends Error {
  constructor(public status: number, message: string, public body: Record<string, unknown> = {}) { super(message) }
}
let csrfToken = ''
export const setCSRF = (value: string) => { csrfToken = value }
export const getCSRF = () => csrfToken

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  if (options.method && !['GET', 'HEAD'].includes(options.method)) headers.set('X-CSRF-Token', csrfToken)
  const response = await fetch(`/api${path}`, { ...options, headers, credentials: 'same-origin' })
  if (response.status === 204) return undefined as T
  const body = await response.json().catch(() => ({ error: `服务暂时不可用 (${response.status})` }))
  if (!response.ok) throw new ApiError(response.status, body.error || '请求失败', body)
  return body as T
}
export function bytes(n: number) { if (!Number.isFinite(n) || n <= 0) return '0 B'; const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), 4); return `${(n / 1024 ** i).toFixed(i === 0 ? 0 : 1)} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][i]}` }
export function duration(seconds: number) { if (!Number.isFinite(seconds) || seconds <= 0) return '—'; if (seconds < 60) return `${Math.ceil(seconds)} 秒`; if (seconds < 3600) return `${Math.ceil(seconds / 60)} 分钟`; return `${(seconds / 3600).toFixed(1)} 小时` }
export const date = (v: string | null) => v ? new Date(v).toLocaleString('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '永久'
export const message = (e: unknown) => e instanceof Error ? e.message : '操作失败，请重试'

export interface User { id: string; username: string; role: 'admin' | 'user' }
export interface Settings { appUrl: string; chunkSize: number; uploadConcurrency: number; uploadExpireHours: number; maxFileSize: number; reservedBytes: number; maxUploadsPerUser: number }
export interface FileItem { id: string; name: string; size: number; mimeType: string; sha256: string; createdAt: string; shareCount: number; status: string; retainOwner: boolean; deleteAt: string | null }
export interface FileList { files: FileItem[]; total: number; page: number; pageSize: number }
export interface Stats { totalStorage: number; usedStorage: number; availableStorage: number; reservedStorage: number; uploadReservations: number; myFileBytes: number; fileCount: number; activeUploads: number; todayUploaded: number; todayDownloaded: number }
