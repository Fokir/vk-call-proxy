# Call-VPN Server — Docker Deployment

> VPN-сервер, маскирующий трафик под VK-звонки через TURN relay серверы.
> Одна команда — и сервер готов к работе.

---

## Оглавление

- [Быстрый старт](#-быстрый-старт)
- [Как это работает](#-как-это-работает)
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

**3. Создайте `.env` и запустите**

```env
IMAGE_TAG=main
LISTEN_PORT=9000
SIREN_SLACK_WEBHOOK=
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

```bash
docker compose up -d
```

Сервер запустится на порту `9000/udp` и готов принимать подключения.

---

## Как это работает

```
                          VK TURN Relay
                         ┌─────────────┐
  Клиент                 │             │              Сервер (ваш VPS)
 ┌──────────┐    TURN    │  ┌───────┐  │    UDP      ┌──────────────┐        ┌──────────┐
 │ Браузер  ├───────────►│  │ Relay ├──┼────────────►│  :9000/udp   ├───────►│ Интернет │
 │ / Прокси │  (TCP/UDP) │  └───────┘  │   DTLS      │  DTLS + MUX  │  TCP   │          │
 └──────────┘            │             │              └──────────────┘        └──────────┘
                         └─────────────┘
```

**Принцип работы:**

1. Клиент получает TURN-credentials от VK API (имитация звонка)
2. Создаёт N параллельных TURN relay-соединений
3. Поверх каждого устанавливает DTLS 1.2 шифрование
4. Мультиплексор объединяет все соединения в единый туннель
5. Сервер принимает DTLS-подключения, группирует по session ID, проксирует трафик в интернет

**Для внешнего наблюдателя трафик выглядит как обычный VK-звонок.**

---

## Конфигурация

Все параметры задаются через файл `.env` рядом с `docker-compose.yml`.

### Параметры

| Переменная | По умолчанию | Описание |
|:-----------|:-------------|:---------|
| `IMAGE_TAG` | `main` | Версия Docker-образа. `main` — последняя сборка, `1.0.0` — конкретная версия |
| `LISTEN_PORT` | `9000` | UDP-порт, открытый на хост-машине |
| `SIREN_SLACK_WEBHOOK` | *(пусто)* | URL Slack webhook для алертов мониторинга |
| `MEMORY_LIMIT` | `256M` | Лимит оперативной памяти контейнера |
| `CPU_LIMIT` | `1.0` | Лимит CPU (количество ядер) |

### Минимальный .env

```env
IMAGE_TAG=main
LISTEN_PORT=9000
```

### Полный .env

```env
# Версия образа
IMAGE_TAG=main

# Порт (UDP) — должен быть открыт в firewall
LISTEN_PORT=9000

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
    image: ghcr.io/fokir/vk-call-proxy:main
    ports:
      - "9000:9000/udp"
    restart: unless-stopped
```

### Стандартный — с .env файлом

Рекомендуемый вариант с вынесением настроек в `.env`:

```yaml
# docker-compose.yml
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

```env
# .env
IMAGE_TAG=main
LISTEN_PORT=9000
SIREN_SLACK_WEBHOOK=
MEMORY_LIMIT=256M
CPU_LIMIT=1.0
```

### Нестандартный порт

Если порт `9000` занят или заблокирован — используйте другой:

```env
LISTEN_PORT=51820
```

> **Важно:** клиенту при подключении нужно указать тот же порт:
> `--server=your-vps-ip:51820`

### С мониторингом в Slack

```env
IMAGE_TAG=main
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
IMAGE_TAG=main
LISTEN_PORT=9000
MEMORY_LIMIT=1G
CPU_LIMIT=4.0
```

### Фиксированная версия (production)

Для стабильности лучше зафиксировать версию вместо `main`:

```env
IMAGE_TAG=1.0.0
```

Доступные теги:
- `main` — последняя сборка из ветки main (может быть нестабильной)
- `1.0.0`, `1.0` — конкретная версия (рекомендуется для production)
- `sha-abc1234` — привязка к конкретному коммиту

---

## Подключение клиента

После запуска сервера, клиенты подключаются так:

### Desktop

```bash
./client \
  --link=<vk-call-link-id> \
  --server=<your-vps-ip>:9000 \
  --n=4 \
  --tcp=true
```

| Флаг | Описание |
|:-----|:---------|
| `--link` | ID ссылки VK-звонка для получения TURN credentials |
| `--server` | Адрес вашего VPS с портом (тот же, что в `LISTEN_PORT`) |
| `--n` | Количество параллельных TURN-соединений (по умолчанию 4) |
| `--tcp` | Использовать TCP для TURN (по умолчанию true) |
| `--socks5-port` | Локальный порт SOCKS5 прокси (по умолчанию 1080) |
| `--http-port` | Локальный порт HTTP прокси (по умолчанию 8080) |

После запуска клиента настройте браузер/систему на прокси:
- **SOCKS5:** `127.0.0.1:1080`
- **HTTP/HTTPS:** `127.0.0.1:8080`

### Mobile (Android / iOS)

Мобильные приложения используют gomobile API с теми же параметрами подключения.

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
