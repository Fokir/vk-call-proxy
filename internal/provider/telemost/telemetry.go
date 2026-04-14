package telemost

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// telemetryLoop имитирует XHR-телеметрию web/Android клиента Телемост:
// периодически POST'ит статистику на endpoint из serverHello.telemetryConfiguration.
//
// Зачем: SFU использует эти запросы для детекта активных участников и принятия
// решений о reshape/drop. Клиенты без телеметрии могут получить более агрессивный
// тайминг reshape → нестабильное соединение.
//
// Поведение:
//   - первый запрос event="join" сразу
//   - далее event="stats" раз в interval (по умолчанию 20с)
//   - event="leave" при Close
func telemetryLoop(
	ctx context.Context,
	cfg *telemetryConfig,
	roomID, peerID, displayName string,
	closeCh <-chan struct{},
) {
	endpoint := cfg.endpointURL()
	if endpoint == "" {
		return
	}
	interval := cfg.interval()

	client := &http.Client{Timeout: 10 * time.Second}

	send := func(event string) {
		body, err := json.Marshal(map[string]interface{}{
			"event":          event,
			"timestamp":      time.Now().UnixMilli(),
			"peerId":         peerID,
			"roomId":         roomID,
			"displayName":    displayName,
			"implementation": "web",
		})
		if err != nil {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Origin", "https://telemost.yandex.com")
		req.Header.Set("Referer", "https://telemost.yandex.com/")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Client-Instance-Id", uuid.New().String())
		req.Header.Set("X-Telemost-Client-Version", clientVersion)
		req.Header.Set("Idempotency-Key", uuid.New().String())
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}

	send("join")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			send("leave")
			return
		case <-closeCh:
			send("leave")
			return
		case <-ticker.C:
			send("stats")
		}
	}
}
