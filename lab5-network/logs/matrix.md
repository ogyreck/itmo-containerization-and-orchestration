| Источник | Получатель | Порт | Ожидание | Факт | | Подробности |
| :--- | :--- | :---: | :---: | :---: | :---: | :--- |
| api | Postgres | 5432 | ALLOW | ALLOW | ✅ | соединение |
| api | Postgres (статус CNPG) | 8000 | DENY | DENY | ✅ | таймаут |
| api | Postgres (метрики) | 9187 | DENY | DENY | ✅ | таймаут |
| api | worker | 8080 | DENY | DENY | ✅ | таймаут |
| api | CoreDNS | 53/udp | ALLOW | DENY | ❌ | нет ответа |
| api | API-сервер | 6443 | DENY | DENY | ✅ | таймаут |
| api | интернет | 443 | DENY | DENY | ✅ | таймаут |
| worker | Postgres | 5432 | ALLOW | ALLOW | ✅ | соединение |
| worker | api | 8080 | DENY | DENY | ✅ | таймаут |
| worker | CoreDNS | 53/udp | ALLOW | DENY | ❌ | нет ответа |
| worker | API-сервер | 6443 | DENY | DENY | ✅ | таймаут |
| worker | интернет | 443 | DENY | DENY | ✅ | таймаут |
| shop-pg | api | 8080 | DENY | DENY | ✅ | таймаут |
| shop-pg | worker | 8080 | DENY | DENY | ✅ | таймаут |
| shop-pg | CoreDNS | 53/udp | ALLOW | DENY | ❌ | нет ответа |
| shop-pg | API-сервер | 6443 | ALLOW | ALLOW | ✅ | соединение |
| shop-pg | интернет | 443 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns lab5) | Postgres | 5432 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns lab5) | api | 8080 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns lab5) | worker | 8080 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns lab5) | CoreDNS | 53/udp | ALLOW | DENY | ❌ | нет ответа |
| посторонний под (ns lab5) | интернет | 443 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns default) | Postgres | 5432 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns default) | api | 8080 | DENY | DENY | ✅ | таймаут |
| посторонний под (ns default) | worker | 8080 | DENY | DENY | ✅ | таймаут |
| клиент (ноутбук) | api через NodePort 192.168.1.22 | 32040 | ALLOW | ALLOW | ✅ | HTTP 200 |
| клиент (ноутбук) | api через NodePort 192.168.1.23 | 32040 | ALLOW | ALLOW | ✅ | HTTP 200 |
| Prometheus | api, worker (/metrics) | 8080 | ALLOW | ALLOW | ✅ | все цели lab5 up=1 |
| CNPG operator | Postgres (статус) | 8000 | ALLOW | ALLOW | ✅ | Cluster in healthy state |
| kubelet (пробы) | все поды lab5 | 8080, 8000 | ALLOW | ALLOW | ✅ | все поды Ready |

Проверок: 30, расхождений: 4
