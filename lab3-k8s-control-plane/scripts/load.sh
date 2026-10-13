#!/usr/bin/env bash
# Нагрузка на shop с логом каждого запроса — для части 4 (отказ control plane)
# и rolling update из части 2.
#
#   ./load.sh                                   # http://k3s-w1.lan:32040, лог в load-<время>.log
#   URL=http://k3s-w2.lan:32040 INTERVAL=0.5 ./load.sh my.log
#
# Каждая итерация: GET /health, POST /order, GET /orders?limit=10.
# Строка лога: время | ручка | HTTP-код (000 = нет ответа) | длительность | детали.
# Для /orders в детали пишется, сколько из 10 последних заказов new и processed:
# пока worker жив, new — только 1-3 самых свежих; если он встал, new растёт до 10.
# Ctrl+C — сводка по кодам ответов.

set -u
export LC_NUMERIC=C   # иначе в русской локали printf ждёт дробь через запятую

URL="${URL:-http://k3s-w1.lan:32040}"
INTERVAL="${INTERVAL:-1}"
TIMEOUT="${TIMEOUT:-3}"
LOG="${1:-load-$(date +%Y%m%d-%H%M%S).log}"

# req METHOD PATH [BODY] — выполняет запрос и печатает строку лога
req() {
    local method=$1 path=$2 body=${3:-} out code dur resp detail
    out=$(curl -sS -m "$TIMEOUT" -X "$method" ${body:+-d "$body"} \
        -w $'\n%{http_code} %{time_total}' "$URL$path" 2>&1)
    resp=$(sed '$d' <<<"$out")
    read -r code dur <<<"$(tail -n1 <<<"$out")"
    case "$path" in
        /orders*) detail="new=$(grep -o '"status":"new"' <<<"$resp" | wc -l) processed=$(grep -o '"status":"processed"' <<<"$resp" | wc -l)" ;;
        /order)   detail=$(grep -o -m1 -E '"id":[0-9]+' <<<"$resp") ;;
        *)        detail=$(head -c 80 <<<"$resp" | tr '\n' ' ') ;;
    esac
    # при обрыве соединения curl отдаёт 000, а причину — в stderr, она попадает в resp
    [[ $code == 000 ]] && detail=$(grep -o 'curl: .*' <<<"$resp" | head -1)
    printf '%s | %-6s | %s | %6.3fs | %s\n' \
        "$(date +%H:%M:%S.%3N)" "${path%%\?*}" "$code" "${dur:-0}" "$detail"
}

summary() {
    echo
    echo "=== сводка ($LOG): ручка / код / количество"
    awk -F' \\| ' '{gsub(/ /,"",$2); gsub(/ /,"",$3); n[$2" "$3]++} END {for (k in n) print k, n[k]}' "$LOG" | sort
    exit 0
}
trap summary INT TERM

echo "нагрузка на $URL каждые ${INTERVAL}s, лог: $LOG (Ctrl+C — стоп и сводка)"
while true; do
    {
        req GET /health
        req POST /order '{"item":"load-test"}'
        req GET '/orders?limit=10'
    } | tee -a "$LOG"
    sleep "$INTERVAL"
done
