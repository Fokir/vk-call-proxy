package telemost

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/google/uuid"
)

// ReconnectConfig настраивает TelemostReconnectManager.
//
// Аналог internal/client.ReconnectConfig, но для Telemost SFU: вместо TURN-allocate +
// DTLS-handshake используется ConnectPaired (WebRTC publisher/subscriber через Goloom).
type ReconnectConfig struct {
	Service     *Service      // для ConnectPaired
	TargetConns int           // сколько конекшенов держим одновременно
	AuthToken   string        // для WriteAuthToken (пусто = не пишем)
	SessionID   uuid.UUID     // MUX session UUID (общий для всех conn'ов)
	ServerNames []string      // len >= TargetConns
	ClientNames []string      // len >= TargetConns
	Logger      *slog.Logger
}

// ReconnectManager следит за MUX-слоем и пересоздаёт упавшие Telemost conn'ы.
//
// Архитектура:
//  1. Изначально N conn'ов (indices 0..N-1) подаются через WrapInitial() — это
//     устанавливает indexed wrapper, уведомляющий manager о смерти.
//  2. Run() блокирует и обрабатывает wakeup'ы из onDie + периодический тикер.
//  3. Упавший index освобождается → reconnectOne() восстанавливает именно этот
//     index (критично для obfuscation key: DeriveIndexedObfuscationKey(token, idx)).
type ReconnectManager struct {
	cfg ReconnectConfig
	m   *mux.Mux

	// activeIdx[i] = true, если index i сейчас занят живым conn'ом.
	// Используется и при первичной установке (WrapInitial), и при reconnect.
	activeIdx []atomic.Bool

	wakeup chan struct{}
}

// NewReconnectManager создаёт manager. После этого нужно обернуть первичные
// conn'ы через WrapInitial и в конце запустить Run().
func NewReconnectManager(cfg ReconnectConfig, m *mux.Mux) *ReconnectManager {
	return &ReconnectManager{
		cfg:       cfg,
		m:         m,
		activeIdx: make([]atomic.Bool, cfg.TargetConns),
		wakeup:    make(chan struct{}, 1),
	}
}

// WrapInitial оборачивает conn'ы, созданные вне manager'а (например, при первом
// коннекте в connectTelemost), так что при падении index освобождается и
// manager знает, что нужно переподключить именно этот индекс.
//
// conns и indices должны быть одной длины; indices[i] — telemost-index для conns[i].
func (rm *ReconnectManager) WrapInitial(conns []io.ReadWriteCloser, indices []int) []io.ReadWriteCloser {
	if len(conns) != len(indices) {
		panic("telemost: WrapInitial len mismatch")
	}
	out := make([]io.ReadWriteCloser, len(conns))
	for i, c := range conns {
		idx := indices[i]
		if idx >= 0 && idx < len(rm.activeIdx) {
			rm.activeIdx[idx].Store(true)
		}
		out[i] = rm.wrap(c, idx)
	}
	return out
}

// wrap создаёт indexedConn, который при любой фатальной ошибке Read/Write/Close
// вызывает onDie один раз и триггерит wakeup manager'а.
func (rm *ReconnectManager) wrap(conn io.ReadWriteCloser, idx int) io.ReadWriteCloser {
	return &indexedConn{
		ReadWriteCloser: conn,
		idx:             idx,
		onDie: func(i int) {
			if i >= 0 && i < len(rm.activeIdx) {
				rm.activeIdx[i].Store(false)
			}
			rm.triggerWakeup()
		},
	}
}

func (rm *ReconnectManager) triggerWakeup() {
	select {
	case rm.wakeup <- struct{}{}:
	default:
	}
}

// Run — главный цикл: при получении wakeup или периодически проверяет активность
// индексов и восстанавливает упавшие. Блокирует до ctx.Done().
func (rm *ReconnectManager) Run(ctx context.Context) {
	logger := rm.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Drain ConnDied и конвертируем в wakeup — MUX не знает про наши индексы,
	// но сам факт смерти — сигнал проверить состояние activeIdx.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case idx, ok := <-rm.m.ConnDied():
				if !ok {
					return
				}
				rm.m.RemoveConn(idx)
				rm.triggerWakeup()
			}
		}
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	const (
		maxBackoff = 30 * time.Second
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-rm.wakeup:
		}

		// Собираем список индексов, которые освободились.
		var missing []int
		for i := 0; i < rm.cfg.TargetConns; i++ {
			if !rm.activeIdx[i].Load() {
				missing = append(missing, i)
			}
		}
		if len(missing) == 0 {
			continue
		}

		logger.Info("telemost reconnect: restoring indices", "missing", missing)

		for _, idx := range missing {
			// Ещё раз проверяем — могла succeed'нуть параллельная попытка.
			if rm.activeIdx[idx].Load() {
				continue
			}

			rm.m.BeginReconnect()
			backoff := time.Second
			for attempt := 1; ; attempt++ {
				if ctx.Err() != nil {
					rm.m.EndReconnect()
					return
				}

				err := rm.reconnectOne(ctx, idx)
				if err == nil {
					break
				}

				logger.Warn("telemost reconnect attempt failed",
					"index", idx, "attempt", attempt, "err", err,
					"next_backoff", backoff)

				select {
				case <-ctx.Done():
					rm.m.EndReconnect()
					return
				case <-time.After(backoff):
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			rm.m.EndReconnect()
		}
	}
}

// reconnectOne создаёт новое ConnectPaired-соединение под указанным index'ом и
// добавляет в MUX. Успех: activeIdx[idx] = true.
func (rm *ReconnectManager) reconnectOne(ctx context.Context, idx int) error {
	if idx >= len(rm.cfg.ServerNames) || idx >= len(rm.cfg.ClientNames) {
		return fmt.Errorf("index %d out of range", idx)
	}

	logger := rm.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, cleanup, err := rm.cfg.Service.ConnectPaired(connCtx,
		logger.With("reconnect_idx", idx),
		rm.cfg.ClientNames[idx], rm.cfg.ServerNames[idx], idx)
	cancel()
	if err != nil {
		return fmt.Errorf("ConnectPaired idx=%d: %w", idx, err)
	}

	if rm.cfg.AuthToken != "" {
		if err := mux.WriteAuthToken(conn, rm.cfg.AuthToken); err != nil {
			cleanup()
			return fmt.Errorf("write auth token: %w", err)
		}
	}

	var sid [16]byte
	copy(sid[:], rm.cfg.SessionID[:])
	if err := mux.WriteSessionID(conn, sid); err != nil {
		cleanup()
		return fmt.Errorf("write session id: %w", err)
	}

	wrapped := rm.wrap(conn, idx)
	rm.activeIdx[idx].Store(true)
	rm.m.AddConn(wrapped)

	logger.Info("telemost reconnect: added", "index", idx,
		"active", rm.m.ActiveConns(), "total", rm.m.TotalConns())
	return nil
}

// indexedConn — обёртка, уведомляющая manager о смерти conn'а с конкретным
// telemost-index'ом.
type indexedConn struct {
	io.ReadWriteCloser
	idx   int
	onDie func(int)
	once  sync.Once
}

func (ic *indexedConn) notifyDead() {
	ic.once.Do(func() { ic.onDie(ic.idx) })
}

func (ic *indexedConn) Read(p []byte) (int, error) {
	n, err := ic.ReadWriteCloser.Read(p)
	if err != nil {
		ic.notifyDead()
	}
	return n, err
}

func (ic *indexedConn) Write(p []byte) (int, error) {
	n, err := ic.ReadWriteCloser.Write(p)
	if err != nil {
		ic.notifyDead()
	}
	return n, err
}

func (ic *indexedConn) Close() error {
	ic.notifyDead()
	return ic.ReadWriteCloser.Close()
}
