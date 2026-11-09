# Install ka from a GitHub release on Windows.
#
#   irm https://raw.githubusercontent.com/JumpKickOSS/ka/main/install.ps1 | iex
#
# Environment:
#   KA_VERSION      a release to install, e.g. 1.0.3 (default: the latest), or
#                   "main" to build the newest commit with Go
#   KA_INSTALL_DIR  where to put ka.exe (default: %LOCALAPPDATA%\Programs\ka,
#                   added to your user PATH)
#   KA_FROM_SOURCE  1 to build with Go even when a release archive exists
#
# The archive is checked against the release's checksums.txt. If cosign is
# installed, checksums.txt is first verified against its Sigstore bundle.
#
# With Go 1.27 or newer installed, ka is built from source when there is no
# archive for this platform or version, or when asked. Run from a checkout of
# the ka repository (.\install.ps1), the script installs the release that
# matches the checked-out tag, or builds the checkout itself when it is not on
# a tag, so the binary always fits the checkout's ui\.
# Running the script again upgrades ka in place.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Windows PowerShell 5.1 may default to TLS 1.0, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'JumpKickOSS/ka'
$Identity = '^https://github\.com/JumpKickOSS/ka/\.github/workflows/release\.yml@refs/tags/v'
$Issuer = 'https://token.actions.githubusercontent.com'

function Say([string]$msg) { [Console]::Error.WriteLine($msg) }
function Die([string]$msg) { Say "ka install: $msg"; exit 1 }

$dir = if ($env:KA_INSTALL_DIR) { $env:KA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ka' }
$version = if ($env:KA_VERSION) { $env:KA_VERSION.TrimStart('v') } else { '' }

function Finish {
    $exe = Join-Path $dir 'ka.exe'
    $v = try { & $exe version } catch { 'ka' }
    Say "Installed $v to $exe"
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not (($userPath -split ';') -contains $dir)) {
        [Environment]::SetEnvironmentVariable('Path', "$dir;$userPath", 'User')
        Say "Added $dir to your user PATH (new terminals pick it up)."
    }
    if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$dir;$env:Path" }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Say 'Note: ka needs git 2.40 or newer at run time.' }
    exit 0
}

function Have-Go { [bool](Get-Command go -ErrorAction SilentlyContinue) }

function Prepare-Dir { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

# Go-Install builds github.com/JumpKickOSS/ka/cmd/ka@REF into $dir.
function Go-Install([string]$why, [string]$ref) {
    if (-not (Have-Go)) { Die $why }
    Prepare-Dir
    Say "Building ka $ref with $((go version).Split(' ')[2])"
    $env:GOBIN = $dir
    go install "github.com/$Repo/cmd/ka@$ref"
    if ($LASTEXITCODE -ne 0) { Die 'go install failed' }
    Finish
}

# A checkout of this repository: install what fits its ui\.
$here = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
$isCheckout = (Test-Path (Join-Path $here 'embed.go')) -and (Test-Path (Join-Path $here 'ui\ka-ui.json')) -and
    (Test-Path (Join-Path $here 'go.mod')) -and ((Get-Content (Join-Path $here 'go.mod') -TotalCount 1) -eq "module github.com/$Repo")
if ($isCheckout -and -not $version) {
    $tag = git -C $here describe --tags --exact-match 2>$null
    if ($LASTEXITCODE -eq 0 -and $tag) {
        $version = $tag.Trim().TrimStart('v')
        Say "This checkout is at $tag"
    } elseif (Have-Go) {
        Prepare-Dir
        Say "This checkout is not on a release tag; building it with $((go version).Split(' ')[2])"
        $env:GOBIN = $dir
        Push-Location $here
        try { go install ./cmd/ka } finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { Die 'go install failed' }
        Finish
    } else {
        Say 'This checkout is not on a release tag and Go is not installed: installing the latest release.'
        Say 'Its web UI may not fit that release; ka then serves the UI built into the binary.'
    }
}

if ($version -in @('main', 'head', 'HEAD')) {
    Go-Install 'building the newest commit needs Go 1.27 or newer (https://go.dev/dl/)' 'main'
}

$arch = switch ($env:PROCESSOR_ARCHITECTURE) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { '' } }
$ref = if ($version) { "v$version" } else { 'latest' }
if (-not $arch) {
    Go-Install "there is no ka release for $($env:PROCESSOR_ARCHITECTURE); install Go 1.27 or newer (https://go.dev/dl/) and run this again" $ref
}
if ($env:KA_FROM_SOURCE -eq '1') {
    Go-Install 'KA_FROM_SOURCE=1 needs Go 1.27 or newer (https://go.dev/dl/)' $ref
}

if (-not $version) {
    # Resolve "latest" once, so every file comes from the same release.
    try {
        $tag = (Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases/latest").tag_name
    } catch { $tag = '' }
    if ($tag -notmatch '^v(.+)$') { Die "could not find the latest release of $Repo" }
    $version = $Matches[1]
}
$base = "https://github.com/$Repo/releases/download/v$version"

Prepare-Dir
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ka-install-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $archive = "ka_windows_$arch.zip"
    Say "Downloading ka $version ($archive)"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    } catch {
        if (-not (Have-Go)) { Die "could not download $base/$archive" }
        Say "No release archive $archive for $version; building it from source instead."
        Go-Install '' "v$version"
    }
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

    if (Get-Command cosign -ErrorAction SilentlyContinue) {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt.sigstore.json" -OutFile (Join-Path $tmp 'checksums.txt.sigstore.json')
        cosign verify-blob --bundle (Join-Path $tmp 'checksums.txt.sigstore.json') `
            --certificate-identity-regexp $Identity --certificate-oidc-issuer $Issuer (Join-Path $tmp 'checksums.txt') 2>$null | Out-Null
        if ($LASTEXITCODE -ne 0) { Die 'checksums.txt failed signature verification' }
        Say 'Verified the release signature'
    }

    $want = (Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { ($_ -split '\s+')[1] -eq $archive } | ForEach-Object { ($_ -split '\s+')[0] }) | Select-Object -First 1
    if (-not $want) { Die "$archive is not listed in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
    if ($got -ne $want.ToLower()) { Die "checksum mismatch for $archive" }

    Expand-Archive -Force -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x')
    $new = Join-Path $dir 'ka.exe.new'
    Copy-Item -Force (Join-Path $tmp 'x\ka.exe') $new
    $exe = Join-Path $dir 'ka.exe'
    # A running ka.exe can't be overwritten but can be renamed out of the way.
    if (Test-Path $exe) {
        $old = Join-Path $dir 'ka.exe.old'
        Remove-Item -Force $old -ErrorAction SilentlyContinue
        Move-Item -Force $exe $old
    }
    Move-Item -Force $new $exe
    Remove-Item -Force (Join-Path $dir 'ka.exe.old') -ErrorAction SilentlyContinue
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
Finish
