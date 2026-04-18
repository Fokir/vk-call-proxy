# ██ Call Proxy

> **Disclaimer:** Данный проект создан исключительно в учебных и исследовательских целях.
> Использование инфраструктуры VK Calls (TURN-серверов) без явного разрешения со стороны правообладателя может нарушать Условия использования сервиса и правила платформы VK. Автор проекта не несет ответственности за любой ущерб или нарушение правил, возникшее в результате использования данного программного обеспечения. Проект демонстрирует техническую возможность интеграции протоколов и не предназначен для нецелевого использования ресурсов сторонних сервисов.

> **⚠ Текущее ограничение:** VK обновил тип капчи — автоматическое решение в Docker (headless Chrome) временно не работает. Relay-to-relay mode в Docker требует ручного решения капчи. Direct mode и desktop-клиент используют `InteractiveSolver` — открывается видимое окно Chrome для ручного прохождения.

Весь трафик проходит через ████ █████ серверы, шифруется DTLS 1.2 и мультиплексируется в единый туннель. Для внешнего наблюдателя это выглядит как обычный ██████████.

---

## Архитектура

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                                                             │
│   Клиент                     ██ Cloud                      VPN-сервер       │
│  ┌─────────┐              ┌───────────┐               ┌──────────────┐      │
│  │ Browser │              │           │               │              │      │
│  │   App   ├──► SOCKS5 ──►│   TURN    │──► UDP ──────►│  :9000/udp   │      │
│  │         │    HTTP      │   Relay   │    DTLS       │  DTLS + MUX  ├──►Internet
│  └─────────┘  :1080/:8080 │           │               │              │      │
│                           └───────────┘               └──────────────┘      │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Как это работает

Система поддерживает три режима:

**Direct mode** — клиент подключается к серверу через ██ ████ █████:

1. Клиент получает TURN credentials через ██ ████ ███
2. Создаёт **N параллельных** TURN allocations (по умолчанию 4)
3. Поверх каждого устанавливает **DTLS 1.2** шифрование (AES-128-GCM)
4. Все соединения объединяются **мультиплексором** в единый туннель
5. Сервер принимает потоки и проксирует TCP-трафик в интернет

**Relay-to-relay mode** — оба узла подключаются через ██ ████ ██████ (сервер не нуждается в открытом порте):

1. Клиент и сервер join'ят один и тот же ██-звонок по ссылке
2. Оба создают TURN allocations внутри ██-инфраструктуры
3. Обмениваются relay-адресами через **██ WebSocket signaling** (зашифровано AES-256-GCM)
4. Устанавливают DTLS relay-to-relay соединения между TURN-серверами ██
5. Мультиплексор объединяет всё в туннель

**Dual mode (direct + relay)** — сервер одновременно принимает оба типа подключений:

1. Сервер слушает на UDP-порту **и** подключается к ██-звонку
2. Direct-клиенты подключаются через TURN → `:9000/udp`
3. Relay-клиенты подключаются через ██-инфраструктуру
4. Включается флагом `--direct` или env `ALSO_DIRECT=1`

### Поток данных

```
# Direct mode
App → SOCKS5/HTTP → MUX → DTLS → ████ █████ (██) → Server:9000/UDP → DTLS → MUX → Internet

# Relay-to-relay mode
App → MUX → DTLS → TURN(client) ↔ TURN(server) → DTLS → MUX → Internet
                        ██ signaling (WebSocket)
```

---

## Компоненты

| Компонент | Описание | Платформы |
|:----------|:---------|:----------|
| **Сервер** | DTLS/UDP listener, группировка сессий, проксирование | Linux (Docker) |
| **Captcha-сервис** | Автоматическое решение ██ капчи через headless Chrome | Linux (Docker) |
| **Scripts-updater** | Sidecar, качает подписанные hot-update скрипты в shared volume | Linux (Docker) |
| **Desktop-клиент** | SOCKS5 + HTTP прокси, TURN + DTLS туннель | Windows, macOS |
| **Android** | Нативное приложение с gomobile, self-update APK через `PackageInstaller` | Android 7+ |
| **iOS** | Нативное приложение с PacketTunnel | iOS 15+ [не реализованно полноценно] |

---

## Hot-update скриптов

VK-капча, параметры авторизации и User-Agent пулы меняются часто. Чтобы не перевыпускать релизы и не переустанавливать APK при каждом обновлении VK, критичные значения вынесены в отдельный каталог [`hot-scripts/`](hot-scripts/) и подгружаются клиентами в рантайме.

**Что там хранится:**

| Файл | Содержимое |
|:-----|:-----------|
| `hot-scripts/vk-config.json` | API-версии VK, endpoints, UA-pool, WS-параметры, captcha-константы (`checkbox_answer`, `debug_info`, селекторы) |
| `hot-scripts/stealth.js` | JS-инъекция для headless Chrome (anti-detection) |
| `hot-scripts/manifest.json` | Подписанный Ed25519 индекс: версия, SHA-256 каждого файла, опциональный блок `apk` для Android self-update |

**Как это работает:**

1. Все бинари имеют bundled-копию скриптов ([`internal/scripts/bundled/`](internal/scripts/bundled/)) через `//go:embed` — работает без сети при первом запуске.
2. [`internal/scripts.Manager`](internal/scripts/) периодически (раз в час + по ошибкам) тянет `manifest.json` с `raw.githubusercontent.com/Fokir/vk-call-proxy/master/hot-scripts/`.
3. Подпись Ed25519 проверяется встроенным публичным ключом (вшивается через ldflags в CI).
4. При успехе скрипты сохраняются атомарно в локальный кэш, старая версия держится для rollback.
5. Если новая версия вызвала 3 ошибки за 5 минут — автоматический откат + quarantine.
6. В Docker `scripts-updater` работает sidecar'ом и пишет в shared volume; server + captcha-service читают оттуда через mtime-polling.
7. На Android `Tunnel.initScripts()` поднимает manager; `UpdateManager` сверяет `manifest.apk.version` с `BuildConfig.VERSION_NAME` и показывает кнопку «Обновить» в UI.

**Обновление без релиза:**

```bash
# 1. Отредактировать hot-scripts/vk-config.json или stealth.js
# 2. Подписать и закоммитить
make bundle                     # синхронизирует internal/scripts/bundled/
git add hot-scripts/ internal/scripts/bundled/
git commit -m "chore(scripts): bump VK API version"
git push
```

GitHub Actions workflow [`scripts-publish.yml`](.github/workflows/scripts-publish.yml) пере-подписывает `manifest.json` и коммитит обратно с `[skip ci]`. Клиенты подтягивают изменения при следующей проверке.

**Генерация ключей (один раз):**

```bash
make keygen                     # создаёт secrets/scripts-signing.{key,pub}
# Приватный ключ → GitHub Secret SCRIPTS_SIGNING_KEY
# Публичный → env SCRIPTS_PUBKEY в .github/workflows/release.yml
```

**Override URL источника:**

- CLI: `--scripts-url=https://...` / `--scripts-pubkey=<base64>`
- Env: `CALLVPN_SCRIPTS_URL`, `CALLVPN_SCRIPTS_PUBKEY`, `CALLVPN_SCRIPTS_DIR`
- Compile-time: `-ldflags "-X github.com/call-vpn/call-vpn/internal/scripts.DefaultURL=... -X github.com/call-vpn/call-vpn/internal/scripts.DefaultPublicKey=..."`

Подробнее: `internal/scripts/` + `tools/scripts-sign/` + `cmd/scripts-updater/`.

---

## Быстрый старт

### Сервер (Docker)

#### Direct mode — сервер слушает на UDP порту

```env
# .env
IMAGE_TAG=latest
LISTEN_PORT=9000
VPN_TOKEN=your-secret-token
```

```bash
docker compose up -d
```

#### Relay-to-relay mode — сервер подключается через ██-звонок

```env
# .env
IMAGE_TAG=latest
VK_CALL_LINK=AbCdEf123456
VPN_TOKEN=your-secret-token
TURN_CONNS=4
```

```bash
docker compose up -d
```

> При указании `VK_CALL_LINK` сервер автоматически переключается в relay-to-relay mode.
> Открытый UDP-порт **не нужен** — всё проходит через ██-инфраструктуру.

#### Dual mode — оба режима одновременно

```env
# .env
IMAGE_TAG=latest
VK_CALL_LINK=AbCdEf123456
VPN_TOKEN=your-secret-token
ALSO_DIRECT=1
LISTEN_PORT=9000
```

```bash
docker compose up -d
```

> С `ALSO_DIRECT=1` сервер принимает и direct-подключения на `:9000/udp`, и relay-клиентов через ██-звонок.

> Docker Compose автоматически запускает **captcha-сервис** для решения ██ капчи.
> Подробнее: [deploy/docker/README.md](deploy/docker/README.md).

> Подробная инструкция по деплою, мониторингу и устранению проблем: **[deploy/docker/README.md](deploy/docker/README.md)**

### Desktop-клиент

#### Direct mode — через сервер с открытым портом

```bash
./client \
  --link=<██-call-link-id> \
  --server=<your-vps-ip>:9000 \
  --token=your-secret-token
```

#### Relay-to-relay mode — через ██-звонок (без сервера с открытым портом)

```bash
./client \
  --link=<██-call-link-id> \
  --token=your-secret-token
```

> Без `--server` клиент автоматически входит в relay-to-relay mode и ждёт сервер в том же ██-звонке.

После запуска настройте прокси в системе или браузере:
- **SOCKS5** — `127.0.0.1:1080`
- **HTTP/HTTPS** — `127.0.0.1:8080`

---

## Сборка

### Требования

- Go 1.25.7+
- Docker (для сервера)
- Android SDK + gomobile (для Android)
- Xcode 15+ (для iOS)

### Сервер

```bash
go build -o server ./cmd/server
./server --listen=0.0.0.0:9000
```

Или через Docker:

```bash
cd deploy/docker
docker compose -f docker-compose.build.yml up --build
```

### Desktop-клиент

```bash
go build -o client ./cmd/client
```

### Мобильные приложения

```bash
# Android
gomobile bind -target=android -androidapi=24 -o mobile/android/app/libs/bind.aar ./mobile/bind
cd mobile/android && ./gradlew assembleRelease

# iOS
gomobile bind -target=ios -o mobile/ios/Bind.xcframework ./mobile/bind
# Далее открыть mobile/ios/ в Xcode и собрать
```

---

## Флаги

### Сервер

| Флаг | По умолчанию | Описание |
|:-----|:-------------|:---------|
| `--listen` | `0.0.0.0:9000` | UDP-адрес DTLS listener (direct mode) |
| `--link` | *(пусто)* | ID ссылки ██-звонка (relay-to-relay mode) |
| `--direct` | `false` | Также слушать на `--listen` в relay mode (env: `ALSO_DIRECT=1`) |
| `--token` | *(пусто)* | Токен аутентификации клиентов (env: `VPN_TOKEN`) |
| `--n` | `4` | Количество TURN-соединений (relay mode) |
| `--tcp` | `true` | TCP для TURN (relay mode) |
| `--proxy` | *(пусто)* | Upstream-прокси для клиентского трафика (env: `PROXY_URL`). Формат: `socks5://host:port` или `http://user:pass@host:port` |

Env: `VK_CALL_LINK` — ссылка ██-звонка (relay mode), `VPN_TOKEN` — токен, `ALSO_DIRECT=1` — dual mode, `PROXY_URL` — upstream-прокси, `SIREN_SLACK_WEBHOOK` — Slack-алерты, `CALLVPN_SCRIPTS_URL` / `CALLVPN_SCRIPTS_PUBKEY` / `CALLVPN_SCRIPTS_DIR` — override источника hot-update скриптов.

**Hot-update флаги (применимы ко всем бинарям, включая `scripts-updater`):**

| Флаг | По умолчанию | Описание |
|:-----|:-------------|:---------|
| `--scripts-url` | *(ldflags / env)* | Базовый URL для `manifest.json` |
| `--scripts-pubkey` | *(ldflags / env)* | Ed25519 public key (base64) для проверки подписи |
| `--scripts-dir` | `./var/scripts` | Локальный каталог кэша |
| `--scripts-interval` | `1h` | Период проверки manifest (updater) |

### Клиент

| Флаг | По умолчанию | Описание |
|:-----|:-------------|:---------|
| `--link` | *(обязательный)* | ID ссылки ██-звонка |
| `--server` | *(пусто)* | Адрес сервера (host:port). Пустой = relay-to-relay mode |
| `--token` | *(пусто)* | Токен аутентификации |
| `--n` | `4` | Количество параллельных TURN+DTLS соединений |
| `--tcp` | `true` | TCP вместо UDP для ████ █████ |
| `--socks5-port` | `1080` | Порт SOCKS5 прокси |
| `--http-port` | `8080` | Порт HTTP/HTTPS прокси |
| `--bind` | `127.0.0.1` | Bind-адрес для прокси |

---

## Структура проекта

```
call-vpn/
├── cmd/
│   ├── server/main.go          # VPN-сервер: DTLS listener, сессии, проксирование
│   ├── client/main.go          # Desktop-клиент: TURN + DTLS + прокси
│   ├── server-ui/              # GUI-обёртка сервера
│   ├── captcha-service/        # HTTP API для решения ██ капчи
│   └── scripts-updater/        # Sidecar: качает подписанные hot-update скрипты в shared volume
├── hot-scripts/                # Hot-update контент (публикуется через GitHub raw)
│   ├── vk-config.json          #   VK API версии, UA, WS params, captcha-константы
│   ├── stealth.js              #   Anti-detection JS для headless Chrome
│   └── manifest.json           #   Ed25519-подписанный индекс (автогенерация в CI)
├── tools/
│   └── scripts-sign/           # CLI: keygen / sign / verify / bundle
├── internal/
│   ├── scripts/                # Manager: fetch + verify + atomic swap + rollback
│   │   └── bundled/            #   //go:embed fallback на случай отсутствия сети
│   ├── scriptshook/            # Регистрация Manager в captcha + vk пакетах
│   ├── dtls/                   # DTLS шифрование
│   │   ├── server.go           #   Listener (pion/dtls)
│   │   ├── client.go           #   DialOverTURN + AsyncPacketPipe
│   │   └── relay.go            #   AcceptOverTURN + PunchRelay (relay-to-relay)
│   ├── signal/                 # ██ WebSocket signaling
│   │   └── signal.go           #   Обмен relay-адресами (AES-256-GCM)
│   ├── mux/                    # Мультиплексор потоков
│   │   ├── protocol.go         #   13-байтовый фрейм, типы сообщений
│   │   ├── mux.go              #   AddConn, OpenStream, AcceptStream
│   │   └── session.go          #   16-byte UUID, WriteSessionID/ReadSessionID
│   ├── turn/                   # ████ █████
│   │   ├── manager.go          #   Пул allocations
│   │   └── credentials.go      #   ██ ████ ███ + FetchJoinResponse
│   ├── proxy/
│   │   ├── socks5/socks5.go    #   SOCKS5 прокси (RFC 1928)
│   │   └── http/http.go        #   HTTP/HTTPS CONNECT прокси
│   └── monitoring/
│       └── siren.go            #   Slack webhook алерты
├── mobile/
│   ├── bind/tunnel.go          # gomobile API: Tunnel (Start/Stop/Dial)
│   ├── android/                # Android-приложение (Kotlin/Gradle)
│   └── ios/                    # iOS-приложение (Swift/Xcode)
├── deploy/
│   └── docker/
│       ├── Dockerfile          # Multi-stage: Alpine → Distroless
│       ├── docker-compose.yml  # Production (ghcr.io image)
│       ├── docker-compose.build.yml  # Dev (сборка из исходников)
│       ├── .env.example        # Шаблон конфигурации
│       └── README.md           # Инструкция по деплою
└── .github/workflows/
    ├── release.yml             # Desktop + Android + Docker + GitHub Release при тэгах
    └── scripts-publish.yml     # Автоподпись hot-scripts/manifest.json при push в hot-scripts/**
```

---

## Протокол

### Мультиплексор — формат фрейма

13-байтовый заголовок + payload (до 65 535 байт):

```
┌──────────┬──────────┬──────────┬──────────┬─────────────┐
│ StreamID │   Type   │ Sequence │  Length  │   Payload   │
│  4 bytes │  1 byte  │  4 bytes │  4 bytes │  0..65535   │
│  uint32  │  uint8   │  uint32  │  uint32  │   bytes     │
└──────────┴──────────┴──────────┴──────────┴─────────────┘
```

**Типы фреймов:**

| Код | Тип | Описание |
|:----|:----|:---------|
| `0x01` | Data | Пользовательские данные |
| `0x02` | Open | Открытие нового потока |
| `0x03` | Close | Закрытие потока |
| `0x04` | Ping | Keepalive запрос |
| `0x05` | Pong | Keepalive ответ |

### Установка сессии

1. Клиент устанавливает DTLS handshake
2. Отправляет **16-byte UUID** (`WriteSessionID`)
3. Сервер читает UUID (`ReadSessionID`) и группирует соединения
4. Новые DTLS-соединения с тем же UUID добавляются через `AddConn()`

---

## Зависимости

| Пакет | Версия | Назначение |
|:------|:-------|:-----------|
| [pion/dtls](https://github.com/pion/dtls) | v3.1.2 | DTLS 1.2 шифрование |
| [pion/turn](https://github.com/pion/turn) | v4.1.4 | TURN RFC 5766 |
| [gorilla/websocket](https://github.com/gorilla/websocket) | v1.5.3 | ██ WebSocket signaling |
| [cbeuw/connutil](https://github.com/cbeuw/connutil) | v1.0.1 | AsyncPacketPipe — мост datagram ↔ DTLS |
| [google/uuid](https://github.com/google/uuid) | v1.6.0 | Генерация session UUID |
| [pion/logging](https://github.com/pion/logging) | v0.2.4 | Логирование |

---

## CI/CD

| Workflow | Триггер | Результат |
|:---------|:--------|:----------|
| `release` | tag `v*` | Desktop бинари + Android APK + Docker images (server, captcha, scripts-updater) + GitHub Release |
| `scripts-publish` | push в `hot-scripts/**` | Переподпись `manifest.json` (Ed25519) и commit с `[skip ci]` |

---

## Безопасность

- **Шифрование:** DTLS 1.2 (TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
- **Signaling:** AES-256-GCM шифрование обмена адресами (при наличии `--token`)
- **Сертификаты:** самоподписанные, генерируются при каждом запуске
- **Контейнер:** distroless runtime, непривилегированный пользователь `nonroot`
- **Маскировка:** трафик неотличим от ██████████ для ███
- **Hot-update:** Ed25519-подпись manifest + SHA-256 каждого файла, публичный ключ вшит через ldflags; неверная подпись игнорируется, 3 ошибки за 5 мин → автоматический rollback + quarantine

---

## Деплой сервера

Подробная инструкция с примерами конфигурации, устранением проблем и настройкой мониторинга:

**[deploy/docker/README.md](deploy/docker/README.md)**
