# Install ka from a GitHub release on Windows.
#
#   irm https://raw.githubusercontent.com/KanArtist/ka/main/install.ps1 | iex
#
# Works in Windows PowerShell 5.1 and PowerShell 7.
#
# Environment:
#   KA_VERSION      a release to install, e.g. 1.0.3 (default: the latest), or
#                   "main" to build the newest commit with Go
#   KA_INSTALL_DIR  where to put ka.exe (default: %LOCALAPPDATA%\Programs\ka,
#                   added to your user PATH)
#   KA_FROM_SOURCE  1 to build with Go even when a release archive exists
#   KA_REQUIRE_SIGNATURE
#                   1 to fail unless cosign verifies the release signature
#
# The archive is checked against the release's checksums.txt. If cosign is
# installed, checksums.txt is first verified against its Sigstore bundle, which
# proves it was built by this repository's release workflow for that version's
# tag. Without cosign the signature is not verified, and the script says so.
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

$Repo = 'KanArtist/ka'
# The signing certificate must name this workflow at the tag being installed.
$IdentityPrefix = 'https://github.com/KanArtist/ka/.github/workflows/release.yml@refs/tags/v'
$Issuer = 'https://token.actions.githubusercontent.com'

function Say([string]$msg) { [Console]::Error.WriteLine($msg) }
function Die([string]$msg) { Say "ka install: $msg"; exit 1 }

# Native runs a native command, such as { git describe }, returns its stdout
# lines and leaves its exit code in $script:NativeExit (-1 if it did not run).
# Under Windows PowerShell 5.1 with ErrorActionPreference 'Stop', a native
# command's redirected stderr line is a terminating error, so the call runs with
# 'Continue' and stderr is passed on as text (or dropped with -Quiet).
function Native([scriptblock]$Cmd, [switch]$Quiet) {
    $ErrorActionPreference = 'Continue'
    $script:NativeExit = -1
    try {
        & $Cmd 2>&1 | ForEach-Object {
            if ($_ -is [System.Management.Automation.ErrorRecord]) {
                if (-not $Quiet) { Say $_.ToString() }
            } else { $_ }
        }
        $script:NativeExit = $LASTEXITCODE
    } catch {
        if (-not $Quiet) { Say $_.ToString() }
    }
}

$dir = if ($env:KA_INSTALL_DIR) { $env:KA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ka' }
$version = if ($env:KA_VERSION) { $env:KA_VERSION.TrimStart('v') } else { '' }

# Add-UserPath puts $dir in front of the user PATH unless it is there already.
# It edits the registry value as stored, so %VARIABLE% entries stay unexpanded
# and the value keeps its kind; [Environment]::SetEnvironmentVariable would
# write back the expanded string.
function Add-UserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $cur = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $kind = if ($key.GetValueNames() -contains 'Path') { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
        $want = $dir.TrimEnd('\')
        foreach ($p in ($cur -split ';')) {
            if ([Environment]::ExpandEnvironmentVariables($p).TrimEnd('\') -eq $want) { return $false }
        }
        $new = if ($cur) { "$dir;$cur" } else { $dir }
        $key.SetValue('Path', $new, $kind)
    } finally { $key.Close() }
    # Tell Explorer, so terminals started from it see the new PATH.
    try {
        if (-not ('KaInstall.Env' -as [type])) {
            Add-Type -Namespace KaInstall -Name Env -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
        }
        $r = [UIntPtr]::Zero
        [void][KaInstall.Env]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$r)
    } catch { }
    return $true
}

function Finish {
    $exe = Join-Path $dir 'ka.exe'
    $v = Native { & $exe version } -Quiet
    if ($script:NativeExit -ne 0 -or -not $v) { $v = 'ka' }
    Say "Installed $v to $exe"
    if (Add-UserPath) { Say "Added $dir to your user PATH (new terminals pick it up)." }
    if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$dir;$env:Path" }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Say 'Note: ka needs git 2.40 or newer at run time.' }
    exit 0
}

function Have-Go { [bool](Get-Command go -ErrorAction SilentlyContinue) }

function Go-Version { ([string](Native { go version } -Quiet)).Split(' ')[2] }

function Prepare-Dir { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

# Go-Install builds github.com/KanArtist/ka/cmd/ka@REF into $dir.
function Go-Install([string]$why, [string]$ref) {
    if (-not (Have-Go)) { Die $why }
    Prepare-Dir
    Say "Building ka $ref with $(Go-Version)"
    $env:GOBIN = $dir
    Native { go install "github.com/$Repo/cmd/ka@$ref" } | Out-Null
    if ($script:NativeExit -ne 0) { Die 'go install failed' }
    Finish
}

# A checkout of this repository: install what fits its ui\. Piped into iex,
# there is no script directory and the current directory is not a checkout.
$here = $PSScriptRoot
$isCheckout = $here -and (Test-Path (Join-Path $here 'embed.go')) -and (Test-Path (Join-Path $here 'ui\ka-ui.json')) -and
    (Test-Path (Join-Path $here 'go.mod')) -and ((Get-Content (Join-Path $here 'go.mod') -TotalCount 1) -eq "module github.com/$Repo")
if ($isCheckout -and -not $version) {
    $tag = $null
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $out = @(Native { git -C $here describe --tags --exact-match } -Quiet)
        if ($script:NativeExit -eq 0 -and $out) { $tag = [string]$out[0] }
    }
    if ($tag) {
        $tag = $tag.Trim()
        $version = $tag.TrimStart('v')
        Say "This checkout is at $tag"
    } elseif (Have-Go) {
        Prepare-Dir
        Say "This checkout is not on a release tag; building it with $(Go-Version)"
        $env:GOBIN = $dir
        Push-Location $here
        try { Native { go install ./cmd/ka } | Out-Null } finally { Pop-Location }
        if ($script:NativeExit -ne 0) { Die 'go install failed' }
        Finish
    } else {
        Say 'This checkout is not on a release tag and Go is not installed: installing the latest release.'
        Say 'Its web UI may not fit that release; ka then serves the UI built into the binary.'
    }
}

if ($version -in @('main', 'head', 'HEAD')) {
    Go-Install 'building the newest commit needs Go 1.27 or newer (https://go.dev/dl/)' 'main'
}

# A 32-bit PowerShell on 64-bit Windows sees PROCESSOR_ARCHITECTURE=x86.
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($cpu) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { '' } }
$ref = if ($version) { "v$version" } else { 'latest' }
if (-not $arch) {
    Go-Install "there is no ka release for $cpu; install Go 1.27 or newer (https://go.dev/dl/) and run this again" $ref
}
if ($env:KA_FROM_SOURCE -eq '1') {
    Go-Install 'KA_FROM_SOURCE=1 needs Go 1.27 or newer (https://go.dev/dl/)' $ref
}

if (-not $version) {
    # Resolve "latest" once, so every file comes from the same release even
    # while a new one is being published. Like install.sh, follow the
    # releases/latest redirect rather than the rate-limited API.
    $tag = ''
    try {
        $r = Invoke-WebRequest -UseBasicParsing -Method Head -Uri "https://github.com/$Repo/releases/latest"
        # Windows PowerShell 5.1: HttpWebResponse; PowerShell 7: HttpResponseMessage.
        $final = if ($r.BaseResponse.ResponseUri) { $r.BaseResponse.ResponseUri } else { $r.BaseResponse.RequestMessage.RequestUri }
        $tag = [string]$final
    } catch { }
    if ($tag -notmatch '/releases/tag/v([^/?#]+)$') { Die "could not find the latest release of $Repo" }
    $version = $Matches[1]
}
$base = "https://github.com/$Repo/releases/download/v$version"

$haveCosign = [bool](Get-Command cosign -ErrorAction SilentlyContinue)
if (-not $haveCosign -and $env:KA_REQUIRE_SIGNATURE -eq '1') {
    Die 'KA_REQUIRE_SIGNATURE=1 needs cosign to verify the release (https://docs.sigstore.dev/cosign/system_config/installation/)'
}

Prepare-Dir
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ka-install-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $archive = "ka_windows_$arch.zip"
    Say "Downloading ka $version ($archive)"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    } catch {
        # Build from source only when the release has no such archive, not on
        # a network or server error.
        $status = 0
        if ($_.Exception.Response) { $status = [int]$_.Exception.Response.StatusCode }
        if ($status -ne 404) { Die "could not download $base/$archive ($($_.Exception.Message))" }
        if (-not (Have-Go)) { Die "there is no release archive $archive for $version; install Go 1.27 or newer (https://go.dev/dl/) to build it" }
        Say "No release archive $archive for $version; building it from source instead."
        Go-Install '' "v$version"
    }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')
    } catch { Die 'could not download checksums.txt' }

    if ($haveCosign) {
        $bundle = Join-Path $tmp 'checksums.txt.sigstore.json'
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt.sigstore.json" -OutFile $bundle
        } catch { Die 'could not download the checksums signature' }
        # cosign reports "Verified OK" on stderr; only the exit code counts.
        Native {
            cosign verify-blob --bundle $bundle --certificate-identity "$IdentityPrefix$version" `
                --certificate-oidc-issuer $Issuer (Join-Path $tmp 'checksums.txt')
        } -Quiet | Out-Null
        if ($script:NativeExit -ne 0) { Die "checksums.txt failed signature verification for v$version" }
        Say 'Verified the release signature'
    } else {
        Say 'Note: cosign is not installed, so the release signature was NOT verified; the checksum'
        Say '      only shows the archive matches checksums.txt from the same download site.'
        Say '      Install cosign, or set KA_REQUIRE_SIGNATURE=1 to make this an error.'
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
