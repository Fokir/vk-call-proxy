# ██ Call Proxy

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
| **Desktop-клиент** | SOCKS5 + HTTP прокси, TURN + DTLS туннель | Windows, macOS |
| **Android** | Нативное приложение с gomobile | Android 7+ |
| **iOS** | Нативное приложение с PacketTunnel | iOS 15+ [не реализованно полноценно] |

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

Env: `VK_CALL_LINK` — ссылка ██-звонка (relay mode), `VPN_TOKEN` — токен, `ALSO_DIRECT=1` — dual mode, `SIREN_SLACK_WEBHOOK` — Slack-алерты.

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
│   └── client/main.go          # Desktop-клиент: TURN + DTLS + прокси
├── internal/
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
    ├── build-server.yml        # Docker image → GHCR
    ├── build-desktop.yml       # Бинарники: Windows, macOS
    ├── build-mobile.yml        # Android APK, iOS IPA
    └── release.yml             # GitHub Release при тэгах
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
| `build-server` | push/PR → main, tags `v*` | Docker image → `ghcr.io/fokir/vk-call-proxy` |
| `build-desktop` | push/PR → main | `client-windows-amd64.exe`, `client-darwin-*` |
| `build-mobile` | push/PR → main | `app-release.apk`, `CallVPN.ipa` |
| `release` | tag `v*` | GitHub Release с артефактами |

---

## Безопасность

- **Шифрование:** DTLS 1.2 (TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
- **Signaling:** AES-256-GCM шифрование обмена адресами (при наличии `--token`)
- **Сертификаты:** самоподписанные, генерируются при каждом запуске
- **Контейнер:** distroless runtime, непривилегированный пользователь `nonroot`
- **Маскировка:** трафик неотличим от ██████████ для ███

---

## Деплой сервера

Подробная инструкция с примерами конфигурации, устранением проблем и настройкой мониторинга:

**[deploy/docker/README.md](deploy/docker/README.md)**
