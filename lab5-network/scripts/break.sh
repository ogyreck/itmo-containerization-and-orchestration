#!/usr/bin/env bash
# Скрытая поломка сетевых политик (часть 6).
# Выбирает случайный файл из faults/ и применяет его поверх values.yaml, не говоря какой.
#
#   ./break.sh            # сломать
#   ./break.sh --reveal   # показать, что было сломано (после диагностики!)
#   ./break.sh --fix      # вернуть чарт к обычному values.yaml
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
CHART="$DIR/../helm"
NS="${NS:-lab5}"
ANSWER="$DIR/.fault-answer"      # имя поломки в base64, чтобы не попалось на глаза случайно

case "${1:-}" in
  --reveal)
    [ -f "$ANSWER" ] || { echo "поломка не применялась"; exit 1; }
    echo "сломано: $(base64 -d "$ANSWER")"; exit 0 ;;
  --fix)
    # --reset-values обязателен: без -f/--set helm берёт user-supplied values прошлой
    # ревизии (неявный --reuse-values), и поломка осталась бы на месте
    helm upgrade shop "$CHART" -n "$NS" --reset-values >/dev/null
    kubectl rollout restart deploy -n "$NS" >/dev/null
    echo "чарт возвращён к обычному values.yaml, поды api и worker пересоздаются"; exit 0 ;;
esac

fault=$(ls "$DIR"/faults/*.yaml | shuf -n1)
basename "$fault" .yaml | base64 > "$ANSWER"

helm upgrade shop "$CHART" -n "$NS" -f "$fault" >/dev/null
# Calico не проверяет пакеты уже установленных соединений, а у api и worker есть пулы
# соединений к Postgres. Без пересоздания подов часть поломок проявилась бы только
# после переподключения пула — через минуты или часы.
kubectl rollout restart deploy -n "$NS" >/dev/null

echo "Что-то сломано. Ищи симптом, потом причину через Whisker."
echo "Когда починишь: ./matrix.sh должен стать зелёным. Проверить себя: ./break.sh --reveal"
