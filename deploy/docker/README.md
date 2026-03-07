# Call-VPN Server — Docker Deployment

> ███-сервер, маскирующий трафик под ██████████ через ████ █████ серверы.
> Одна команда — и сервер готов к работе.

---

## Оглавление

- [Быстрый старт](#-быстрый-старт)
- [Как это работает](#-как-это-работает)
- [Режимы работы](#-режимы-работы)
- [Конфигурация](#-конфигурация)
- [Примеры docker-compose.yml](#-примеры-docker-composeyml)
- [Подключение клиента](#-подключение-клиента)
- [Мониторинг](#-мониторинг)
- [Управление сервером](#-управление-сервером)
- [Обновление](#-обновление)
- [Сборка из исходников](#-сборка-из-исходников)
- [Устранение проблем](#-устранение-проблем)

---

## Быстрый старт

### Требования

- Docker Engine 20.10+
- Docker Compose v2
- Открытый UDP-порт (по умолчанию `9000`)

### Установка за 3 шага

**1. Создайте директорию на сервере**

```bash
mkdir call-vpn && cd call-vpn
```

**2. Создайте `docker-compose.yml`**

```yaml
services:
  server:
    image: ghcr.io/fokir/vk-call-proxy:${IMAGE_TAG:-latest}
    ports:
      - "${LISTEN_PORT:-9000}:9000/udp"
    environment:
      - VPN_TOKEN=${VPN_TOKEN:-}
      - VK_CALL_LINK=${VK_CALL_LINK:-}
      - TURN_CONNS=${TURN_CONNS:-4}
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

**3. Создайте `.env` и запустите**

```env
IMAGE_TAG=latest
LISTEN_PORT=9000
VPN_TOKEN=
SIREN_SLACK_WEBHOOK=
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

```bash
docker compose up -d
```

Сервер запустится на порту `9000/udp` в direct mode и готов принимать подключения.

> Для relay-to-relay mode добавьте `VK_CALL_LINK` — см. раздел [Режимы работы](#-режимы-работы).

---

## Как это работает

```
                          ██ ████ █████
                         ┌─────────────┐
  Клиент                 │             │              Сервер (ваш VPS)
 ┌──────────┐    TURN    │  ┌───────┐  │    UDP      ┌──────────────┐        ┌──────────┐
 │ Браузер  ├───────────►│  │ Relay ├──┼────────────►│  :9000/udp   ├───────►│ Интернет │
 │ / Прокси │  (TCP/UDP) │  └───────┘  │   DTLS      │  DTLS + MUX  │  TCP   │          │
 └──────────┘            │             │              └──────────────┘        └──────────┘
                         └─────────────┘
```

**Принцип работы:**

1. Клиент получает TURN-credentials от ██ API
2. Создаёт N параллельных ████ █████-соединений
3. Поверх каждого устанавливает DTLS 1.2 шифрование
4. Мультиплексор объединяет все соединения в единый туннель
5. Сервер принимает DTLS-подключения, группирует по session ID, проксирует трафик в интернет

**Для внешнего наблюдателя трафик выглядит как обычный ██████████.**

---

## Режимы работы

Сервер поддерживает два режима. Режим определяется автоматически по наличию переменной `VK_CALL_LINK`.

### Direct mode (по умолчанию)

Сервер слушает на UDP-порту, клиент подключается через ████ █████.

**Требования:** открытый UDP-порт на сервере.

```env
# .env
IMAGE_TAG=latest
LISTEN_PORT=9000
VPN_TOKEN=your-secret-token
```

```
Клиент → ████ █████ (██) → Сервер:9000/UDP → Интернет
```

### Relay-to-relay mode

Клиент и сервер join'ят один ██████████. Обмен адресами идёт через ██ WebSocket signaling (зашифрован AES-256-GCM при наличии `VPN_TOKEN`). Трафик проходит через два ██ ████ █████.

**Требования:** VK call link. Открытый порт **не нужен**.

```env
# .env
IMAGE_TAG=latest
VK_CALL_LINK=AbCdEf123456
VPN_TOKEN=your-secret-token
TURN_CONNS=4
```

```
Клиент → TURN(клиент) ↔ TURN(сервер) → Интернет
              VK signaling (WebSocket)
```

**Как получить VK call link:**

1. Откройте VK → Мессенджер → любой диалог → кнопка «Звонок» → «Ссылка на звонок»
2. Скопируйте ID из ссылки: `https://vk.com/call/join/AbCdEf123456` → `AbCdEf123456`
3. Передайте одну и ту же ссылку серверу (`VK_CALL_LINK`) и клиенту (`--link`)

**Порядок запуска:**

1. Запустите сервер — он подключится к VK-звонку и будет ждать клиента
2. Запустите клиент с той же ссылкой — он подключится к звонку, обменяется адресами и установит туннель

> **Важно:** `VPN_TOKEN` должен совпадать на клиенте и сервере. Он используется как для аутентификации, так и для шифрования signaling-обмена.

---

## Конфигурация

Все параметры задаются через файл `.env` рядом с `docker-compose.yml`.

### Параметры

| Переменная | По умолчанию | Описание |
|:-----------|:-------------|:---------|
| `IMAGE_TAG` | `latest` | Версия Docker-образа. `latest` — последняя сборка, `1.0.0` — конкретная версия |
| `LISTEN_PORT` | `9000` | UDP-порт, открытый на хост-машине (только direct mode) |
| `VPN_TOKEN` | *(пусто)* | Токен аутентификации клиентов (рекомендуется) |
| `VK_CALL_LINK` | *(пусто)* | ID ссылки VK-звонка. Если задан — включается relay-to-relay mode |
| `TURN_CONNS` | `4` | Количество TURN-соединений (только relay mode) |
| `SIREN_SLACK_WEBHOOK` | *(пусто)* | URL Slack webhook для алертов мониторинга |
| `MEMORY_LIMIT` | `256M` | Лимит оперативной памяти контейнера |
| `CPU_LIMIT` | `1.0` | Лимит CPU (количество ядер) |

### Минимальный .env (direct mode)

```env
IMAGE_TAG=latest
LISTEN_PORT=9000
```

### Минимальный .env (relay-to-relay mode)

```env
IMAGE_TAG=latest
VK_CALL_LINK=AbCdEf123456
VPN_TOKEN=your-secret-token
```

### Полный .env

```env
# Версия образа
IMAGE_TAG=latest

# Порт (UDP) — только для direct mode, должен быть открыт в firewall
LISTEN_PORT=9000

# Токен аутентификации (рекомендуется; в relay mode также шифрует signaling)
VPN_TOKEN=your-secret-token

# VK call link — если задан, включается relay-to-relay mode
VK_CALL_LINK=

# Количество TURN-соединений (relay mode)
TURN_CONNS=4

# Slack алерты (опционально)
SIREN_SLACK_WEBHOOK=<your-slack-webhook-url>

# Ресурсы
MEMORY_LIMIT=512M
CPU_LIMIT=2.0
```

---

## Примеры docker-compose.yml

### Базовый — минимальная конфигурация

Самый простой вариант, готовый к работе без изменений:

```yaml
services:
  server:
    image: ghcr.io/fokir/vk-call-proxy:latest
    ports:
      - "9000:9000/udp"
    restart: unless-stopped
```

### Стандартный — с .env файлом (direct mode)

Рекомендуемый вариант с вынесением настроек в `.env`:

```yaml
# docker-compose.yml
services:
  server:
    image: ghcr.io/fokir/vk-call-proxy:${IMAGE_TAG:-latest}
    ports:
      - "${LISTEN_PORT:-9000}:9000/udp"
    environment:
      - VPN_TOKEN=${VPN_TOKEN:-}
      - VK_CALL_LINK=${VK_CALL_LINK:-}
      - TURN_CONNS=${TURN_CONNS:-4}
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

```env
# .env
IMAGE_TAG=latest
LISTEN_PORT=9000
VPN_TOKEN=your-secret-token
SIREN_SLACK_WEBHOOK=
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

### Relay-to-relay mode

Сервер подключается через VK-звонок. Открытый UDP-порт не нужен:

```env
# .env
IMAGE_TAG=latest
VK_CALL_LINK=AbCdEf123456
VPN_TOKEN=your-secret-token
TURN_CONNS=4
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

```bash
docker compose up -d
```

> `ports` в docker-compose.yml можно убрать — в relay mode сервер не слушает UDP.

### Нестандартный порт

Если порт `9000` занят или заблокирован — используйте другой:

```env
LISTEN_PORT=51820
```

> **Важно:** клиенту при подключении нужно указать тот же порт:
> `--server=your-vps-ip:51820`

### С мониторингом в Slack

```env
IMAGE_TAG=latest
LISTEN_PORT=9000
SIREN_SLACK_WEBHOOK=<your-slack-webhook-url>
```

Алерты отправляются при:
- Ошибках аутентификации TURN
- Потере пакетов
- Отключении клиентов
- Деградации туннеля

### Высоконагруженный сервер

Для сервера с большим количеством клиентов:

```env
IMAGE_TAG=latest
LISTEN_PORT=9000
MEMORY_LIMIT=1G
CPU_LIMIT=4.0
```

### Фиксированная версия (production)

Для стабильности лучше зафиксировать версию вместо `latest`:

```env
IMAGE_TAG=1.0.0
```

Доступные теги:
- `latest` — последняя сборка из ветки main (может быть нестабильной)
- `1.0.0`, `1.0` — конкретная версия (рекомендуется для production)
- `sha-abc1234` — привязка к конкретному коммиту

---

## Подключение клиента

После запуска сервера, клиенты подключаются так:

### Desktop — direct mode

```bash
./client \
  --link=<vk-call-link-id> \
  --server=<your-vps-ip>:9000 \
  --token=your-secret-token
```

### Desktop — relay-to-relay mode

```bash
./client \
  --link=<vk-call-link-id> \
  --token=your-secret-token
```

> Без `--server` клиент автоматически входит в relay-to-relay mode.
> Используйте тот же `--link`, что и в `VK_CALL_LINK` на сервере.

| Флаг | Описание |
|:-----|:---------|
| `--link` | ID ссылки VK-звонка (обязательный) |
| `--server` | Адрес VPS с портом. Пустой = relay-to-relay mode |
| `--token` | Токен аутентификации (должен совпадать с сервером) |
| `--n` | Количество параллельных TURN-соединений (по умолчанию 4) |
| `--tcp` | Использовать TCP для TURN (по умолчанию true) |
| `--socks5-port` | Локальный порт SOCKS5 прокси (по умолчанию 1080) |
| `--http-port` | Локальный порт HTTP прокси (по умолчанию 8080) |

После запуска клиента настройте браузер/систему на прокси:
- **SOCKS5:** `127.0.0.1:1080`
- **HTTP/HTTPS:** `127.0.0.1:8080`

### Mobile (Android / iOS)

Мобильные приложения используют gomobile API с теми же параметрами. Если `ServerAddr` пустой в `TunnelConfig` — используется relay-to-relay mode.

---

## Мониторинг

### Логи

```bash
# Последние 100 строк
docker compose logs --tail=100

# Следить в реальном времени
docker compose logs -f

# Только ошибки
docker compose logs | grep -i error
```

### Статус

```bash
# Состояние контейнера
docker compose ps

# Потребление ресурсов
docker compose stats
```

### Slack-алерты

Для настройки:

1. Создайте [Incoming Webhook](https://api.slack.com/messaging/webhooks) в Slack
2. Укажите URL в `.env`:
   ```env
   SIREN_SLACK_WEBHOOK=https://hooks.slack.com/services/...
   ```
3. Перезапустите сервер: `docker compose up -d`

---

## Управление сервером

```bash
# Запуск
docker compose up -d

# Остановка
docker compose down

# Перезапуск
docker compose restart

# Просмотр логов
docker compose logs -f
```

---

## Обновление

```bash
# Скачать новый образ и перезапустить
docker compose pull
docker compose up -d
```

Если используете фиксированную версию — обновите `IMAGE_TAG` в `.env`:

```env
IMAGE_TAG=1.1.0
```

Затем:

```bash
docker compose up -d
```

---

## Сборка из исходников

Если нужно собрать образ из исходного кода (разработка/тестирование):

```bash
git clone https://github.com/Fokir/vk-call-proxy.git
cd vk-call-proxy/deploy/docker
docker compose -f docker-compose.build.yml up --build
```

---

## Устранение проблем

### Сервер не запускается

```bash
# Проверьте логи
docker compose logs

# Убедитесь что порт свободен
ss -ulnp | grep 9000
```

### Клиент не подключается

1. **Проверьте firewall** — порт `9000/udp` должен быть открыт:
   ```bash
   # Ubuntu/Debian (ufw)
   sudo ufw allow 9000/udp

   # CentOS/RHEL (firewalld)
   sudo firewall-cmd --permanent --add-port=9000/udp
   sudo firewall-cmd --reload

   # Или через iptables
   sudo iptables -A INPUT -p udp --dport 9000 -j ACCEPT
   ```

2. **Проверьте доступность** с клиентской машины:
   ```bash
   nc -vzu <your-vps-ip> 9000
   ```

3. **Убедитесь** что клиент указывает правильный `--server=<ip>:<port>`

### Контейнер перезапускается (OOM)

Увеличьте лимит памяти в `.env`:

```env
MEMORY_LIMIT=512M
```

---

## Требования к серверу

| Параметр | Минимум | Рекомендуется |
|:---------|:--------|:-------------|
| RAM | 128 MB | 256 MB |
| CPU | 1 vCPU | 1+ vCPU |
| Сеть | 10 Mbit/s | 100 Mbit/s |
| ОС | Любая с Docker | Ubuntu 22.04+ |
| Порт | 1 UDP | 1 UDP |

---

## Безопасность

- Образ работает от **непривилегированного пользователя** (`nonroot`)
- Используется **distroless** runtime (минимальная поверхность атаки)
- Трафик шифруется **DTLS 1.2** (AES-128-GCM)
- Самоподписанные сертификаты генерируются автоматически при запуске
