# Download and try DeployGuard

Get a bundle from [GitHub Releases](https://github.com/AddysEdge/deployguard/releases/latest).
Choose `linux`, `darwin` (macOS) or `windows`, and `amd64` (Intel/AMD) or `arm64`
(Apple Silicon/ARM). Each archive contains the CLI, a local fixture server and a sample workload.
Go, Docker, a database and API keys are not required to run these binaries.

The release workflow tests on Linux and cross-compiles six targets. Source CI also tests on
Windows. A successful cross-build is not a claim that every packaged target was run on hardware.

## Verify and extract

Download `checksums.txt` beside your archive. On Linux, use `sha256sum`; on macOS,
`shasum -a 256`; in PowerShell, `Get-FileHash -Algorithm SHA256 <archive>`. Compare
the value for the exact filename before extraction.

```bash
tar -xzf deployguard_v0.1.0_linux_amd64.tar.gz
./deployguard version
./deployguard validate --config deployguard.yaml
```

Use the filename for your platform. Windows also supports `tar -xzf`; run
`.\deployguard.exe version` and `.\deployguard.exe validate --config deployguard.yaml`.

## Run a local comparison

With Node.js 24 available, the repository also provides a bounded foreground demo
that starts its own loopback APIs and checks the published executable:

```bash
node scripts/demo-release.mjs --binary /path/to/extracted/deployguard
```

On Windows, pass the path to `deployguard.exe`. It records a clean PASS and an
expected regression FAIL, with JSON reports and a transcript under `reports/`.
It requires no Go installation, credentials or external API. The
[published-binary workflow](../.github/workflows/published-demo.yml) verifies this
on Linux, Windows and macOS using the checksummed v0.1.0 release.

Use two unused ports. The sample below compares the healthy baseline to the deliberately
regressed fixture. A FAIL and exit code 1 are the expected result. Choose
`--variant clean` for the candidate to try a passing comparison instead.

### Bash (Linux or macOS)

```bash
./dg-fixture --variant baseline --addr 127.0.0.1:18080 >baseline.log 2>&1 &
baseline_pid=$!
./dg-fixture --variant regressed --addr 127.0.0.1:18081 >candidate.log 2>&1 &
candidate_pid=$!
trap 'kill "$baseline_pid" "$candidate_pid" 2>/dev/null || true' EXIT

curl --fail --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:18080/health
curl --fail --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:18081/health
./deployguard compare --config deployguard.yaml \
  --baseline http://127.0.0.1:18080 --candidate http://127.0.0.1:18081 \
  --report run.json
# Expected exit code: 1. The report is written even when the gate blocks the release.
```

### PowerShell (Windows)

```powershell
function Wait-ForFixture([string]$uri) {
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        try {
            Invoke-RestMethod $uri -TimeoutSec 1 -ErrorAction Stop | Out-Null
            return
        } catch {
            Start-Sleep -Milliseconds 100
        }
    }
    throw "Fixture did not start: $uri"
}

$baseline = Start-Process .\dg-fixture.exe -ArgumentList '--variant','baseline','--addr','127.0.0.1:18080' -WindowStyle Hidden -PassThru
$candidate = Start-Process .\dg-fixture.exe -ArgumentList '--variant','regressed','--addr','127.0.0.1:18081' -WindowStyle Hidden -PassThru
try {
    Wait-ForFixture http://127.0.0.1:18080/health
    Wait-ForFixture http://127.0.0.1:18081/health
    .\deployguard.exe compare --config deployguard.yaml --baseline http://127.0.0.1:18080 --candidate http://127.0.0.1:18081 --report run.json
    # Expected $LASTEXITCODE: 1.
} finally {
    Stop-Process -Id $baseline.Id,$candidate.Id -ErrorAction SilentlyContinue
}
```

The API/latency numbers in a local fixture run describe that run, not production performance.
Only compare origins you own or have authorization to test.

## Existing CI evidence

- [Clean candidate passes](https://github.com/AddysEdge/deployguard/actions/runs/40748712946).
- [Regressed candidate is blocked](https://github.com/AddysEdge/deployguard/actions/runs/37548443328):
  the failed workflow is the intended demonstration of the gate, not broken normal CI.
- [Normal build, tests, race detector and seeded evaluation](https://github.com/AddysEdge/deployguard/actions/workflows/ci.yml).

## Build bundles from source

With Go 1.27+, Bash, tar and sha256sum installed:

```bash
bash scripts/package-release.sh v0.1.0
```

The script writes archives and checksums under the ignored `dist/` directory. A manual run of
the release workflow builds preview artifacts; only a version-tag push publishes a release.
