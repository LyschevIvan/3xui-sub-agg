# 3x-ui Subscription Aggregator

Мульти-пользовательский агрегатор подписок поверх [3x-ui](https://github.com/MHSanaei/3x-ui). Каждый пользователь добавляет свои панели через веб-интерфейс и получает единую ссылку подписки для VPN-клиентов (v2rayTun, v2rayNG, Hiddify и др.).

## Что есть

- **Админ-панель** — единственный bootstrap-админ из конфига. Выдаёт одноразовые ссылки-приглашения на регистрацию и удаляет пользователей.
- **Кабинет пользователя** — добавление/редактирование/удаление своих 3x-ui серверов. У каждого пользователя — свой непубличный prefix подписки.
- **Агрегация VLESS-подписок** — сервис опрашивает панели каждые `refresh_interval`, собирает VLESS конфиги и отдаёт по `/sub/{prefix}/{email}` в base64.
- **Persistent SQLite** — пользователи, серверы, сессии и инвайты лежат в локальной БД.

## Эндпоинты

| Путь | Описание |
|------|----------|
| `GET /` | Редирект: не авторизован → `/login`, авторизован → `/dashboard` |
| `GET/POST /login` | Вход |
| `POST /logout` | Выход |
| `GET/POST /register?token=XXX` | Регистрация по инвайту |
| `GET /dashboard` | Список серверов пользователя + его URL подписки |
| `GET/POST /dashboard/servers/new` | Добавить сервер |
| `GET/POST /dashboard/servers/{id}` | Редактировать сервер |
| `POST /dashboard/servers/{id}/delete` | Удалить сервер |
| `GET /admin` | (только админ) пользователи + инвайты |
| `POST /admin/invites/new` | Создать инвайт |
| `POST /admin/invites/{token}/delete` | Отозвать инвайт |
| `POST /admin/users/{id}/delete` | Удалить пользователя |
| `GET /sub/{prefix}/{email}` | Base64-подписка (публичный, знание prefix+email = доступ) |
| `GET /healthz` | Liveness |

## Быстрый старт

### 1. Конфиг

```bash
cp config.example.yaml config.yaml
```

```yaml
listen: ":8080"
public_url: "https://sub.example.com"   # используется для ссылок-приглашений
cookies_secure: true                    # false для локального dev
refresh_interval: "5m"
request_timeout: "10s"
db_path: "/app/data/data.db"
admin:
  login: "admin"
  password: "${ADMIN_PASSWORD}"
```

Bootstrap-админ создаётся при первом запуске. На боевом держите пароль в env (`ADMIN_PASSWORD=...`).

### 2. Запуск

**Docker Compose (рекомендуется):**
```bash
ADMIN_PASSWORD='supersecret' docker compose up -d
```
Появится volume `xui_data` для SQLite и Caddy получит Let's Encrypt сертификат.

**Локально:**
```bash
go run ./cmd/aggregator -config config.yaml
```

### 3. Первое использование

1. Откройте `https://sub.example.com/login`, войдите как `admin`.
2. Перейдите в «Админ» → «Создать инвайт». Скопируйте ссылку.
3. Передайте ссылку новому пользователю. Он откроет её, зарегистрируется и попадёт в свой дашборд.
4. В дашборде пользователь добавляет свои 3x-ui серверы. Через `refresh_interval` в таблице появится список его клиентов с готовыми subscription URL.

## Архитектура подписок

URL подписки имеет вид `https://sub.example.com/sub/{user_prefix}/{email}`, где `user_prefix` — случайный 24-символьный hex, закреплённый за пользователем. Prefix защищает от перебора email'ов: без знания prefix пользователя невозможно получить чужую подписку.

Сервер ищет все панели этого пользователя, в которых есть клиент с таким email, и склеивает VLESS-ссылки в base64. Если сервер временно недоступен — его последние успешные данные остаются в снапшоте (no flapping).

## Поддерживаемые протоколы

VLESS + (Reality | TLS | None) поверх (TCP | WebSocket | gRPC | xHTTP).

## База данных

SQLite, одна таблица на сущность (`users`, `servers`, `sessions`, `invites`). Миграции автоматические при старте. Для бэкапа достаточно скопировать файл `data.db`.

## Безопасность (что стоит знать)

- Пароли пользователей — bcrypt.
- Cookie сессии — `HttpOnly`, `SameSite=Lax`, `Secure` при `cookies_secure: true`.
- Сессии живут 30 дней, истёкшие чистятся каждый час.
- Инвайты живут 7 дней и сгорают после использования.
- `/sub/{prefix}/{email}` — публичный по дизайну (нужен VPN-клиентам без авторизации). Prefix закрывает enumeration.
- Пароли панелей 3x-ui хранятся в БД в открытом виде. Если компрометация БД критична — используйте FDE на хосте.

## Сборка

```bash
make build
go build ./cmd/aggregator
```
