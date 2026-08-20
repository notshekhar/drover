# drover installer for Windows.
#   irm https://raw.githubusercontent.com/notshekhar/drover/main/install.ps1 | iex
#
# drover is a single static Go binary, so there is nothing else to install.
#
# Layout after install:
#   $env:USERPROFILE\.drover-bin\drover.exe
#   ...\.drover-bin added to the user PATH
#
# The install dir is .drover-bin, NOT .drover. The latter is drover's data
# directory — its objects and checkouts — and the installer never touches it.
#
# Knobs (set before piping to iex):
#   $env:DROVER_REPO_SLUG   notshekhar/drover
#   $env:DROVER_VERSION     vX.Y.Z            pin a release
#   $env:DROVER_BIN_HOME    ~\.drover-bin     install dir
#   $env:DROVER_FORCE       1                 reinstall when up to date
#   $env:DROVER_UNINSTALL   1                 remove the install

$ErrorActionPreference = "Stop"

$RepoSlug = if ($env:DROVER_REPO_SLUG) { $env:DROVER_REPO_SLUG } else { "notshekhar/drover" }
$BinHome  = if ($env:DROVER_BIN_HOME)  { $env:DROVER_BIN_HOME }  else { Join-Path $env:USERPROFILE ".drover-bin" }
$Pin      = $env:DROVER_VERSION
$Force    = $env:DROVER_FORCE -eq "1"

function Bold($m) { Write-Host $m -ForegroundColor White }
function Dim($m)  { Write-Host $m -ForegroundColor DarkGray }
function Fail($m) { Write-Host $m -ForegroundColor Red; exit 1 }

# ── Uninstall ─────────────────────────────────────────────────────────────
if ($env:DROVER_UNINSTALL -eq "1") {
    Bold "> Removing drover"
    if (Test-Path $BinHome) {
        Remove-Item $BinHome -Recurse -Force
        Dim "  removed $BinHome"
    }
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -and $userPath.Split(';') -contains $BinHome) {
        $kept = ($userPath.Split(';') | Where-Object { $_ -ne $BinHome }) -join ';'
        [Environment]::SetEnvironmentVariable("Path", $kept, "User")
        Dim "  removed $BinHome from PATH"
    }
    Bold "drover removed"
    $data = Join-Path $env:USERPROFILE ".drover"
    if (Test-Path $data) {
        # Objects and checkouts. Deleting someone's data on an uninstall would
        # be unforgivable, so it is only ever mentioned.
        Dim "  your data is still at $data; delete it yourself if you want it gone"
    }
    exit 0
}

# ── Detect target ─────────────────────────────────────────────────────────
$archRaw = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) { $archRaw = $env:PROCESSOR_ARCHITEW6432 }
switch ($archRaw) {
    "AMD64" { $arch = "x64" }
    "ARM64" { $arch = "arm64" }
    default { Fail "unsupported architecture: $archRaw" }
}
$target = "windows-$arch"

Bold "> drover installer"
Dim  "  target: $target"

# ── Resolve the release tag ───────────────────────────────────────────────
# The releases/latest redirect avoids the anonymous API rate limit.
function Resolve-LatestTag {
    try {
        $resp = Invoke-WebRequest -Uri "https://github.com/$RepoSlug/releases/latest" `
            -MaximumRedirection 0 -ErrorAction SilentlyContinue -UseBasicParsing
        $loc = $resp.Headers.Location
        if ($loc) { return ($loc -split '/')[-1] }
    } catch {
        if ($_.Exception.Response) {
            $loc = $_.Exception.Response.Headers.Location
            if ($loc) { return ($loc.ToString() -split '/')[-1] }
        }
    }
    try {
        $api = Invoke-RestMethod -Uri "https://api.github.com/repos/$RepoSlug/releases/latest" -UseBasicParsing
        return $api.tag_name
    } catch { return $null }
}

if ($Pin) {
    $tag = if ($Pin.StartsWith("v")) { $Pin } else { "v$Pin" }
} else {
    $tag = Resolve-LatestTag
}
if (-not $tag) { Fail "could not resolve a release tag for $RepoSlug. Is there a published release yet?" }

# ── Up-to-date gate ───────────────────────────────────────────────────────
$exe = Join-Path $BinHome "drover.exe"
if ((Test-Path $exe) -and -not $Force) {
    try {
        $installed = (& $exe version) -replace '^drover\s+', ''
        if ($installed -and ("v$installed" -eq $tag)) {
            Bold "drover is up to date ($installed)"
            Dim  "  `$env:DROVER_FORCE=1 to reinstall"
            exit 0
        }
        Dim "  update: $installed -> $($tag.TrimStart('v'))"
    } catch {}
} else {
    Dim "  installing $($tag.TrimStart('v'))"
}

$tmpRoot = Join-Path ([System.IO.Path]::GetTempPath()) "drover-install-$PID"
New-Item -ItemType Directory -Force -Path $tmpRoot | Out-Null

$base = "https://github.com/$RepoSlug/releases/download/$tag"
$url  = "$base/drover-$target.tar.gz"
$tar  = Join-Path $tmpRoot "drover.tar.gz"

# Streamed download with a live bar. Throws on HTTP errors; the caller falls
# back to Invoke-WebRequest on any failure (older hosts, redirected console,
# missing System.Net.Http, ...).
function Download-WithProgress {
    param([string]$Url, [string]$OutFile)

    # Windows PowerShell 5.1 needs the assembly loaded explicitly.
    try { Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue } catch {}

    $client = [System.Net.Http.HttpClient]::new()
    $client.DefaultRequestHeaders.UserAgent.ParseAdd("drover-installer")
    $stream = $null
    $file = $null
    try {
        $resp = $client.GetAsync($Url, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        if (-not $resp.IsSuccessStatusCode) { throw "HTTP $([int]$resp.StatusCode)" }
        $total  = $resp.Content.Headers.ContentLength
        $stream = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $file   = [System.IO.File]::Create($OutFile)

        $buf = New-Object byte[] 262144
        $done = 0
        $width = 50
        $lastPct = -1
        try { [Console]::CursorVisible = $false } catch {}
        while (($n = $stream.Read($buf, 0, $buf.Length)) -gt 0) {
            $file.Write($buf, 0, $n)
            $done += $n
            if ($total) {
                $pct = [int][math]::Min(100, ($done * 100 / $total))
                if ($pct -ne $lastPct) {
                    $on  = [int]($pct * $width / 100)
                    # "·" (U+00B7) over the shell installer's "･": it exists in
                    # the legacy conhost codepages, so old terminals degrade
                    # gracefully instead of printing boxes.
                    $bar = ("■" * $on) + ("·" * ($width - $on))
                    Write-Host -NoNewline ("`r$bar {0,3}%" -f $pct) -ForegroundColor DarkYellow
                    $lastPct = $pct
                }
            }
        }
        if ($lastPct -ge 0) { Write-Host "" }
    } finally {
        try { [Console]::CursorVisible = $true } catch {}
        if ($file)   { $file.Dispose() }
        if ($stream) { $stream.Dispose() }
        $client.Dispose()
    }
}

Bold "> Downloading $($url.Split('/')[-1])"
$downloaded = $false
if (-not [Console]::IsOutputRedirected) {
    try {
        Download-WithProgress -Url $url -OutFile $tar
        $downloaded = $true
    } catch {
        Remove-Item $tar -Force -ErrorAction SilentlyContinue
    }
}
if (-not $downloaded) {
    try {
        Invoke-WebRequest -Uri $url -OutFile $tar -UseBasicParsing
    } catch {
        Fail "download failed: $url`n  the release may not have a $target asset"
    }
}

# ── Verify ────────────────────────────────────────────────────────────────
try {
    $sumFile = Join-Path $tmpRoot "drover.tar.gz.sha256"
    Invoke-WebRequest -Uri "$url.sha256" -OutFile $sumFile -UseBasicParsing -ErrorAction Stop
    $expected = ((Get-Content $sumFile -Raw).Trim() -split '\s+')[0]
    $got = (Get-FileHash $tar -Algorithm SHA256).Hash.ToLower()
    if ($expected.ToLower() -ne $got) {
        Fail "sha256 mismatch (expected $expected, got $got)"
    }
    Dim "  sha256 ok"
} catch {
    Dim "  sha256 file missing - skipping verify"
}

# ── Extract ───────────────────────────────────────────────────────────────
Bold "> Extracting"
# tar.exe ships with Windows 10 1803 and later.
if (-not (Get-Command tar -ErrorAction SilentlyContinue)) {
    Fail "tar is required and was not found (Windows 10 1803+ ships it)"
}
& tar -xzf $tar -C $tmpRoot
if ($LASTEXITCODE -ne 0) { Fail "extract failed" }

$srcExe = Join-Path (Join-Path $tmpRoot $target) "drover.exe"
if (-not (Test-Path $srcExe)) { Fail "tarball missing $target/drover.exe" }

# ── Install ───────────────────────────────────────────────────────────────
Bold "> Installing to $BinHome"
New-Item -ItemType Directory -Force -Path $BinHome | Out-Null

# A running drover holds its own exe open, so replacing it in place fails.
# Move the old one aside first; Windows permits renaming a running image.
if (Test-Path $exe) {
    $old = "$exe.old"
    Remove-Item $old -Force -ErrorAction SilentlyContinue
    try { Move-Item $exe $old -Force } catch {
        Fail "could not replace $exe - is drover still running? Stop it and re-run."
    }
    Remove-Item $old -Force -ErrorAction SilentlyContinue
}
Move-Item $srcExe $exe -Force
# `drover upgrade` looks for this before replacing the binary.
Set-Content -Path (Join-Path $BinHome ".install-method") -Value "binary" -ErrorAction SilentlyContinue
Remove-Item $tmpRoot -Recurse -Force -ErrorAction SilentlyContinue

# ── PATH ──────────────────────────────────────────────────────────────────
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
if ($userPath.Split(';') -notcontains $BinHome) {
    $newPath = if ($userPath.TrimEnd(';')) { "$($userPath.TrimEnd(';'));$BinHome" } else { $BinHome }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Dim "  added $BinHome to your PATH"
    $needsNewShell = $true
}
# Make it usable in this session too, so the next line of the README works.
if ($env:Path.Split(';') -notcontains $BinHome) { $env:Path = "$env:Path;$BinHome" }

# ── Smoke test ────────────────────────────────────────────────────────────
try {
    $version = (& $exe version)
} catch {
    Fail "the installed binary did not run: $exe"
}

Write-Host ""
Bold "$version installed"
Write-Host ""
Dim  "  start the engine:"
Write-Host "    drover serve"
Dim  "  then, in another terminal, give it a repository:"
Write-Host "    drover apply -f repo.yaml"
Dim  "  and point an agent at it:"
Write-Host "    claude mcp add --transport http drover http://127.0.0.1:7432/mcp"
if ($needsNewShell) {
    Write-Host ""
    Dim "  open a new terminal for the PATH change to apply everywhere"
}
Write-Host ""
