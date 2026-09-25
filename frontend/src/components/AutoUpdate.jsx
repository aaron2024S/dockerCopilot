import React, { useEffect, useRef, useState } from 'react'
import {
  X,
  RefreshCw,
  Zap,
  Save,
  Shield,
  Trash2,
  Plus,
  Package,
  AlertCircle,
  CheckCircle2,
  Clock,
  Lock
} from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { autoUpdateAPI } from '../api/client.js'
import { cn } from '../utils/cn.js'

// 执行来源的中文名
const TRIGGER_LABELS = {
  cron: '定时任务',
  manual: '手动执行',
}

// 把后端返回的 RFC3339 时间转成本地可读格式
function formatTime(value) {
  if (!value) return '尚未执行'
  const date = new Date(value)
  if (isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false }).replace(/\//g, '-')
}

// 开关组件
function Switch({ checked, onChange, disabled = false, title }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={title}
      disabled={disabled}
      onClick={() => !disabled && onChange(!checked)}
      className={cn(
        'relative inline-flex h-6 w-11 flex-shrink-0 items-center rounded-full transition-colors duration-200',
        'focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 dark:focus:ring-offset-gray-800',
        checked ? 'bg-primary-600 dark:bg-primary-500' : 'bg-gray-300 dark:bg-gray-600',
        disabled && 'opacity-50 cursor-not-allowed'
      )}
    >
      <span
        className={cn(
          'inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform duration-200',
          checked ? 'translate-x-6' : 'translate-x-1'
        )}
      />
    </button>
  )
}

// 单项开关行
function SettingRow({ icon: Icon, title, description, checked, onChange, disabled, tone = 'primary' }) {
  const toneClass = {
    primary: 'bg-primary-50 text-primary-600 dark:bg-primary-900/30 dark:text-primary-400',
    amber: 'bg-amber-50 text-amber-600 dark:bg-amber-900/30 dark:text-amber-400',
    blue: 'bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-400',
  }[tone]

  return (
    <div className="flex items-start justify-between gap-4 py-3">
      <div className="flex items-start gap-3 flex-1 min-w-0">
        {Icon && (
          <div className={cn('flex-shrink-0 w-9 h-9 rounded-lg flex items-center justify-center', toneClass)}>
            <Icon className="h-4 w-4" />
          </div>
        )}
        <div className="min-w-0">
          <div className="text-sm font-medium text-gray-900 dark:text-white">{title}</div>
          <div className="text-xs text-gray-500 dark:text-gray-400 mt-0.5 leading-relaxed">{description}</div>
        </div>
      </div>
      <Switch checked={checked} onChange={onChange} disabled={disabled} title={title} />
    </div>
  )
}

export function AutoUpdate({ onClose }) {
  // 表单态：三个开关（后端是值类型，必须全量提交）
  const [form, setForm] = useState(null)
  // 排除列表（本地可编辑副本）
  const [excludeList, setExcludeList] = useState([])
  const [manualInput, setManualInput] = useState('')
  const [saving, setSaving] = useState(false)
  const [running, setRunning] = useState(false)
  const [message, setMessage] = useState(null)

  // 用户有未保存改动时，不能被后台轮询回来的配置覆盖
  const dirtyRef = useRef(false)
  const [dirty, setDirty] = useState(false)
  const markDirty = () => {
    dirtyRef.current = true
    setDirty(true)
  }

  // 配置
  const {
    data: setting,
    isLoading: settingLoading,
    refetch: refetchSetting,
  } = useQuery({
    queryKey: ['autoUpdateSetting'],
    queryFn: async () => {
      const response = await autoUpdateAPI.getSetting()
      if (response.data.code === 200 || response.data.code === 0) {
        return response.data.data || {}
      }
      throw new Error(response.data.msg || '获取自动更新配置失败')
    },
  })

  // 候选容器（真实容器列表 + 筛选结论）
  const {
    data: candidatesData,
    isLoading: candidatesLoading,
    refetch: refetchCandidates,
  } = useQuery({
    queryKey: ['autoUpdateCandidates'],
    queryFn: async () => {
      const response = await autoUpdateAPI.getCandidates()
      if (response.data.code === 200 || response.data.code === 0) {
        return response.data.data || { candidates: [] }
      }
      throw new Error(response.data.msg || '获取候选容器失败')
    },
  })

  const candidates = candidatesData?.candidates || []
  const willUpdateCount = candidatesData?.willUpdate ?? candidates.filter(c => c.willUpdate).length
  // running 同时来自配置接口与候选接口，任一为真即视为执行中
  const isRunning = running || !!setting?.running || !!candidatesData?.running

  // 首次加载/保存后同步表单；有未保存改动时跳过，避免覆盖用户正在编辑的内容
  useEffect(() => {
    if (!setting || dirtyRef.current) return
    setForm({
      enabled: !!setting.enabled,
      deleteOldImage: !!setting.deleteOldImage,
      protectSelf: !!setting.protectSelf,
    })
    setExcludeList(Array.isArray(setting.excludeList) ? setting.excludeList : [])
  }, [setting])

  // 执行中时轮询刷新，无需用户手动点刷新
  useEffect(() => {
    if (!isRunning) return
    const timer = setInterval(() => {
      refetchSetting()
      refetchCandidates()
    }, 3000)
    return () => clearInterval(timer)
  }, [isRunning, refetchSetting, refetchCandidates])

  // 执行结束后自动收尾刷新一次
  const prevRunningRef = useRef(false)
  useEffect(() => {
    if (prevRunningRef.current && !isRunning) {
      setRunning(false)
      refetchSetting()
      refetchCandidates()
    }
    prevRunningRef.current = isRunning
  }, [isRunning, refetchSetting, refetchCandidates])

  const updateForm = (key, value) => {
    markDirty()
    setForm(prev => ({ ...(prev || {}), [key]: value }))
    setMessage(null)
  }

  const isExcludedName = (name) =>
    excludeList.some(item => item.toLowerCase() === String(name).toLowerCase())

  const toggleExclude = (name) => {
    markDirty()
    setMessage(null)
    setExcludeList(prev =>
      prev.some(item => item.toLowerCase() === name.toLowerCase())
        ? prev.filter(item => item.toLowerCase() !== name.toLowerCase())
        : [...prev, name]
    )
  }

  const addManualExclude = () => {
    const value = manualInput.trim()
    if (!value) return
    if (isExcludedName(value)) {
      setManualInput('')
      return
    }
    markDirty()
    setMessage(null)
    setExcludeList(prev => [...prev, value])
    setManualInput('')
  }

  const removeExclude = (value) => {
    markDirty()
    setMessage(null)
    setExcludeList(prev => prev.filter(item => item !== value))
  }

  const handleSave = async () => {
    if (!form) return
    setSaving(true)
    setMessage(null)
    try {
      const response = await autoUpdateAPI.updateSetting({
        enabled: !!form.enabled,
        deleteOldImage: !!form.deleteOldImage,
        protectSelf: !!form.protectSelf,
        excludeList,
      })
      if (response.data.code === 200 || response.data.code === 0) {
        dirtyRef.current = false
        setDirty(false)
        setMessage({ type: 'success', text: '配置已保存并生效' })
        await refetchSetting()
        await refetchCandidates()
      } else {
        throw new Error(response.data.msg || '保存失败')
      }
    } catch (error) {
      setMessage({
        type: 'error',
        text: '保存失败: ' + (error.response?.data?.msg || error.message || '未知错误'),
      })
    } finally {
      setSaving(false)
    }
  }

  const handleRunNow = async () => {
    if (dirty) {
      setMessage({ type: 'error', text: '有未保存的改动，请先保存再执行' })
      return
    }
    setRunning(true)
    setMessage(null)
    try {
      const response = await autoUpdateAPI.run()
      if (response.data.code === 200 || response.data.code === 0) {
        setMessage({ type: 'success', text: '已开始执行，下方进度会自动刷新' })
        refetchSetting()
      } else {
        // 409 表示上一轮还在跑
        setRunning(false)
        setMessage({ type: 'error', text: response.data.msg || '触发失败' })
      }
    } catch (error) {
      setRunning(false)
      setMessage({
        type: 'error',
        text: '触发失败: ' + (error.response?.data?.msg || error.message || '未知错误'),
      })
    }
  }

  const lastResult = Array.isArray(setting?.lastResult) ? setting.lastResult : []

  return (
    <div className="fixed inset-0 bg-black bg-opacity-50 flex items-center justify-center z-50 p-2 sm:p-4">
      <div className="bg-white dark:bg-gray-800 rounded-2xl shadow-xl w-full max-w-3xl max-h-[92vh] flex flex-col overflow-hidden">
        {/* 头部 */}
        <div className="px-5 py-4 border-b border-gray-200 dark:border-gray-700 flex items-start justify-between gap-3 flex-shrink-0">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <div className="w-9 h-9 rounded-lg bg-amber-50 dark:bg-amber-900/30 flex items-center justify-center flex-shrink-0">
                <Zap className="h-5 w-5 text-amber-600 dark:text-amber-400" />
              </div>
              <div className="min-w-0">
                <h3 className="text-lg font-semibold text-gray-900 dark:text-white">自动更新</h3>
                <p className="text-xs text-gray-500 dark:text-gray-400">
                  每小时第 30 分钟检测一次，检测到更新就自动更新
                </p>
              </div>
            </div>
          </div>
          <button
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors flex-shrink-0"
            title="关闭"
          >
            <X className="h-5 w-5" />
          </button>
        </div>

        {/* 内容 */}
        <div className="flex-1 overflow-y-auto px-5 py-4 space-y-5">
          {settingLoading && !form ? (
            <div className="animate-pulse space-y-3">
              {[1, 2, 3].map(i => (
                <div key={i} className="h-14 bg-gray-100 dark:bg-gray-700/50 rounded-xl" />
              ))}
            </div>
          ) : (
            <>
              {/* 提示信息 */}
              {message && (
                <div
                  className={cn(
                    'flex items-start gap-2 px-3 py-2.5 rounded-lg text-sm border',
                    message.type === 'success'
                      ? 'bg-green-50 dark:bg-green-900/20 border-green-200 dark:border-green-800 text-green-700 dark:text-green-300'
                      : 'bg-red-50 dark:bg-red-900/20 border-red-200 dark:border-red-800 text-red-700 dark:text-red-300'
                  )}
                >
                  {message.type === 'success' ? (
                    <CheckCircle2 className="h-4 w-4 flex-shrink-0 mt-0.5" />
                  ) : (
                    <AlertCircle className="h-4 w-4 flex-shrink-0 mt-0.5" />
                  )}
                  <span>{message.text}</span>
                </div>
              )}

              {/* 执行中提示 */}
              {isRunning && (
                <div className="flex items-center gap-2 px-3 py-2.5 rounded-lg text-sm bg-blue-50 dark:bg-blue-900/20 border border-blue-200 dark:border-blue-800 text-blue-700 dark:text-blue-300">
                  <RefreshCw className="h-4 w-4 animate-spin flex-shrink-0" />
                  <span>正在执行自动更新，请勿关闭页面…</span>
                </div>
              )}

              {/* 开关区 */}
              <div className="card p-4 divide-y divide-gray-100 dark:divide-gray-700/60">
                <div className="pb-1">
                  <SettingRow
                    icon={Zap}
                    tone="amber"
                    title="自动更新总开关"
                    description="开启后每小时第 30 分钟检测，检测到更新就自动更新。关闭时仍会正常检测并标记，只是不动容器。"
                    checked={!!form?.enabled}
                    onChange={(value) => updateForm('enabled', value)}
                  />
                </div>
                <SettingRow
                  icon={Trash2}
                  tone="blue"
                  title="更新后删除旧镜像"
                  description="确认旧镜像已无其他容器引用后再删除。多容器共享同一镜像时会自动跳过，不会误删。"
                  checked={!!form?.deleteOldImage}
                  onChange={(value) => updateForm('deleteOldImage', value)}
                />
                <SettingRow
                  icon={Shield}
                  tone="primary"
                  title="保护 Docker Copilot 自身"
                  description="永不自动更新本工具容器，避免更新过程中把自己重启导致任务中断。建议保持开启。"
                  checked={!!form?.protectSelf}
                  onChange={(value) => updateForm('protectSelf', value)}
                />
              </div>

              {/* 排除列表 */}
              <div>
                <div className="flex items-center justify-between mb-2">
                  <div>
                    <h4 className="text-sm font-semibold text-gray-900 dark:text-white">排除列表</h4>
                    <p className="text-xs text-gray-500 dark:text-gray-400 mt-0.5">
                      勾选不需要自动更新的容器；也可手动添加容器名或镜像名，支持 redis* 通配
                    </p>
                  </div>
                  <span className="badge-info flex-shrink-0">{excludeList.length} 项</span>
                </div>

                {/* 已排除项 */}
                {excludeList.length > 0 && (
                  <div className="flex flex-wrap gap-1.5 mb-3">
                    {excludeList.map((item) => (
                      <span
                        key={item}
                        className="inline-flex items-center gap-1 pl-2.5 pr-1 py-1 rounded-lg text-xs font-medium bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-200"
                      >
                        <span className="max-w-[14rem] truncate">{item}</span>
                        <button
                          onClick={() => removeExclude(item)}
                          className="p-0.5 rounded hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors"
                          title="移除"
                        >
                          <X className="h-3 w-3" />
                        </button>
                      </span>
                    ))}
                  </div>
                )}

                {/* 手动添加 */}
                <div className="flex gap-2 mb-3">
                  <input
                    type="text"
                    value={manualInput}
                    onChange={(e) => setManualInput(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault()
                        addManualExclude()
                      }
                    }}
                    className="input flex-1"
                    placeholder="例如 mysql 或 redis*"
                  />
                  <button
                    onClick={addManualExclude}
                    disabled={!manualInput.trim()}
                    className={cn(
                      'btn-secondary flex items-center gap-1 flex-shrink-0',
                      !manualInput.trim() && 'opacity-50 cursor-not-allowed'
                    )}
                  >
                    <Plus className="h-4 w-4" />
                    添加
                  </button>
                </div>

                {/* 容器勾选列表 */}
                <div className="border border-gray-200 dark:border-gray-700 rounded-xl overflow-hidden">
                  {candidatesLoading && candidates.length === 0 ? (
                    <div className="px-4 py-6 text-center text-sm text-gray-500 dark:text-gray-400">
                      正在读取容器列表…
                    </div>
                  ) : candidates.length === 0 ? (
                    <div className="px-4 py-6 text-center">
                      <Package className="mx-auto h-8 w-8 text-gray-400" />
                      <p className="mt-2 text-sm text-gray-500 dark:text-gray-400">暂无容器</p>
                    </div>
                  ) : (
                    <div className="divide-y divide-gray-100 dark:divide-gray-700/60 max-h-72 overflow-y-auto">
                      {candidates.map((candidate) => {
                        const protectedItem = candidate.protected
                        const checked = isExcludedName(candidate.containerName)
                        return (
                          <div
                            key={candidate.containerId}
                            className={cn(
                              'flex items-center gap-3 px-3 py-2.5 transition-colors',
                              protectedItem
                                ? 'bg-gray-50 dark:bg-gray-800/60'
                                : 'hover:bg-gray-50 dark:hover:bg-gray-700/40'
                            )}
                          >
                            {/* 勾选框 */}
                            <button
                              onClick={() => !protectedItem && toggleExclude(candidate.containerName)}
                              disabled={protectedItem}
                              className={cn(
                                'flex-shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors',
                                protectedItem
                                  ? 'bg-gray-200 dark:bg-gray-600 border-gray-300 dark:border-gray-500 cursor-not-allowed'
                                  : checked
                                    ? 'bg-primary-600 border-primary-600 dark:bg-primary-500 dark:border-primary-500'
                                    : 'border-gray-300 dark:border-gray-500 hover:border-primary-400'
                              )}
                              title={protectedItem ? 'Docker Copilot 自身，永不自动更新' : checked ? '取消排除' : '加入排除'}
                            >
                              {checked && !protectedItem && (
                                <CheckCircle2 className="h-3 w-3 text-white" />
                              )}
                              {protectedItem && <Lock className="h-2.5 w-2.5 text-gray-500 dark:text-gray-400" />}
                            </button>

                            {/* 信息 */}
                            <div className="flex-1 min-w-0">
                              <div className="flex items-center gap-2 min-w-0">
                                <span className="text-sm font-medium text-gray-900 dark:text-white truncate">
                                  {candidate.containerName}
                                </span>
                                {candidate.state && (
                                  <span
                                    className={cn(
                                      'badge flex-shrink-0',
                                      candidate.state === 'running'
                                        ? 'badge-success'
                                        : 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300'
                                    )}
                                  >
                                    {candidate.state === 'running' ? '运行中' : '已停止'}
                                  </span>
                                )}
                              </div>
                              <p className="text-xs text-gray-500 dark:text-gray-400 truncate mt-0.5">
                                {candidate.image}
                              </p>
                            </div>

                            {/* 状态标记 */}
                            <div className="flex-shrink-0 text-right">
                              {candidate.willUpdate ? (
                                <span className="badge-warning">将自动更新</span>
                              ) : (
                                <span className="text-xs text-gray-400 dark:text-gray-500" title={candidate.excludeReason}>
                                  {candidate.excludeReason || '跳过'}
                                </span>
                              )}
                            </div>
                          </div>
                        )
                      })}
                    </div>
                  )}
                </div>
              </div>

              {/* 上次执行结果 */}
              <div>
                <div className="flex items-center justify-between mb-2">
                  <h4 className="text-sm font-semibold text-gray-900 dark:text-white">执行记录</h4>
                  <div className="flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
                    <Clock className="h-3.5 w-3.5" />
                    <span>
                      {formatTime(setting?.lastRunAt)}
                      {setting?.lastTrigger && ` · ${TRIGGER_LABELS[setting.lastTrigger] || setting.lastTrigger}`}
                    </span>
                  </div>
                </div>
                {lastResult.length === 0 ? (
                  <div className="card px-4 py-4 text-center text-sm text-gray-500 dark:text-gray-400">
                    暂无执行记录
                  </div>
                ) : (
                  <div className="border border-gray-200 dark:border-gray-700 rounded-xl overflow-hidden divide-y divide-gray-100 dark:divide-gray-700/60 max-h-56 overflow-y-auto">
                    {lastResult.map((record, index) => (
                      <div key={`${record.containerId}-${record.finishedAt}-${index}`} className="flex items-start gap-3 px-3 py-2.5">
                        <div
                          className={cn(
                            'flex-shrink-0 w-6 h-6 rounded-full flex items-center justify-center mt-0.5',
                            record.success
                              ? 'bg-green-50 dark:bg-green-900/30'
                              : 'bg-red-50 dark:bg-red-900/30'
                          )}
                        >
                          {record.success ? (
                            <CheckCircle2 className="h-3.5 w-3.5 text-green-600 dark:text-green-400" />
                          ) : (
                            <AlertCircle className="h-3.5 w-3.5 text-red-600 dark:text-red-400" />
                          )}
                        </div>
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-2">
                            <span className="text-sm font-medium text-gray-900 dark:text-white truncate">
                              {record.containerName}
                            </span>
                            <span className="text-xs text-gray-400 dark:text-gray-500 flex-shrink-0">
                              {formatTime(record.finishedAt)}
                            </span>
                          </div>
                          <p className="text-xs text-gray-500 dark:text-gray-400 mt-0.5 break-words">
                            {record.message || (record.success ? '更新成功' : '更新失败')}
                          </p>
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* 配置路径提示 */}
              {setting?.configPath && (
                <p className="text-xs text-gray-400 dark:text-gray-500 break-all">
                  配置文件：{setting.configPath}
                </p>
              )}
            </>
          )}
        </div>

        {/* 底部操作 */}
        <div className="px-5 py-4 border-t border-gray-200 dark:border-gray-700 bg-gray-50 dark:bg-gray-700/30 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 flex-shrink-0">
          <div className="text-xs text-gray-500 dark:text-gray-400 order-2 sm:order-1">
            {candidates.length > 0 && (
              <>
                当前 <span className="font-semibold text-amber-600 dark:text-amber-400">{willUpdateCount}</span> 个容器会被自动更新
                {dirty && <span className="ml-2 text-amber-600 dark:text-amber-400">· 有未保存的改动</span>}
              </>
            )}
          </div>
          <div className="flex gap-2 order-1 sm:order-2">
            <button
              onClick={handleRunNow}
              disabled={isRunning || saving || !form}
              className={cn(
                'flex-1 sm:flex-none btn-secondary flex items-center justify-center gap-2',
                (isRunning || saving || !form) && 'opacity-50 cursor-not-allowed'
              )}
              title="立即执行一轮自动更新（不受总开关限制）"
            >
              {isRunning ? (
                <>
                  <RefreshCw className="h-4 w-4 animate-spin" />
                  执行中
                </>
              ) : (
                <>
                  <Zap className="h-4 w-4" />
                  立即执行
                </>
              )}
            </button>
            <button
              onClick={handleSave}
              disabled={saving || !form}
              className={cn(
                'flex-1 sm:flex-none btn-primary flex items-center justify-center gap-2',
                (saving || !form) && 'opacity-50 cursor-not-allowed'
              )}
            >
              {saving ? (
                <>
                  <RefreshCw className="h-4 w-4 animate-spin" />
                  保存中
                </>
              ) : (
                <>
                  <Save className="h-4 w-4" />
                  保存设置
                </>
              )}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
