#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
snapshots="${CXT_READ_PROGRESS_SNAPSHOTS:-100}"
doc_mib="${CXT_READ_PROGRESS_DOC_MIB:-4}"
case "$snapshots" in 100|1000) ;; *) printf 'CXT_READ_PROGRESS_SNAPSHOTS must be 100 or 1000\n' >&2; exit 1 ;; esac
case "$doc_mib" in 4|32) ;; *) printf 'CXT_READ_PROGRESS_DOC_MIB must be 4 or 32\n' >&2; exit 1 ;; esac
if [ -n "${CXT_TEST_DSN:-}" ]; then printf 'Unset CXT_TEST_DSN: this runner creates its own disposable database.\n' >&2; exit 1; fi
for command in docker go git python3 tar openssl shasum; do command -v "$command" > /dev/null; done
artifacts="$(python3 - "${CXT_READ_PROGRESS_ARTIFACTS:-}" <<'PY'
import pathlib
import sys
import tempfile
directory = pathlib.Path(sys.argv[1]) if sys.argv[1] else pathlib.Path(tempfile.mkdtemp(prefix='cxthub-read-progress.'))
if directory.is_symlink() or (directory.exists() and (not directory.is_dir() or any(directory.iterdir()))):
    sys.exit('CXT_READ_PROGRESS_ARTIFACTS must be a new or empty, non-symlink directory')
directory = directory.resolve()
if any(character in str(directory) for character in ',\n\r'):
    sys.exit('Artifact path cannot contain commas or newlines')
directory.mkdir(mode=0o700, parents=True, exist_ok=True)
directory.chmod(0o700)
print(directory)
PY
)"
suffix="$(date -u +%Y%m%d%H%M%S)_$$_$(openssl rand -hex 6)"
run="cxt-read-progress-${suffix//_/-}"
database="cxt_read_progress_$suffix"
postgres="$run-postgres"
app="$run-app"
network="$run-network"
label=cxthub.read-progress.run
password="$(openssl rand -hex 16)"
cleanup() {
  result=$?
  trap - EXIT INT TERM
  for container in "$app" "$postgres"; do
    if [ "$(docker inspect --format "{{index .Config.Labels \"$label\"}}" "$container" 2>/dev/null || true)" = "$run" ]; then
      docker logs "$container" > "$artifacts/${container##"$run-"}.log" 2>&1 || true
      docker inspect "$container" > "$artifacts/${container##"$run-"}-inspect.json" 2>/dev/null || true
      docker rm -fv "$container" >> "$artifacts/cleanup.log" 2>&1 || result=1
    fi
  done
  if [ "$(docker network inspect --format "{{index .Labels \"$label\"}}" "$network" 2>/dev/null || true)" = "$run" ]; then
    docker network rm "$network" >> "$artifacts/cleanup.log" 2>&1 || result=1
  fi
  printf '%s\n' "$result" > "$artifacts/runner-exit.txt"
  if [ "$result" -ne 0 ]; then
    for log in build-linux.log test.log; do
      if [ -f "$artifacts/$log" ]; then printf '\n--- %s ---\n' "$log"; tail -40 "$artifacts/$log"; fi
    done
  fi
  printf '\nResult=%s; retained local artifacts: %s\n' "$result" "$artifacts"
  exit "$result"
}
endpoint="$(docker context inspect "$(docker context show)" --format '{{.Endpoints.docker.Host}}')"
if [ -z "${DOCKER_CONTEXT:-}" ]; then endpoint="${DOCKER_HOST:-$endpoint}"; fi
case "$endpoint" in unix://*) ;; *) printf 'Refusing non-local Docker endpoint: %s\n' "$endpoint" >&2; exit 1 ;; esac
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'Local-only read progress; artifacts=%s snapshots=%s document_MiB=%s\n' "$artifacts" "$snapshots" "$doc_mib"
printf 'run=%s\ndatabase=%s\nendpoint=%s\nsnapshots=%s\ndocument_MiB=%s\n' "$run" "$database" "$endpoint" "$snapshots" "$doc_mib" > "$artifacts/run.txt"
docker version > "$artifacts/docker-version.txt"
docker info --format '{{.OSType}}/{{.Architecture}} cgroup={{.CgroupVersion}}' > "$artifacts/docker-info.txt"
case "$(cat "$artifacts/docker-info.txt")" in
  linux/aarch64\ cgroup=2|linux/arm64\ cgroup=2) architecture=arm64 ;;
  linux/x86_64\ cgroup=2|linux/amd64\ cgroup=2) architecture=amd64 ;;
  *) printf 'Linux Docker with cgroup v2 and amd64/arm64 is required.\n' >&2; exit 1 ;;
esac
git -C "$root" rev-parse HEAD > "$artifacts/git-head.txt"
python3 - "$root" "$artifacts" <<'PY'
import gzip
import hashlib
import io
import pathlib
import subprocess
import sys
import tarfile
root, directory = map(pathlib.Path, sys.argv[1:])
listed = subprocess.check_output(['git', '-C', str(root), 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', 'backend', 'schemas', 'scripts/test-read-progress-load.sh'])
selected = set()
for encoded in listed.split(b'\0'):
    if not encoded:
        continue
    relative = pathlib.PurePosixPath(encoded.decode())
    if any(part.startswith('.env') or part in ('data', 'tmp', 'secrets', 'node_modules') for part in relative.parts):
        continue
    allowed = (relative.parts[0] == 'backend' and relative.suffix == '.go') or (relative.parts[0] == 'schemas' and relative.suffix == '.sql') or str(relative) in ('backend/go.mod', 'backend/go.sum', 'scripts/test-read-progress-load.sh')
    source = root / relative
    if allowed and source.is_file() and not any((root / parent).is_symlink() for parent in (relative, *relative.parents)):
        if '\n' in str(relative) or '\r' in str(relative):
            sys.exit('Source filenames cannot contain newlines')
        selected.add(str(relative))
required = {'backend/go.mod', 'backend/go.sum', 'scripts/test-read-progress-load.sh'}
if not required.issubset(selected) or not any(name.startswith('schemas/db/migrations/') for name in selected):
    sys.exit('Required regular source files are missing from archive allowlist')
hashes = []
with (directory / 'source.tar.gz').open('wb') as output, gzip.GzipFile(filename='', mode='wb', fileobj=output, mtime=0) as compressed, tarfile.open(fileobj=compressed, mode='w') as archive:
    for relative in sorted(selected):
        payload = (root / relative).read_bytes()
        member = tarfile.TarInfo(relative)
        member.size, member.mode = len(payload), 0o644
        archive.addfile(member, io.BytesIO(payload))
        hashes.append(f'{hashlib.sha256(payload).hexdigest()}  {relative}')
(directory / 'source-files.txt').write_text('\n'.join(sorted(selected)) + '\n')
(directory / 'source-sha256.txt').write_text('\n'.join(hashes) + '\n')
(directory / 'git-status.txt').write_bytes(subprocess.check_output(['git', '-C', str(root), 'status', '--short', '--', *sorted(selected)]))
PY
mkdir -m 700 "$artifacts/source" "$artifacts/test-report" "$artifacts/cgroup-before" "$artifacts/cgroup-after"
tar -xzf "$artifacts/source.tar.gz" -C "$artifacts/source"
cp "$artifacts/source/scripts/test-read-progress-load.sh" "$artifacts/runner.sh"
(
  cd "$artifacts/source/backend"
  export CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" GOWORK=off GOENV=off GOFLAGS=
  go version > "$artifacts/go-version.txt"
  go env GOOS GOARCH CGO_ENABLED GOWORK GOMOD GOTOOLCHAIN > "$artifacts/go-build-env.txt"
  go test -mod=readonly -trimpath -buildvcs=false -tags postgres -c -o "$artifacts/app.test" ./internal/app > "$artifacts/build-linux.log" 2>&1
  go version -m "$artifacts/app.test" > "$artifacts/binary-build-info.txt"
)
(cd "$artifacts" && shasum -a 256 source.tar.gz app.test runner.sh > sha256.txt)
docker network create --internal --label "$label=$run" "$network" > "$artifacts/network-id.txt"
docker run -d --name "$postgres" --network "$network" --network-alias read-progress-postgres --label "$label=$run" -e POSTGRES_USER=read_progress -e "POSTGRES_PASSWORD=$password" -e "POSTGRES_DB=$database" postgres:16 -c max_connections=48 -c shared_buffers=64MB > "$artifacts/postgres-id.txt"
image_id="$(docker inspect --format '{{.Image}}' "$postgres")"
docker image inspect "$image_id" --format '{{.Id}} {{json .RepoDigests}}' > "$artifacts/postgres-image.txt"
ready=0
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$postgres" pg_isready -h 127.0.0.1 -U read_progress -d "$database" > /dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then printf 'Disposable PostgreSQL did not become ready.\n' >&2; exit 1; fi
docker create --name "$app" --network "$network" --label "$label=$run" --memory 512m --memory-swap 512m --cgroupns private --workdir /artifacts/source/backend/internal/app --mount "type=bind,source=$artifacts,target=/artifacts" -e CXT_READ_PROGRESS=1 -e "CXT_READ_PROGRESS_SNAPSHOTS=$snapshots" -e "CXT_READ_PROGRESS_DOC_MIB=$doc_mib" -e CXT_READ_PROGRESS_ARTIFACTS=/artifacts/test-report -e "CXT_TEST_DSN=postgres://read_progress:$password@read-progress-postgres:5432/$database?sslmode=disable" -e GOMEMLIMIT=400MiB --entrypoint /bin/sh "$image_id" -c '
snapshot() {
  for metric in memory.current memory.peak memory.events memory.stat memory.max memory.swap.max; do
    cat "/sys/fs/cgroup/$metric" > "/artifacts/$1/$metric" || return 98
  done
}
snapshot cgroup-before || exit 98
pwd > /artifacts/working-directory.txt
cat /proc/self/cgroup > /artifacts/launcher-cgroup.txt || exit 98
printf "Fixture seeding and test execute in one app.test process inside the 512MiB application cgroup; PostgreSQL is separate.\n" > /artifacts/execution-scope.txt
/artifacts/app.test -test.run "^TestPGReadModelMixedProgress$" -test.v -test.timeout 10m -test.count 1 > /artifacts/test.log 2>&1 &
test_pid=$!
printf "%s\n" "$test_pid" > /artifacts/test-pid.txt
cat "/proc/$test_pid/cgroup" > /artifacts/test-cgroup.txt || exit 98
wait "$test_pid"
result=$?
printf "%s\n" "$result" > /artifacts/test-exit.txt
snapshot cgroup-after || exit 98
exit "$result"
' > "$artifacts/app-id.txt"
docker start "$app" > /dev/null
docker wait "$app" > "$artifacts/container-exit.txt"
docker inspect "$app" > "$artifacts/app-inspect.json"
docker inspect "$postgres" > "$artifacts/postgres-inspect.json"
if [ -f "$artifacts/test.log" ]; then cat "$artifacts/test.log"; fi
python3 - "$artifacts" <<'PY'
import json
import pathlib
import sys
directory = pathlib.Path(sys.argv[1])
app = json.loads((directory / 'app-inspect.json').read_text())[0]
postgres = json.loads((directory / 'postgres-inspect.json').read_text())[0]
def read_text(name):
    target = directory / name
    return target.read_text().strip() if target.is_file() else ''
def read_integer(name):
    value = read_text(name)
    return int(value) if value.isdigit() else None
def read_values(name):
    return {key: int(value) for key, value in (line.split() for line in read_text(name).splitlines())}
limit = 512 * 1024 * 1024
peak = read_integer('cgroup-after/memory.peak')
events = read_values('cgroup-after/memory.events')
reports = sorted((directory / 'test-report').rglob('*.json'))
checks = {
    'container_exit': app['State']['ExitCode'] == 0 and read_integer('container-exit.txt') == 0,
    'test_exit': read_integer('test-exit.txt') == 0,
    'test_ran': '--- PASS: TestPGReadModelMixedProgress (' in read_text('test.log'),
    'no_oom': not app['State']['OOMKilled'] and not postgres['State']['OOMKilled'] and all(events.get(key) == 0 for key in ('oom', 'oom_kill')) and events.get('oom_group_kill', 0) == 0,
    'strict_peak': peak is not None and 0 < peak <= limit,
    'docker_limits': app['HostConfig']['Memory'] == limit and app['HostConfig']['MemorySwap'] == limit,
    'cgroup_limits': all(read_integer(f'{phase}/memory.max') == limit and read_integer(f'{phase}/memory.swap.max') == 0 for phase in ('cgroup-before', 'cgroup-after')),
    'memory_evidence': all(read_integer(f'{phase}/memory.current') is not None and bool(read_values(f'{phase}/memory.stat')) and bool(read_values(f'{phase}/memory.events')) for phase in ('cgroup-before', 'cgroup-after')),
    'same_cgroup': bool(read_text('test-cgroup.txt')) and read_text('test-cgroup.txt') == read_text('launcher-cgroup.txt'),
    'working_directory': read_text('working-directory.txt') == '/artifacts/source/backend/internal/app',
    'test_reports': bool(reports),
}
for report in reports:
    try:
        json.loads(report.read_text())
    except (ValueError, OSError):
        checks['test_reports'] = False
diagnostics = {
    'passed': all(checks.values()), 'checks': checks,
    'peak_bytes': peak, 'memory_events': events,
    'memory_current_bytes': read_integer('cgroup-after/memory.current'),
    'memory_stat': read_values('cgroup-after/memory.stat'),
    'container_exit_code': app['State']['ExitCode'], 'oom_killed': app['State']['OOMKilled'],
    'reports': [str(report.relative_to(directory)) for report in reports],
    'scope': read_text('execution-scope.txt'),
}
(directory / 'diagnostics.json').write_text(json.dumps(diagnostics, indent=2) + '\n')
print(f'Checks={json.dumps(checks, sort_keys=True)}; peak_bytes={peak}; diagnostics={directory / "diagnostics.json"}')
sys.exit(0 if diagnostics['passed'] else 1)
PY
