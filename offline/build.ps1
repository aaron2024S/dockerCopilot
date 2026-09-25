<#
.SYNOPSIS
  dockerCopilot 离线镜像一键构建（Windows，**不需要 Docker 引擎**）。

.DESCRIPTION
  按顺序执行：交叉编译 Go 二进制 ->（可选）构建前端 -> 拉取基础镜像层
  -> 组装 docker-save 镜像 tar -> 静态校验 -> 启动行为校验。
  任一环节失败即中止，不会产出坏镜像。

.EXAMPLE
  .\build.ps1                          # 用现有二进制，重建 amd64 镜像并校验（默认只做 amd64）
  .\build.ps1 -Arch arm64              # 只做 arm64
  .\build.ps1 -Arch all                # 两个架构都做
  .\build.ps1 -CrossCompile -Frontend  # 从源码全量重建（含前端与二进制）

.NOTES
  前置：Python 3（脚本会自动探测本机托管运行时）。
  加 -CrossCompile 时还需要 Go 工具链；加 -Frontend 时还需要 Node。
#>
param(
  [ValidateSet('amd64', 'arm64', 'all')][string]$Arch = 'amd64',
  [switch]$CrossCompile,
  [switch]$Frontend,
  [string]$SourceDateEpoch = '',
  [string]$GoExe = '',
  [string]$PythonExe = '',
  [string]$NodeExe = ''
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$env:PYTHONIOENCODING = 'utf-8'

# 设了就固定归档里的时间戳，同样输入能产出逐字节相同的镜像 tar（可复现构建）。
# 不设则用当前时间，哈希每次都会变，只适合"能用就行"的临时构建。
if ($SourceDateEpoch) {
  $env:SOURCE_DATE_EPOCH = $SourceDateEpoch
}

$Offline = $PSScriptRoot
$Project = Split-Path $Offline -Parent
$Version = (Get-Content (Join-Path $Project 'version') -Raw).Trim()

function Find-Exe([string]$Given, [string[]]$Candidates, [string]$Name) {
  if ($Given -and (Test-Path $Given)) { return $Given }
  foreach ($c in $Candidates) { if ($c -and (Test-Path $c)) { return $c } }
  $cmd = Get-Command $Name -ErrorAction SilentlyContinue
  if ($cmd) { return $cmd.Source }
  return $null
}

$Py = Find-Exe $PythonExe @(
  "$env:USERPROFILE\.workbuddy\binaries\python\versions\3.13.12\python.exe"
) 'python'
if (-not $Py) { throw 'FATAL: 找不到 Python，请用 -PythonExe 指定' }
Write-Host "python : $Py" -ForegroundColor DarkGray

$ArchList = if ($Arch -eq 'all') { @('amd64', 'arm64') } else { @($Arch) }

# ----------------------------------------------------------- 0. 前端（可选）
if ($Frontend) {
  $Npm = Find-Exe '' @(
    "$env:USERPROFILE\.workbuddy\binaries\node\versions\22.22.2-3\npm.cmd"
  ) 'npm'
  if (-not $Npm) { throw 'FATAL: 找不到 npm' }
  Write-Host "`n=== [0/4] 构建前端 ===" -ForegroundColor Cyan
  Push-Location (Join-Path $Project 'frontend')
  try {
    # 本机 esbuild 压缩阶段会确定性死锁，必须关掉 minify
    & $Npm run build -- --minify false
    if ($LASTEXITCODE -ne 0) { throw "前端构建失败 (exit $LASTEXITCODE)" }
  } finally { Pop-Location }
  Write-Host "  产物已写入 front/（//go:embed 的目标目录）"
}

# --------------------------------------------------- 1. Go 二进制（可选）
if ($CrossCompile) {
  $Go = Find-Exe $GoExe @(
    "$env:USERPROFILE\.workbuddy\binaries\go\rt\go\bin\go.exe"
  ) 'go'
  if (-not $Go) { throw 'FATAL: 找不到 Go 工具链' }
  $env:GOPATH = "$env:USERPROFILE\.workbuddy\binaries\go\gopath"
  $env:GOPROXY = 'https://goproxy.cn,direct'
  $env:GOTOOLCHAIN = 'local'
  $env:GOFLAGS = '-mod=mod'
  $env:CGO_ENABLED = '0'
  $env:GOOS = 'linux'
  $ld = "-w -s -X github.com/onlyLTY/dockerCopilot/internal/config.Version=$Version " +
        "-X github.com/onlyLTY/dockerCopilot/internal/config.BuildDate=" +
        (Get-Date -Format 'yyyy-MM-dd')

  Write-Host "`n=== [1/4] 交叉编译 linux 二进制 ===" -ForegroundColor Cyan
  Write-Host "go     : $Go" -ForegroundColor DarkGray
  foreach ($a in $ArchList) {
    $env:GOARCH = $a
    $dest = Join-Path $Project "dc-back\dist\linux\$a"
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Write-Host "  -> $a ..."
    & $Go -C $Project build --trimpath "-ldflags=$ld" -o "$dest\dockerCopilot" .
    # stderr 里的 "go: downloading ..." 会被 PowerShell 记成错误，故只看二进制是否产出
    if (-not (Test-Path "$dest\dockerCopilot")) { throw "$a 编译失败" }
  }
}

# ------------------------------------------------ 2. 拉基础层 + 组装 + 校验
foreach ($a in $ArchList) {
  Write-Host "`n=== [2/4] 拉取基础镜像 linux/$a ===" -ForegroundColor Cyan
  & $Py (Join-Path $Offline 'fetch_base.py') --arch $a
  if ($LASTEXITCODE -ne 0) { throw "fetch_base 失败 ($a)" }

  Write-Host "`n=== [3/4] 组装镜像 $a ===" -ForegroundColor Cyan
  & $Py (Join-Path $Offline 'build_offline.py') --arch $a
  if ($LASTEXITCODE -ne 0) { throw "build_offline 失败 ($a)" }

  $tar = Join-Path $Offline "dockercopilot-$Version-$a-image.tar"

  Write-Host "`n=== [4/4] 校验镜像 $a ===" -ForegroundColor Cyan
  & $Py (Join-Path $Offline 'validate_image.py') $tar
  if ($LASTEXITCODE -ne 0) { throw "静态校验未通过 ($a)" }

  & $Py (Join-Path $Offline 'validate_boot.py') $tar
  if ($LASTEXITCODE -ne 0) { throw "行为校验未通过 ($a)" }
}

Write-Host "`n==== 全部完成 ====" -ForegroundColor Green
foreach ($a in $ArchList) {
  $tar = Join-Path $Offline "dockercopilot-$Version-$a-image.tar"
  $mb = [math]::Round((Get-Item $tar).Length / 1MB, 1)
  Write-Host ("  {0}  $mb MB" -f (Split-Path $tar -Leaf))
}
Write-Host "`n部署：把镜像 tar + docker-compose.offline.yml 拷到目标机，然后"
Write-Host "  docker load -i dockercopilot-$Version-amd64-image.tar"
Write-Host "  docker compose -f docker-compose.offline.yml up -d"
Write-Host "`nsha256 清单: $(Join-Path $Offline 'SHA256SUMS.txt')"
