# CallVPN for iOS

███-клиент, маскирующий трафик под ██████████. iOS-версия использует Network Extension (Packet Tunnel Provider) и Go-биндинги через gomobile.

## Требования

- macOS с Xcode 15+
- Go 1.21+
- [gomobile](https://pkg.go.dev/golang.org/x/mobile/cmd/gomobile)
- [xcodegen](https://github.com/yonaskolb/XcodeGen)
- Apple Developer Account (для Network Extension capability)

## Установка инструментов

### Go + gomobile

```bash
# Установить Go (если нет): https://go.dev/dl/

# Установить gomobile
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest

# Инициализировать gomobile
gomobile init
```

### xcodegen

```bash
brew install xcodegen
```

## Сборка

### 1. Собрать Go-биндинги (Bind.xcframework)

Из корня репозитория:

```bash
cd mobile/ios
make framework
```

Или вручную:

```bash
gomobile bind -target=ios -o mobile/ios/Bind.xcframework ./mobile/bind
```

Это создаст `Bind.xcframework` в директории `mobile/ios/`. Фреймворк содержит скомпилированный Go-код туннеля для arm64 (устройство) и arm64-simulator.

### 2. Сгенерировать Xcode-проект

```bash
cd mobile/ios
make project
```

Или вручную:

```bash
cd mobile/ios
xcodegen generate
```

Это создаст `CallVPN.xcodeproj` из `project.yml`.

### 3. Настроить подпись в Xcode

```bash
open CallVPN.xcodeproj
```

В Xcode:

1. Выбери target **CallVPN** в навигаторе проекта
2. Перейди на вкладку **Signing & Capabilities**
3. Выбери свою **Team** (Apple Developer Account)
4. Xcode автоматически создаст provisioning profile
5. Повтори для target **PacketTunnel**

**Важно:** Оба target должны использовать одну и ту же Team. Network Extension требует платного Apple Developer Account ($99/год). Free account НЕ поддерживает Network Extension.

### 4. Настроить App Group

App Group `group.com.callvpn.app` используется для передачи данных между основным приложением и Network Extension (логи, состояние соединения).

Если Xcode не настроил его автоматически:

1. Target **CallVPN** → Signing & Capabilities → + Capability → **App Groups**
2. Добавь `group.com.callvpn.app`
3. Повтори для target **PacketTunnel**

### 5. Собрать и установить

- Подключи iPhone по кабелю (или используй WiFi debugging)
- Выбери устройство в Xcode
- Нажми **Cmd+R** (Build & Run)

Первый запуск попросит разрешение на установку VPN-профиля в настройках iOS.

## Быстрая сборка (одна команда)

```bash
cd mobile/ios
make build   # framework + xcodegen
open CallVPN.xcodeproj
```

## Конфигурация приложения

### Поля в интерфейсе

| Поле | Описание | Пример |
|------|----------|--------|
| **Ссылка VK звонка** | Полная ссылка или ID из VK call-link | `https://vk.com/call/join/ABC123` или `ABC123` |
| **Режим** | Relay-to-Relay (без сервера) или Direct (свой сервер) | — |
| **Адрес сервера** | Только для Direct режима: host:port VPN-сервера | `vpn.example.com:9000` |
| **Токен** | Токен авторизации (должен совпадать с сервером) | `my-secret-token` |
| **Подключения (1-16)** | Количество параллельных TURN+DTLS соединений | `4` |

### Режимы работы

**Relay-to-Relay** (рекомендуется):
- Не нужен собственный сервер
- Оба участника (клиент и сервер) подключаются к одному VK-звонку
- Трафик идет через TURN-серверы VK
- Введи только ссылку VK-звонка и токен

**Direct**:
- Нужен VPN-сервер с публичным IP
- Клиент подключается к серверу напрямую через TURN
- Введи ссылку VK-звонка, адрес сервера и токен

### Где взять токен

Токен задается при запуске VPN-сервера:

```bash
# На сервере:
./server --listen=0.0.0.0:9000 --token=my-secret-token
# или через env:
VPN_TOKEN=my-secret-token ./server --listen=0.0.0.0:9000
```

В приложении введи тот же токен. Если сервер запущен без `--token`, поле оставь пустым.

### Где взять ссылку VK-звонка

1. Открой [VK Звонки](https://vk.com/call) в браузере
2. Создай звонок
3. Скопируй ссылку приглашения (формат: `https://vk.com/call/join/XXXXX`)
4. Вставь в поле "Ссылка VK звонка"

На сервере используй тот же call-link:
```bash
# Relay-to-relay:
VK_CALL_LINK=XXXXX VPN_TOKEN=my-secret-token ./server
```

## Архитектура iOS-приложения

```
CallVPN.app (основное приложение)
├── CallVPNApp.swift      — SwiftUI интерфейс, настройки, управление VPN
├── Info.plist             — конфигурация приложения
├── CallVPN.entitlements   — Network Extension + App Group
└── Assets.xcassets/       — иконка приложения

PacketTunnel.appex (Network Extension)
├── PacketTunnelProvider.swift  — NEPacketTunnelProvider, Go-туннель, packet I/O
├── Info.plist                  — NSExtension конфигурация
└── PacketTunnel.entitlements   — Network Extension + App Group

Bind.xcframework (gomobile Go-биндинги)
└── BindTunnel — TURN + DTLS + MUX туннель
```

### Взаимодействие компонентов

- **App → Extension**: параметры подключения передаются через `NETunnelProviderProtocol.providerConfiguration`
- **Extension → App**: логи и состояние соединения передаются через `UserDefaults` (App Group `group.com.callvpn.app`)
- **Extension → Go**: вызовы gomobile API (`BindTunnel.start()`, `writePacket()`, `readPacketData()`)
- **iOS TUN → Extension → Go → TURN → Server**: пакеты идут через `NEPacketTunnelProvider.packetFlow`

## Устранение проблем

### "Network Extension not found"
- Убедись что оба target подписаны одной Team
- Проверь что `com.callvpn.app.PacketTunnel` — bundle ID расширения

### "App Group mismatch"
- Оба target должны иметь `group.com.callvpn.app` в App Groups capability
- Проверь entitlements файлы

### Нет логов в приложении
- Логи передаются через UserDefaults (App Group)
- Убедись что App Group настроен корректно в обоих targets
- Подожди ~1 секунду после подключения (polling каждые 500мс)

### "VPN permission denied"
- iOS покажет системный диалог при первом подключении
- Зайди в Настройки → VPN → разреши CallVPN

### Сборка gomobile падает
```bash
# Проверь версию Go
go version  # нужна 1.21+

# Переинициализируй gomobile
gomobile init

# Проверь что Android SDK не мешает iOS сборке
gomobile bind -target=ios -v -o Bind.xcframework ../../mobile/bind
```

### Xcodegen ошибки
```bash
# Убедись что Bind.xcframework уже собран ПЕРЕД xcodegen
ls Bind.xcframework  # должен существовать

# Пересоздай проект
rm -rf CallVPN.xcodeproj
xcodegen generate
```

## CI/CD

Для автоматической сборки в CI (GitHub Actions):

```yaml
- name: Build iOS framework
  run: |
    go install golang.org/x/mobile/cmd/gomobile@latest
    gomobile init
    cd mobile/ios
    gomobile bind -target=ios -o Bind.xcframework ../../mobile/bind

- name: Build iOS app
  run: |
    cd mobile/ios
    brew install xcodegen
    xcodegen generate
    xcodebuild -project CallVPN.xcodeproj \
      -scheme CallVPN \
      -sdk iphoneos \
      -configuration Release \
      CODE_SIGN_IDENTITY="" \
      CODE_SIGNING_REQUIRED=NO
```
