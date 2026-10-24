#!/usr/bin/env bash
# Непрерывный хаос: раз в INTERVAL секунд штатно выселяет случайный под api или worker.
# Лог (время, под, нода, IP) нужен, чтобы потом сопоставить смерти подов с графиком k6.
#
#   ./chaos.sh                  # бесконечно, Ctrl+C для остановки
#   INTERVAL=10 ./chaos.sh
#
# api   — через Eviction API (как kubectl drain): если выселение нарушит PDB
#         (maxUnavailable: 1), API-сервер вернёт 429 и под останется жить.
# worker — PDB нет, поэтому своя проверка: не трогаем, если Ready меньше MIN_READY_WORKER.
set -euo pipefail

NS="${NS:-lab5}"
SELECTOR='app.kubernetes.io/name in (shop-api,shop-worker)'
LOG="${LOG:-chaos-$(date +%Y%m%d-%H%M%S).log}"

# Замена api становится Ready за ~8 с, каждое выселение — минус одна реплика на это время.
# 7 с — «каждые несколько секунд»; от провала до нуля api страхует PDB.
INTERVAL="${INTERVAL:-7}"
MIN_READY_WORKER="${MIN_READY_WORKER:-2}"

log() { echo "$(date -Is) $*" | tee -a "$LOG"; }
trap 'log "остановлено"; echo "лог: $LOG"; exit 0' INT TERM

# Поды сервиса, которые Ready и ещё не удаляются
ready_count() {
  kubectl get pods -n "$NS" -l "app.kubernetes.io/name=$1" -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{range .status.conditions}}{{if and (eq .type "Ready") (eq .status "True")}}x{{"\n"}}{{end}}{{end}}{{end}}{{end}}' | grep -c x || true
}

evict() {
  kubectl create --raw "/api/v1/namespaces/$NS/pods/$1/eviction" -f - 2>&1 >/dev/null <<<"{\"apiVersion\":\"policy/v1\",\"kind\":\"Eviction\",\"metadata\":{\"name\":\"$1\",\"namespace\":\"$NS\"}}"
}

log "старт: ns=$NS interval=${INTERVAL}s"
while true; do
  # Кандидаты: ещё не Terminating (у них нет deletionTimestamp)
  mapfile -t pods < <(kubectl get pods -n "$NS" -l "$SELECTOR" \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{index .metadata.labels "app.kubernetes.io/name"}} {{.metadata.name}} {{.spec.nodeName}} {{.status.podIP}}{{"\n"}}{{end}}{{end}}')

  if (( ${#pods[@]} == 0 )); then
    log "нет живых подов, жду"; sleep "$INTERVAL"; continue
  fi

  read -r svc name node ip <<<"$(printf '%s\n' "${pods[@]}" | shuf -n1)"

  case "$svc" in
    shop-api)
      if err=$(evict "$name"); then
        log "evicted $name node=$node ip=$ip"
      elif grep -q 'disruption budget' <<<"$err"; then
        log "skip $name: PDB не разрешает (429)"
      else
        log "error $name: $err"
      fi
      ;;
    shop-worker)
      ready=$(ready_count shop-worker)
      if (( ready < MIN_READY_WORKER )); then
        log "skip $name: ready worker=$ready < $MIN_READY_WORKER"
      else
        kubectl delete pod -n "$NS" "$name" --wait=false >/dev/null
        log "deleted $name node=$node ip=$ip (ready worker было $ready)"
      fi
      ;;
  esac

  sleep "$INTERVAL"
done
