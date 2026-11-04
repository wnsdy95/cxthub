#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
artifacts="$(mktemp -d "${TMPDIR:-/tmp}/cxthub-doc-range.XXXXXX")"
chmod 700 "$artifacts"
run="cxthub-range-$(date +%Y%m%d%H%M%S)-$$"
postgres="$run-postgres"
load="$run-load"
network="$run-network"
password="$(openssl rand -hex 16)"
memory_limit="${CXT_RANGE_GOMEMLIMIT:-400MiB}"
worker_mib="${CXT_RANGE_WORKER_MIB:-32}"
upload_source="${CXT_RANGE_UPLOAD_SOURCE:-files}"
case "$upload_source" in files|postgres) ;; *) printf 'CXT_RANGE_UPLOAD_SOURCE must be files or postgres\n' >&2; exit 1 ;; esac
case "$worker_mib" in 32|310) ;; *) printf 'CXT_RANGE_WORKER_MIB must be 32 or 310\n' >&2; exit 1 ;; esac
case "${CXT_RANGE_CASE:-mixed}" in
  mixed) phase=load ;;
  baseline) phase=baseline ;;
  cold) phase=cold ;;
  *) printf 'CXT_RANGE_CASE must be mixed, baseline or cold\n' >&2; exit 1 ;;
esac
cleanup() {
  result=$?
  trap - EXIT
  docker logs "$postgres" > "$artifacts/postgres.log" 2>&1 || true
  docker inspect "$postgres" > "$artifacts/postgres-inspect.json" 2>/dev/null || true
  docker inspect "$load" > "$artifacts/load-inspect.json" 2>/dev/null || true
  docker rm -fv "$load" "$postgres" > "$artifacts/cleanup.log" 2>&1 || true
  docker network rm "$network" >> "$artifacts/cleanup.log" 2>&1 || true
  if [ "$result" -ne 0 ]; then
    for log in build-host.log build-linux.log fixture-self-test.log setup.log load.log; do
      if [ -f "$artifacts/$log" ]; then printf '\n--- %s ---\n' "$log"; tail -40 "$artifacts/$log"; fi
    done
  fi
  printf '\nResult=%s; retained local artifacts: %s\n' "$result" "$artifacts"
  exit "$result"
}
trap cleanup EXIT
printf 'Local-only range regression; artifacts=%s GOMEMLIMIT=%s worker_MiB=%s phase=%s upload_source=%s\n' "$artifacts" "$memory_limit" "$worker_mib" "$phase" "$upload_source"
docker info --format '{{.OSType}}/{{.Architecture}} cgroup={{.CgroupVersion}}' > "$artifacts/docker-info.txt"
architecture="$(docker info --format '{{.Architecture}}')"
case "$architecture" in
  aarch64|arm64) architecture=arm64 ;;
  x86_64|amd64) architecture=amd64 ;;
  *) printf 'Unsupported Docker architecture: %s\n' "$architecture" >&2; exit 1 ;;
esac
cd "$root/backend"
go version > "$artifacts/go-version.txt"
git rev-parse HEAD > "$artifacts/git-head.txt"
git diff --binary > "$artifacts/worktree.diff"
git status --short > "$artifacts/git-status.txt"
cp "$root/scripts/test-doc-range-load.sh" "$artifacts/runner.sh"
if [ -n "${CXT_RANGE_REUSE_BUILD:-}" ]; then
  cmp "$CXT_RANGE_REUSE_BUILD/docker-info.txt" "$artifacts/docker-info.txt"
  for file in source.tar.gz source-files.txt source-sha256.txt git-head.txt worktree.diff git-status.txt go-version.txt setup.test load.test; do
    cp "$CXT_RANGE_REUSE_BUILD/$file" "$artifacts/$file"
  done
  printf '%s\n' "$CXT_RANGE_REUSE_BUILD" > "$artifacts/reused-build.txt"
else
python3 - "$root" "$artifacts" <<'PY'
import hashlib
import pathlib
import subprocess
import sys
import tarfile
root, directory = map(pathlib.Path, sys.argv[1:])
listed = subprocess.check_output(['git', '-C', str(root), 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', 'backend', 'schemas/db/migrations', 'scripts/test-doc-range-load.sh'])
selected = []
for encoded in listed.split(b'\0'):
    if not encoded:
        continue
    relative = pathlib.PurePosixPath(encoded.decode())
    if any(part.startswith('.env') or part in ('data', 'tmp', 'secrets', 'node_modules') for part in relative.parts):
        continue
    if relative.suffix not in ('.go', '.sql') and relative.as_posix() not in ('backend/go.mod', 'backend/go.sum', 'scripts/test-doc-range-load.sh'):
        continue
    source = root / relative
    if source.is_file() and not source.is_symlink():
        selected.append(relative.as_posix())
hashes = []
with tarfile.open(directory / 'source.tar.gz', 'w:gz') as archive:
    for relative in sorted(set(selected)):
        archive.add(root / relative, arcname=relative, recursive=False)
with tarfile.open(directory / 'source.tar.gz', 'r:gz') as archive:
    for relative in sorted(set(selected)):
        hashes.append(f'{hashlib.sha256(archive.extractfile(relative).read()).hexdigest()}  {relative}')
(directory / 'source-files.txt').write_text('\n'.join(sorted(set(selected))) + '\n')
(directory / 'source-sha256.txt').write_text('\n'.join(hashes) + '\n')
PY
fi
mkdir -m 700 "$artifacts/source"
tar -xzf "$artifacts/source.tar.gz" -C "$artifacts/source"
cd "$artifacts/source/backend"
if [ -z "${CXT_RANGE_REUSE_BUILD:-}" ]; then
  CGO_ENABLED=0 go test -tags postgres -c -o "$artifacts/setup.test" ./internal/adapters/delivery/http > "$artifacts/build-host.log" 2>&1
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go test -tags postgres -c -o "$artifacts/load.test" ./internal/adapters/delivery/http > "$artifacts/build-linux.log" 2>&1
fi
shasum -a 256 "$artifacts/source.tar.gz" "$artifacts/setup.test" "$artifacts/load.test" "$artifacts/runner.sh" > "$artifacts/sha256.txt"
"$artifacts/setup.test" -test.run '^TestDoc(Range|Visible)Load' -test.v -test.timeout 2m > "$artifacts/fixture-self-test.log" 2>&1
docker network create "$network" > "$artifacts/network.txt"
docker run -d --name "$postgres" --network "$network" --network-alias range-postgres --label "cxthub.range-load=$run" -p 127.0.0.1::5432 -e POSTGRES_USER=range_load -e "POSTGRES_PASSWORD=$password" -e POSTGRES_DB=cxthub_range_load postgres:16 -c shared_buffers=64MB -c max_connections=48 > "$artifacts/postgres-id.txt"
ready=0
for attempt in $(seq 1 60); do
  if docker exec "$postgres" pg_isready -h 127.0.0.1 -U range_load -d cxthub_range_load > /dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
test "$ready" = 1
port="$(docker port "$postgres" 5432/tcp | sed 's/.*://')"
export CXT_RANGE_DISPOSABLE=1 CXT_RANGE_RUN="$run" CXT_RANGE_ARTIFACTS="$artifacts" CXT_RANGE_MIGRATIONS="$artifacts/source/schemas/db/migrations" CXT_RANGE_WORKER_MIB="$worker_mib" CXT_RANGE_UPLOAD_SOURCE="$upload_source"
CXT_RANGE_PHASE=setup CXT_RANGE_DSN="postgres://range_load:$password@127.0.0.1:$port/cxthub_range_load?sslmode=disable" "$artifacts/setup.test" -test.run '^TestPostgresDocRangeMixedLoad$' -test.v -test.timeout 15m > "$artifacts/setup.log" 2>&1
cat "$artifacts/setup.log"
docker image inspect postgres:16 --format '{{.Id}} {{json .RepoDigests}}' > "$artifacts/image.txt"
docker create --name "$load" --network "$network" --label "cxthub.range-load=$run" --memory 512m --memory-swap 512m --mount "type=bind,source=$artifacts,target=/artifacts" -e CXT_RANGE_DISPOSABLE=1 -e "CXT_RANGE_RUN=$run" -e CXT_RANGE_ARTIFACTS=/artifacts -e "CXT_RANGE_PHASE=$phase" -e "CXT_RANGE_UPLOAD_SOURCE=$upload_source" -e "CXT_RANGE_DSN=postgres://range_load:$password@range-postgres:5432/cxthub_range_load?sslmode=disable" -e "GOMEMLIMIT=$memory_limit" --entrypoint /bin/sh postgres:16 -c '
/artifacts/load.test -test.run "^TestPostgresDocRangeMixedLoad$" -test.v -test.timeout 15m > /artifacts/load.log 2>&1
result=$?
for metric in memory.max memory.swap.max memory.peak memory.events memory.current memory.stat; do
  cat "/sys/fs/cgroup/$metric" > "/artifacts/$metric" || exit 98
done
exit "$result"
' > "$artifacts/load-id.txt"
docker start "$load" > /dev/null
docker wait "$load" > "$artifacts/container-exit.txt"
docker inspect "$load" > "$artifacts/load-inspect.json"
cat "$artifacts/load.log"
python3 - "$artifacts" "$phase" <<'PY'
import json
import math
import pathlib
import sys
directory = pathlib.Path(sys.argv[1])
container = json.loads((directory / 'load-inspect.json').read_text())[0]
def read_integer(name):
    target = directory / name
    return int(target.read_text()) if target.exists() else None
def read_values(name):
    target = directory / name
    return {key: int(value) for key, value in (line.split() for line in target.read_text().splitlines())} if target.exists() else {}
def read_json(name, default):
    target = directory / name
    return json.loads(target.read_text()) if target.exists() else default
peak = read_integer('memory.peak')
events = read_values('memory.events')
samples = read_json('samples.json', [])
operations = read_json('operations.json', [])
visible_costs = read_json('visible-costs.json', [])
if visible_costs is None:
    visible_costs = []
assert isinstance(visible_costs, list), 'visible cost observations must be an array'
windows = read_json('work-windows.json', {})
functional = read_json('functional-result.json', {})
def latency(group):
    values = sorted(sample['Microseconds'] for sample in group)
    return {'samples': len(group), 'p50_us': values[(len(values) - 1) // 2], 'p95_us': values[math.ceil(len(values) * .95) - 1], 'max_us': values[-1]}
latency_groups = {}
for sample in samples:
    key = f'{sample["Phase"]}/{sample["Kind"]}/HTTP{sample["Status"]}'
    latency_groups.setdefault(key, []).append(sample)
operation_groups = {}
for operation in operations:
    key = f'{operation["Phase"]}/{operation["Kind"]}'
    operation_groups.setdefault(key, []).append(operation)
overlap = {}
for phase, (start, end) in windows.items():
    overlap[phase] = {}
    for sample in samples:
        if sample['Phase'] != phase or sample['Kind'] in ('chunk_upload', 'submit'):
            continue
        counts = overlap[phase].setdefault(sample['Kind'], {'overlapping_attempts': 0, 'http200_completed_during': 0})
        counts['overlapping_attempts'] += sample['Start'] < end and sample['End'] > start
        counts['http200_completed_during'] += sample['Status'] == 200 and not sample.get('Error') and start < sample['End'] < end
memory_samples = []
sampling_errors = []
memory_path = directory / 'memory-samples.jsonl'
if memory_path.exists():
    for ordinal, line in enumerate(memory_path.read_text().splitlines(), 1):
        try:
            memory_samples.append(json.loads(line))
        except ValueError as error:
            sampling_errors.append(f'line {ordinal}: {error}')
limit = 512 * 1024 * 1024
memory_groups = {phase: [sample for sample in memory_samples if sample['phase'] == phase] for phase in sorted({sample['phase'] for sample in memory_samples})}
def memory_point(sample):
    return {'time': sample['time'], 'phase': sample['phase'], 'current_bytes': sample['cgroup']['memory.current']['bytes'], 'peak_bytes': sample['cgroup']['memory.peak']['bytes'], 'stat': sample['cgroup']['memory.stat'], 'process_bytes': sample['process_bytes'], 'events': sample['cgroup']['memory.events']}
report = {
    'phase': sys.argv[2],
    'runtime_settings': dict(value.split('=', 1) for value in container['Config']['Env'] if value.startswith(('GOMEMLIMIT=', 'CXT_RANGE_UPLOAD_SOURCE='))),
    'test_exit_code': container['State']['ExitCode'],
    'docker_memory_limit_bytes': container['HostConfig']['Memory'],
    'docker_memory_swap_bytes': container['HostConfig']['MemorySwap'],
    'cgroup_memory_max_bytes': read_integer('memory.max'),
    'cgroup_memory_swap_max_bytes': read_integer('memory.swap.max'),
    'functional': functional,
    'functional_pass': functional.get('passed') is True and bool(samples) and all(not sample.get('Error') for sample in samples) and bool(operations) and all(operation['Completed'] for operation in operations),
    'no_oom_pass': not container['State']['OOMKilled'] and all(events.get(key) == 0 for key in ('oom', 'oom_kill')),
    'strict_peak_pass': peak is not None and 0 < peak <= limit,
    'peak_bytes': peak,
    'headroom_bytes': limit - peak if peak is not None else None,
    'docker_oom_killed': container['State']['OOMKilled'],
    'memory_events': events,
    'memory_stat_after_process_exit': read_values('memory.stat'),
    'http_attempts': len(samples),
    'retryable_503': sum(sample['Status'] == 503 for sample in samples),
    'admission_retry_attempts': sum(sample.get('Retry', False) for sample in samples),
    'http_latency_by_kind_status': {key: {**latency(group), 'max_response_bytes': max(sample['Bytes'] for sample in group)} for key, group in sorted(latency_groups.items())},
    'http_operations_including_retry_wait': {key: {**latency(group), 'completed': sum(operation['Completed'] for operation in group), 'failed': sum(not operation['Completed'] for operation in group), 'attempts': sum(operation['Attempts'] for operation in group)} for key, group in sorted(operation_groups.items())},
    'completion_during_overlap': overlap,
    'visible_cost_samples': len(visible_costs),
    'visible_cost_maxima': {name: max((cost[name] for cost in visible_costs), default=None) for name in ('Units', 'Pages', 'Chunks', 'ChunkBytes', 'Forbidden')},
    'worker_admission': read_json('worker-admission.json', {}),
    'process_rss_high_water_bytes': max((sample['process_bytes']['VmHWM'] for sample in memory_samples), default=None),
    'first_observed_over_cap': next((memory_point(sample) for sample in memory_samples if sample['cgroup']['memory.peak']['bytes'] > limit), None),
    'maximum_sampled_rss': memory_point(max(memory_samples, key=lambda sample: sample['process_bytes']['VmRSS'])) if memory_samples else None,
    'sample_count': len(memory_samples),
    'phase_extrema': {phase: {'samples': len(group), 'max_anon_bytes': max(sample['cgroup']['memory.stat']['anon'] for sample in group), 'max_file_bytes': max(sample['cgroup']['memory.stat']['file'] for sample in group), 'max_rss_bytes': max(sample['process_bytes']['VmRSS'] for sample in group), 'max_current_bytes': max(sample['cgroup']['memory.current']['bytes'] for sample in group), 'cumulative_peak_bytes': max(sample['cgroup']['memory.peak']['bytes'] for sample in group)} for phase, group in memory_groups.items()},
    'sampling_errors': sampling_errors,
    'work_windows': windows,
    'interpretation': {
        'kernel_reference': 'https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html#memory-interface-files',
        'memory_max': 'Temporary overages are possible; this regression nevertheless retains its stricter peak <= 512MiB requirement.',
        'events_max': 'Limit pressure is distinct from OOM; inspect oom/oom_kill and Docker OOMKilled separately.',
        'stat': 'file includes filesystem cache; anon includes anonymous mappings. Neither is equivalent to Go heap alone.',
        'scope': 'Server, worker, bounded clients, fixture metadata, verification and sampler share the cgroup. File-cache ownership is not isolated.',
        'sampling': '100ms observations are non-atomic and may miss transients; memory.peak and VmHWM are cumulative high-water marks.',
        'claims': 'Functional and no-OOM success do not override the strict gate or establish production headroom or an SLA.',
        'unmeasured': 'PostgreSQL is outside the app cap. No disk-cold cache reset, production corpus, base/inherited-boundary matrix, sustained traffic, cross-replica behavior, or process-isolated server RSS measurement.',
        'visible_cost_scope': 'Per-request service-to-store port calls and bounded chunk bytes, not SQL count or PostgreSQL memory. Whole-document and legacy fallback ports are rejected before reading.',
        'retries': 'HTTP status groups count attempts, not completed operations. Operation latency includes retry waits; failed/exhausted operations fail the functional gate.'
    }
}
(directory / 'diagnostics.json').write_text(json.dumps(report, indent=2) + '\n')
print(f'Diagnostics: functional_pass={report["functional_pass"]} no_oom_pass={report["no_oom_pass"]} strict_peak_pass={report["strict_peak_pass"]} events.max={events.get("max")} report={directory / "diagnostics.json"}', flush=True)
for key, stats in report['http_latency_by_kind_status'].items():
    print(f'{key}: samples={stats["samples"]} p95_us={stats["p95_us"]}', flush=True)
print(f'Completion during overlap: {json.dumps(overlap, sort_keys=True)}', flush=True)
assert container['HostConfig']['Memory'] == 512 * 1024 * 1024, 'Docker memory cap differs'
assert container['HostConfig']['MemorySwap'] == 512 * 1024 * 1024, 'Docker swap allowance differs'
assert not container['State']['OOMKilled'], 'Docker reports OOMKilled'
assert container['State']['ExitCode'] == 0, f"test failed: exit={container['State']['ExitCode']}"
assert int((directory / 'container-exit.txt').read_text()) == 0, 'docker wait failed'
assert int((directory / 'memory.max').read_text()) == 512 * 1024 * 1024, 'cgroup memory cap differs'
assert int((directory / 'memory.swap.max').read_text()) == 0, 'cgroup swap enabled'
assert 0 < peak <= 512 * 1024 * 1024, f'invalid cgroup peak: {peak}'
assert all(int(events.get(key, -1)) == 0 for key in ('oom', 'oom_kill')), f'OOM events: {events}'
assert samples and all(not sample.get('Error') for sample in samples), 'failed HTTP attempts'
assert report['functional_pass'], 'functional completion proof missing or failed'
assert memory_samples and not sampling_errors, 'memory sampling failed'
marker = {'baseline': 'PASS: isolated baseline', 'cold': 'PASS: isolated cold', 'load': 'PASS: response identities'}[sys.argv[2]]
assert marker in (directory / 'load.log').read_text(), 'completion/checksum proof missing'
print(f'Verified Docker cap=512MiB swap=0 OOMKilled=false peak={peak} bytes ({peak / 1048576:.2f}MiB); HTTP attempts={len(samples)} retryable503={sum(sample["Status"] == 503 for sample in samples)}')
print(f'Headroom={(512 * 1048576 - peak) / 1048576:.2f}MiB; memory.events={events}')
print('PostgreSQL and fixture seeding are outside the application cgroup; bounded HTTP clients are inside. Synthetic compressible data, finite requests, no latency SLA claim.')
print('Graph fixture is 700 linear snapshots, one main branch, no PR/merge topology; production graph concurrency is not established.')
PY
