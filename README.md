# Viktum Call Proxy

Весь трафик проходит через TURN relay серверы VK, шифруется DTLS 1.2 и мультиплексируется в единый туннель. Для внешнего наблюдателя это выглядит как обычный видеозвонок.

---

## Архитектура

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                                                             │
│   Клиент                     VK Cloud                      VPN-сервер       │
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

1. Клиент получает TURN credentials через Call API
2. Создаёт **N параллельных** TURN allocations (по умолчанию 4)
3. Поверх каждого устанавливает **DTLS 1.2** шифрование (AES-128-GCM)
4. Все соединения объединяются **мультиплексором** в единый туннель
5. Клиент отправляет **16-byte session UUID** для группировки соединений на сервере
6. Сервер принимает потоки и проксирует TCP-трафик в интернет

### Поток данных

```
App → SOCKS5/HTTP → MUX → DTLS → TURN Relay (VK) → Server:9000/UDP → DTLS → MUX → Internet
```

---

## Компоненты

| Компонент | Описание | Платформы |
|:----------|:---------|:----------|
| **Сервер** | DTLS/UDP listener, группировка сессий, проксирование | Linux (Docker) |
| **Desktop-клиент** | SOCKS5 + HTTP прокси, TURN + DTLS туннель | Windows, macOS |
| **Android** | Нативное приложение с gomobile | Android 7+ |
| **iOS** | Нативное приложение с PacketTunnel | iOS 15+ |

---

## Быстрый старт

### Сервер (Docker)

Создайте на сервере директорию и два файла:

**docker-compose.yml**

```yaml
services:
  server:
    image: ghcr.io/fokir/vk-call-proxy:${IMAGE_TAG:-main}
    ports:
      - "${LISTEN_PORT:-9000}:9000/udp"
    environment:
      - SIREN_SLACK_WEBHOOK=${SIREN_SLACK_WEBHOOK:-}
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: ${MEMORY_LIMIT:-256M}
          cpus: "${CPU_LIMIT:-1.0}"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

**.env**

```env
# Версия образа: main (latest) или конкретная, например 1.0.0
IMAGE_TAG=main

# UDP-порт на хосте (должен быть открыт в firewall)
LISTEN_PORT=9000

# Slack webhook для алертов (опционально)
SIREN_SLACK_WEBHOOK=

# Лимиты ресурсов
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

Запуск:

```bash
docker compose up -d
```

> Подробная инструкция по деплою, мониторингу и устранению проблем: **[deploy/docker/README.md](deploy/docker/README.md)**

### Desktop-клиент

```bash
./client \
  --link=<vk-call-link-id> \
  --server=<your-vps-ip>:9000 \
  --n=4 \
  --tcp=true
```

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
| `--listen` | `0.0.0.0:9000` | UDP-адрес DTLS listener |

Env: `SIREN_SLACK_WEBHOOK` — URL для Slack-алертов (опционально).

### Клиент

| Флаг | По умолчанию | Описание |
|:-----|:-------------|:---------|
| `--link` | *(обязательный)* | ID ссылки VK-звонка |
| `--server` | *(обязательный)* | Адрес VPN-сервера (host:port) |
| `--n` | `4` | Количество параллельных TURN+DTLS соединений |
| `--tcp` | `true` | TCP вместо UDP для TURN relay |
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
│   │   └── client.go           #   DialOverTURN + AsyncPacketPipe
│   ├── mux/                    # Мультиплексор потоков
│   │   ├── protocol.go         #   13-байтовый фрейм, типы сообщений
│   │   ├── mux.go              #   AddConn, OpenStream, AcceptStream
│   │   └── session.go          #   16-byte UUID, WriteSessionID/ReadSessionID
│   ├── turn/                   # TURN relay
│   │   ├── manager.go          #   Пул allocations
│   │   └── credentials.go      #   VK Call API credentials
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
| [pion/dtls](https://github.com/pion/dtls) | v3.0.7 | DTLS 1.2 шифрование |
| [pion/turn](https://github.com/pion/turn) | v4.1.4 | TURN RFC 5766 |
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
- **Сертификаты:** самоподписанные, генерируются при каждом запуске
- **Контейнер:** distroless runtime, непривилегированный пользователь `nonroot`
- **Маскировка:** трафик неотличим от VK-звонка для DPI

---

## Деплой сервера

Подробная инструкция с примерами конфигурации, устранением проблем и настройкой мониторинга:

**[deploy/docker/README.md](deploy/docker/README.md)**
