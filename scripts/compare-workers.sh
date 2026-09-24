#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
command -v docker >/dev/null
command -v k6 >/dev/null

results_dir=$(mktemp -d "${TMPDIR:-/tmp}/fx-quotes-workers.XXXXXX")
project="fx-quotes-compare-$(date +%s)-$$"
export FX_QUOTES_LOAD_HOST_PORT=${FX_QUOTES_LOAD_HOST_PORT:-18081}
export FX_QUOTES_LOAD_MAX_ACTIVE=20 FX_QUOTES_LOAD_LOG_LEVEL=error
export FX_QUOTES_FIXTURE_LATENCY=100ms FX_QUOTES_FIXTURE_RATE=0.9234
export FX_QUOTES_FIXTURE_SOURCE_DATE=2026-09-22 FX_QUOTES_FIXTURE_STATUS=200

compose() {
  docker compose --env-file /dev/null -p "$project" -f compose.load.yaml "$@"
}

cleanup() {
  # Удаляем только стенд с уникальным именем, созданный этим запуском.
  compose down --volumes --remove-orphans
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Результаты: %s\n' "$results_dir"
k6 version > "$results_dir/environment.txt"
docker info --format '{{.OSType}}/{{.Architecture}} CPUs={{.NCPU}} Memory={{.MemTotal}}' >> "$results_dir/environment.txt"
compose build
failed_runs=0

for repetition in 1 2; do
  for workers in 2 8 16 64; do
    export FX_QUOTES_LOAD_WORKER_COUNT=$workers
    compose up -d --wait --wait-timeout 120
    docker update --cpus 1 --memory 512m --memory-swap 512m "$(compose ps -q app)"
    docker update --cpus 2 --memory 512m --memory-swap 512m "$(compose ps -q postgres)"
    docker update --cpus 1 --memory 128m --memory-swap 128m "$(compose ps -q fixture-provider)"
    prefix="$results_dir/workers-$workers-run-$repetition"
    base_url="http://127.0.0.1:$FX_QUOTES_LOAD_HOST_PORT"
    k6 run --quiet -e FX_QUOTES_K6_PROFILE=smoke -e "FX_QUOTES_K6_BASE_URL=$base_url" \
      -e FX_QUOTES_K6_EXPECTED_RATE=0.9234 k6/scripts/async-quotes.js > "$prefix-smoke.log" 2>&1
    case "$workers" in
      2|8) profile=stress ;;
      *) profile=load ;;
    esac
    printf 'Обработчиков: %s, повтор: %s, профиль: %s\n' "$workers" "$repetition" "$profile"
    if ! k6 run --quiet --summary-trend-stats 'avg,p(50),p(95),p(99),max' \
      --summary-export "$prefix.json" \
      -e "FX_QUOTES_K6_BASE_URL=$base_url" -e "FX_QUOTES_K6_PROFILE=$profile" \
      -e FX_QUOTES_K6_EXPECTED_RATE=0.9234 \
      -e FX_QUOTES_K6_START_RATE=100 -e FX_QUOTES_K6_TARGET_RATE=100 \
      -e FX_QUOTES_K6_RAMP_DURATION=1s -e FX_QUOTES_K6_STEADY_DURATION=30s \
      -e FX_QUOTES_K6_RAMP_DOWN_DURATION=1s \
      -e FX_QUOTES_K6_PREALLOCATED_VUS=100 -e FX_QUOTES_K6_MAX_VUS=200 \
      k6/scripts/async-quotes.js > "$prefix.log" 2>&1; then
      compose logs --no-color --tail 100 > "$prefix-containers.log" 2>&1
      printf 'Проверка не прошла. См. %s.log\n' "$prefix" >&2
      failed_runs=$((failed_runs + 1))
    fi
    # Следующий вариант начинает с пустой БД, без истории предыдущего прогона.
    cleanup
  done
done
printf 'Неуспешных прогонов: %s. JSON и журналы: %s\n' "$failed_runs" "$results_dir"
test "$failed_runs" -eq 0
