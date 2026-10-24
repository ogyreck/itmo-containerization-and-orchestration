#!/usr/bin/env bash
# Проверка матрицы доступа вживую (часть 5, и после починки в части 6).
# Из каждого источника пробует каждого получателя и сводит «ожидаемо / фактически».
#
#   ./matrix.sh            # таблица в stdout, код возврата 1, если есть расхождения
#   ./matrix.sh > matrix.md
#
# Как трактуется результат nc -z:
#   соединение установлено  -> ALLOW
#   connection refused      -> ALLOW (пакет дошёл до пода, просто порт никто не слушает)
#   таймаут                 -> DENY  (пакет молча выброшен политикой)
#
# Источники-поды проверяются через эфемерный контейнер netshoot (kubectl debug --target):
# он живёт в network namespace пода, и на него действуют те же политики. Один контейнер
# на под за прогон. Удалить эфемерный контейнер нельзя — он уйдёт при пересоздании пода.
set -uo pipefail

NS="${NS:-lab5}"
IMG="${IMG:-nicolaka/netshoot:v0.13}"
NODEPORT="${NODEPORT:-32040}"
NODES="${NODES:-192.168.1.22 192.168.1.23}"
PROM="${PROM:-http://k3s-mon.lan:32001}"
RUN="mx$(date +%H%M%S)"

pod_by() { kubectl get pods -n "$NS" -l "app.kubernetes.io/name=$1" \
  -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' | awk '{print $1}'; }
ip_of()  { kubectl get pod -n "$1" "$2" -o jsonpath='{.status.podIP}'; }

API=$(pod_by shop-api); WORKER=$(pod_by shop-worker); PG=$(pod_by shop-postgres)
API_IP=$(ip_of "$NS" "$API"); WORKER_IP=$(ip_of "$NS" "$WORKER"); PG_IP=$(ip_of "$NS" "$PG")
DNS_IP=$(kubectl get svc -n kube-system kube-dns -o jsonpath='{.spec.clusterIP}')
APISERVER=$(kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].endpoints[0].addresses[0]}')

# Матрица: получатель|адрес|порт|ожидание для каждого источника.
# dns проверяется резолвом имени, остальное — TCP-соединением.
read -r -d '' TARGETS_API <<EOF
Postgres|$PG_IP|5432|ALLOW
Postgres (статус CNPG)|$PG_IP|8000|DENY
Postgres (метрики)|$PG_IP|9187|DENY
worker|$WORKER_IP|8080|DENY
CoreDNS|$DNS_IP|dns|ALLOW
API-сервер|$APISERVER|6443|DENY
интернет|1.1.1.1|443|DENY
EOF
read -r -d '' TARGETS_WORKER <<EOF
Postgres|$PG_IP|5432|ALLOW
api|$API_IP|8080|DENY
CoreDNS|$DNS_IP|dns|ALLOW
API-сервер|$APISERVER|6443|DENY
интернет|1.1.1.1|443|DENY
EOF
read -r -d '' TARGETS_PG <<EOF
api|$API_IP|8080|DENY
worker|$WORKER_IP|8080|DENY
CoreDNS|$DNS_IP|dns|ALLOW
API-сервер|$APISERVER|6443|ALLOW
интернет|1.1.1.1|443|DENY
EOF
read -r -d '' TARGETS_STRAY_NS <<EOF
Postgres|$PG_IP|5432|DENY
api|$API_IP|8080|DENY
worker|$WORKER_IP|8080|DENY
CoreDNS|$DNS_IP|dns|ALLOW
интернет|1.1.1.1|443|DENY
EOF
read -r -d '' TARGETS_STRAY_OTHER <<EOF
Postgres|$PG_IP|5432|DENY
api|$API_IP|8080|DENY
worker|$WORKER_IP|8080|DENY
EOF

# Скрипт, который выполняется внутри netshoot: на вход строки «имя|адрес|порт|ожидание»,
# на выход «имя|порт|ожидание|факт|подробность».
PROBE='
while IFS="|" read -r name addr port exp; do
  [ -z "$name" ] && continue
  if [ "$port" = dns ]; then
    a=$(dig +time=2 +tries=1 +short @"$addr" kube-dns.kube-system.svc.cluster.local 2>&1)
    if echo "$a" | grep -qE "^[0-9.]+$"; then echo "$name|53/udp|$exp|ALLOW|резолв ок"
    else echo "$name|53/udp|$exp|DENY|нет ответа"; fi
  else
    out=$(nc -zv -w 2 "$addr" "$port" 2>&1); rc=$?
    if [ $rc -eq 0 ]; then echo "$name|$port|$exp|ALLOW|соединение"
    elif echo "$out" | grep -qi refused; then echo "$name|$port|$exp|ALLOW|refused, порт не слушается"
    else echo "$name|$port|$exp|DENY|таймаут"; fi
  fi
done'

ROWS=()
add_rows() { # $1 — подпись источника, stdin — вывод PROBE
  while IFS='|' read -r name port exp act det; do
    [ -z "$name" ] && continue
    ROWS+=("$1|$name|$port|$exp|$act|$det")
  done
}

# Команда для netshoot: цели и проверяющий код упакованы в base64 — так не нужно
# экранировать кавычки при передаче через kubectl.
job_cmd() { # $1 — цели
  local b64; b64=$(printf '%s\n%s\n' "PROBE_INPUT=\$(cat <<'__T__'
$1
__T__
)" 'printf "%s\n" "$PROBE_INPUT" | (
'"$PROBE"'
)' | base64 -w0)
  echo "echo $b64 | base64 -d | sh"
}

# Источник — существующий под: эфемерный контейнер с теми же политиками, что у пода
probe_pod() { # $1 подпись, $2 под, $3 контейнер-цель, $4 цели
  local c="$RUN"
  kubectl debug -n "$NS" "$2" --image="$IMG" --target="$3" -c "$c" --profile=general -q \
    -- sh -c "$(job_cmd "$4")" >/dev/null 2>&1 \
    || { echo "не удалось создать эфемерный контейнер в $2" >&2; return; }
  for _ in $(seq 1 90); do
    st=$(kubectl get pod -n "$NS" "$2" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"$c\")].state.terminated.reason}")
    [ -n "$st" ] && break; sleep 1
  done
  add_rows "$1" < <(kubectl logs -n "$NS" "$2" -c "$c" 2>/dev/null)
}

# Источник — временный «посторонний» под без наших меток
probe_stray() { # $1 подпись, $2 namespace, $3 цели
  local p="stray-$RUN"
  kubectl run "$p" -n "$2" --image="$IMG" --restart=Never --labels=role=stray \
    --command -- sh -c "$(job_cmd "$3")" >/dev/null 2>&1
  kubectl wait -n "$2" --for=jsonpath='{.status.phase}'=Succeeded pod/"$p" --timeout=120s >/dev/null 2>&1
  add_rows "$1" < <(kubectl logs -n "$2" "$p" 2>/dev/null)
  kubectl delete pod -n "$2" "$p" --wait=false >/dev/null 2>&1
}

echo "Проверяю: api=$API worker=$WORKER pg=$PG" >&2
probe_pod   "api"                       "$API"    shop-api    "$TARGETS_API"
probe_pod   "worker"                    "$WORKER" shop-worker "$TARGETS_WORKER"
probe_pod   "shop-pg"                   "$PG"     postgres    "$TARGETS_PG"
probe_stray "посторонний под (ns $NS)"  "$NS"     "$TARGETS_STRAY_NS"
probe_stray "посторонний под (ns default)" default "$TARGETS_STRAY_OTHER"

# Внешний клиент: ноутбук -> NodePort каждой ноды
for n in $NODES; do
  code=$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://$n:$NODEPORT/health")
  if [ "$code" = 200 ]; then ROWS+=("клиент (ноутбук)|api через NodePort $n|$NODEPORT|ALLOW|ALLOW|HTTP 200")
  else ROWS+=("клиент (ноутбук)|api через NodePort $n|$NODEPORT|ALLOW|DENY|нет ответа"); fi
done

# Косвенные проверки: источники, из которых команду не запустишь
up=$(curl -s -m 5 -G "$PROM/api/v1/query" --data-urlencode "query=min(up{namespace=\"$NS\"})" \
  | grep -oE '"value":\[[^]]*"[01]"' | grep -oE '"[01]"$' | tr -d '"')
[ "$up" = 1 ] && act=ALLOW det="все цели lab5 up=1" || act=DENY det="есть цели up=0 или нет данных"
ROWS+=("Prometheus|api, worker (/metrics)|8080|ALLOW|$act|$det")

phase=$(kubectl get cluster.postgresql.cnpg.io -n "$NS" -o jsonpath='{.items[0].status.phase}')
[ "$phase" = "Cluster in healthy state" ] && act=ALLOW || act=DENY
ROWS+=("CNPG operator|Postgres (статус)|8000|ALLOW|$act|$phase")

notready=$(kubectl get pods -n "$NS" -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}' | grep -vc True)
[ "$notready" = 0 ] && act=ALLOW det="все поды Ready" || act=DENY det="не Ready: $notready"
ROWS+=("kubelet (пробы)|все поды lab5|8080, 8000|ALLOW|$act|$det")

# Сводка
fail=0
echo "| Источник | Получатель | Порт | Ожидание | Факт | | Подробности |"
echo "| :--- | :--- | :---: | :---: | :---: | :---: | :--- |"
for r in "${ROWS[@]}"; do
  IFS='|' read -r src dst port exp act det <<<"$r"
  if [ "$exp" = "$act" ]; then mark="✅"; else mark="❌"; fail=$((fail+1)); fi
  echo "| $src | $dst | $port | $exp | $act | $mark | $det |"
done
echo
echo "Проверок: ${#ROWS[@]}, расхождений: $fail"
[ "$fail" -eq 0 ]
