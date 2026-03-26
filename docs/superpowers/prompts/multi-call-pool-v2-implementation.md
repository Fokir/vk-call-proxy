# Промт для новой сессии: Multi-Call Pool v2 Implementation

Скопируй всё ниже и вставь как первое сообщение в новой сессии Claude Code:

---

Задача: реализовать Multi-Call Pool v2 для call-vpn.

## Контекст

Прочитай память проекта через Serena: `project_multi_call_pool`, `testing/android-device`, `testing/e2e-setup`, `testing/speed-test`, `testing/server-deployment`. Это даст полный контекст.

Ветка: `feat/rate-limited-allocations` (текущая, не переключайся).

## Spec

Прочитай полный spec: `docs/superpowers/specs/2026-03-27-multi-call-pool-v2-design.md`

Там описано:
- Архитектура: новый пакет `internal/tunnel/` (pool.go, slot.go, signaling.go, config.go)
- Data flow: N звонков × M conn'ов → shared MUX
- Signaling redundancy: broadcast через все WS, dedup by nonce+callIndex
- Server pre-allocation: 1 TURN per slot при старте
- Reconnect: serial queue с backoff 3s→60s, бесконечно
- CLI: `--link` repeatable, `--n` per call
- Acceptance criteria: 8 групп включая Android E2E тестирование
- Android test matrix: от 1×1 до 4×4 комбинаций

## Порядок реализации

1. **Напиши implementation plan** (используй superpowers:writing-plans skill) на основе spec'а
2. **Реализуй** `internal/tunnel/` пакет:
   - `config.go` — типы, константы
   - `signaling.go` — SignalingRouter (broadcast + dedup)
   - `slot.go` — CallSlot (connect → allocate → monitor → reconnect)
   - `pool.go` — CallPool (manages slots + shared MUX)
3. **Интеграция**:
   - `cmd/client/main.go` — `--link` repeatable flag
   - `cmd/server/main.go` — `--link` repeatable flag
   - `internal/client/client.go` — route to pool.Start() if multi-link
   - `internal/server/server.go` — route to pool if multi-link
   - `internal/mux/mux.go` — trySendInFrame panic recovery
4. **Unit тесты**: SignalingRouter dedup, CallSlot lifecycle, reconnect queue
5. **test-e2e.sh** — адаптировать для `--links=N`
6. **PC E2E тестирование**: запусти test-e2e.sh с разными комбинациями
7. **Android тестирование**:
   - Собери APK (инструкции в памяти `testing/android-device`)
   - Установи через adb
   - Запусти server локально (НЕ Docker): `./callvpn-server.exe --link=... --n=4 --token=test123`
   - Протестируй полную матрицу из spec'а (8 комбинаций)

## Важные ограничения

- **НЕ переключай ветки** — работай в `feat/rate-limited-allocations`
- **НЕ используй Docker** для тестирования — сервер всегда локально
- **Токены и ссылки** в `.env` файле (4 call links, 2 VK tokens, VPN token)
- **Rate limiting**: VK API max 3 req/s — все VK HTTP запросы через serial queue
- **Provider-agnostic**: `tunnel/` пакет не знает о VK, принимает `[]provider.Service`
- **Backward compat**: один `--link` = текущее поведение без pool
- **Frame size**: 1200 байт — hard limit TURN relay, не менять

## Что уже реализовано в ветке

- MUX stream striping (disabled by default, ENABLE_STRIPING=1)
- Adaptive striping (auto-enable/disable по throughput)
- TCP_NODELAY на TURN connections
- Batch writes в writeLoop (до 64 фреймов)
- TURN round-robin по всем серверам из VK API
- VK rate limit detection + batched allocation с backoff
- Configurable frame size (MUX_FRAME_SIZE env var)
- test-e2e.sh — E2E тест скрипт

## Acceptance criteria

Реализация завершена ТОЛЬКО когда ВСЕ чеклисты из секции "Acceptance Criteria" в spec'е пройдены, включая Android E2E тестирование на подключенном устройстве.
