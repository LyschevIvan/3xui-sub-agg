# 3x-ui Subscription Aggregator

Сервис агрегирует подписки с нескольких [3x-ui](https://github.com/MHSanaei/3x-ui) панелей в единую ссылку для VPN-клиентов (v2rayTun, v2rayNG, Hiddify и др.).

## Как это работает

Сервис периодически опрашивает каждую 3x-ui панель через её HTTP API, собирает все включённые inbound'ы и клиентов, и формирует `vless://` ссылки. По запросу `/sub/{email}` клиент получает base64-подписку со всеми серверами, на которых заведён его email.

```
[v2rayTun] → GET /sub/alice → [Aggregator] → 3x-ui API × N серверов
                                         ↓
                              vless://…@de1…  (alice на DE-1)
                              vless://…@nl1…  (alice на NL-1)
```

**Ключевые свойства:**
- Идентификатор клиента — `email` (имя клиента в 3x-ui). Один и тот же email на разных серверах = одна подписка.
- Если сервер недоступен — клиент получает последние актуальные данные с этого сервера, остальные работают в штатном режиме.
- Если клиента нет на каком-то сервере — этот сервер просто не попадает в его подписку.

**Поддерживаемые протоколы:** VLESS + Reality / TLS / None поверх TCP, xHTTP, gRPC, WebSocket.

## Быстрый старт

### 1. Конфиг

```bash
cp config.example.yaml config.yaml
```

Отредактируйте `config.yaml`:

```yaml
listen: ":8080"
refresh_interval: "5m"
request_timeout: "10s"

servers:
  - name: "DE-1"
    api_url: "http://1.2.3.4:54321"
    path: "/admin/"           # webBasePath панели, с / с обеих сторон
    username: "admin"
    password: "your-password"
    insecure_skip_verify: false
    host_override: "de1.example.com"   # публичный адрес для vless://, если отличается от api_url
```

Пароль можно задать через переменную окружения вместо конфига:
```yaml
password: "${XUI_DE1_PASSWORD}"
```

### 2. Запуск

**Локально:**
```bash
make run
# или явно:
go run ./cmd/aggregator -config config.yaml
```

**Docker:**
```bash
make docker
make docker-run   # монтирует config.yaml из текущей директории
```

**Docker Compose (с Caddy):**
```yaml
services:
  aggregator:
    image: 3xui-sub-agg:dev
    volumes:
      - ./config.yaml:/app/config.yaml:ro
    restart: unless-stopped

  caddy:
    image: caddy:alpine
    ports:
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
    depends_on:
      - aggregator
```

```
# Caddyfile
sub.example.com {
    reverse_proxy aggregator:8080
}
```

### 3. Добавить в VPN-клиент

После запуска ссылка для клиента с email `alice`:
```
http://your-server:8080/sub/alice
```

Добавьте её как subscription URL в v2rayTun / v2rayNG / Hiddify. Клиент сам обновит список серверов по интервалу (заголовок `Profile-Update-Interval: 12` часов).

## HTTP API

| Метод | Путь | Описание |
|-------|------|----------|
| `GET` | `/sub/{email}` | Подписка для клиента (base64) |
| `GET` | `/healthz` | Проверка живости сервиса |
| `GET` | `/debug/snapshot` | Текущее состояние: серверы, клиенты, ошибки |

## Конфигурация

| Параметр | По умолчанию | Описание |
|----------|-------------|----------|
| `listen` | `:8080` | Адрес и порт сервера |
| `refresh_interval` | `5m` | Интервал опроса панелей |
| `request_timeout` | `10s` | Таймаут HTTP-запроса к панели |
| `servers[].name` | — | Отображаемое имя в remark ссылки |
| `servers[].api_url` | — | URL панели 3x-ui |
| `servers[].path` | `/` | webBasePath из настроек панели |
| `servers[].username` | — | Логин администратора |
| `servers[].password` | — | Пароль (поддерживается `${ENV_VAR}`) |
| `servers[].insecure_skip_verify` | `false` | Игнорировать самоподписанный TLS |
| `servers[].host_override` | — | Публичный хост/IP для `vless://`, если отличается от `api_url` |

## Управление серверами

Чтобы **добавить** сервер — добавьте блок в `servers` и перезапустите сервис. Клиенты, чей email есть на новом сервере, автоматически получат новую ссылку при следующем обновлении подписки.

Чтобы **удалить** сервер — уберите блок из конфига и перезапустите. Ссылка пропадёт из подписок.

## Сборка

```bash
make build    # → bin/aggregator
make test     # тесты
make lint     # golangci-lint
make tidy     # go mod tidy
```

## Безопасность

- `config.yaml` содержит пароли от панелей — не коммитьте его, держите права `chmod 600 config.yaml`.
- Сам сервис работает по HTTP — выносите его за HTTPS reverse proxy (Caddy, nginx).
- URL подписки содержит email клиента открытым текстом. Если это неприемлемо — закройте сервис аутентификацией на уровне reverse proxy.
