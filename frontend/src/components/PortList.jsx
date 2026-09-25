import React, { useState } from 'react'
import { cn } from '../utils/cn.js'

// 卡片里默认最多显示几个端口，多出来的收起成「+N」。
// 之所以是 2 而不是 3：端口胶囊换行会把卡片撑高，同一行里卡片高度就参差了。
// 收到 2 个后，端口区最多两行，卡片高度基本齐平（超出部分点开详情看全部）。
const MAX_VISIBLE = 2

function portTitle(port) {
  if (port.direct) {
    return `host 网络：直接占用宿主机 ${port.containerPort}/${port.protocol}，容器端口即宿主机端口`
  }
  if (port.hostPort > 0) {
    const ip = port.hostIp && port.hostIp !== '0.0.0.0' && port.hostIp !== '::' ? `${port.hostIp}:` : ''
    return `已映射：宿主机 ${ip}${port.hostPort} → 容器 ${port.containerPort}/${port.protocol}`
  }
  return `容器内暴露 ${port.containerPort}/${port.protocol}，未映射到宿主机`
}

/**
 * 单个端口胶囊。
 * 已映射到宿主机的用实色高亮，形式上就是 docker 的 宿主机端口:容器端口；
 * host 网络的也是实色高亮，但只写一个数字 —— 那里没有端口转换，两边是同一个端口；
 * 没映射的用灰色虚线框，只显示容器端口。
 */
export function PortBadge({ port, size = 'sm' }) {
  const direct = !!port.direct
  const mapped = port.hostPort > 0
  const showProtocol = port.protocol && port.protocol !== 'tcp'

  return (
    <span
      title={portTitle(port)}
      className={cn(
        'inline-flex items-baseline rounded-md border font-mono leading-none whitespace-nowrap',
        size === 'md' ? 'px-2 py-1 text-xs' : 'px-1.5 py-[3px] text-[11px]',
        direct || mapped
          ? 'bg-primary-50 text-primary-700 border-primary-200 dark:bg-primary-500/15 dark:text-primary-300 dark:border-primary-500/30'
          : 'bg-gray-50 text-gray-500 border-dashed border-gray-300 dark:bg-gray-700/40 dark:text-gray-400 dark:border-gray-600'
      )}
    >
      {direct ? (
        <span className="font-semibold">{port.containerPort}</span>
      ) : mapped ? (
        <>
          <span className="font-semibold">{port.hostPort}</span>
          <span className="opacity-50">:</span>
          <span className="opacity-75">{port.containerPort}</span>
        </>
      ) : (
        <span className="opacity-75">{port.containerPort}</span>
      )}
      {showProtocol && <span className="ml-0.5 uppercase opacity-60">{port.protocol}</span>}
    </span>
  )
}

/**
 * 卡片里的端口行：端口胶囊，超出 maxVisible 时收成「+N」，点开看全部。
 * 不设前置图标，胶囊左边缘与容器名同列（与「运行:」那行对齐）。
 * 没有端口时不占空位，而是明确写一句说明，避免让人以为没加载出来。
 */
export function PortBadgeRow({ ports = [], networkMode = '', maxVisible = MAX_VISIBLE }) {
  const [expanded, setExpanded] = useState(false)
  const isHostNetwork = networkMode === 'host'

  if (ports.length === 0) {
    return (
      <div className="mt-2 flex items-center min-h-[18px]">
        <span
          className="text-[11px] text-gray-400 dark:text-gray-500 truncate"
          title={
            isHostNetwork
              ? 'host 网络模式：容器直接用宿主机网络，端口不需要映射；镜像本身也没声明 EXPOSE，所以列不出它监听了哪些端口'
              : '这个容器没有映射端口，镜像也没有声明 EXPOSE'
          }
        >
          {isHostNetwork ? 'host 网络，未声明端口' : '未映射端口'}
        </span>
      </div>
    )
  }

  const visible = expanded ? ports : ports.slice(0, maxVisible)
  const hiddenCount = ports.length - visible.length

  return (
    <div className="mt-2 flex items-start gap-1 flex-wrap min-h-[18px]">
      {visible.map((port, index) => (
        <PortBadge key={`${port.hostPort}-${port.containerPort}-${port.protocol}-${index}`} port={port} />
      ))}
      {hiddenCount > 0 && (
        <button
          onClick={(event) => {
            event.stopPropagation()
            setExpanded(true)
          }}
          className="px-1.5 py-[3px] rounded-md text-[11px] font-medium leading-none bg-gray-100 text-gray-500 hover:bg-gray-200 dark:bg-gray-700 dark:text-gray-300 dark:hover:bg-gray-600 transition-colors"
          title={`还有 ${hiddenCount} 个端口，点击展开`}
        >
          +{hiddenCount}
        </button>
      )}
      {expanded && ports.length > maxVisible && (
        <button
          onClick={(event) => {
            event.stopPropagation()
            setExpanded(false)
          }}
          className="px-1.5 py-[3px] rounded-md text-[11px] font-medium leading-none text-gray-400 hover:text-gray-600 dark:text-gray-400 dark:hover:text-gray-200 transition-colors"
          title="收起"
        >
          收起
        </button>
      )}
    </div>
  )
}
