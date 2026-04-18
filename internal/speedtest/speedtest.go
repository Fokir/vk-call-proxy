package speedtest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/mux"
)

// HandleServer processes a speed test stream on the server side.
func HandleServer(stream *mux.Stream, logger *slog.Logger) {
	logger.Info("speed test started", "stream_id", stream.ID)
	defer logger.Info("speed test finished", "stream_id", stream.ID)

	payload := make([]byte, mux.MaxFramePayload)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	for {
		cmd, length, err := readHeader(stream)
		if err != nil {
			if err != io.EOF {
				logger.Debug("speed test read header", "err", err)
			}
			return
		}

		switch cmd {
		case CmdPing:
			handlePing(stream, length, logger)
		case CmdDownload:
			handleDownload(stream, payload, logger)
		case CmdUpload:
			handleUpload(stream, logger)
		default:
			logger.Warn("speed test unknown command", "cmd", cmd)
			return
		}
	}
}

func handlePing(stream *mux.Stream, length uint32, logger *slog.Logger) {
	buf := make([]byte, length)
	if _, err := io.ReadFull(stream, buf); err != nil {
		logger.Debug("ping read failed", "err", err)
		return
	}
	if err := writeHeader(stream, CmdPing, uint32(len(buf))); err != nil {
		logger.Debug("ping write header failed", "err", err)
		return
	}
	if _, err := stream.Write(buf); err != nil {
		logger.Debug("ping write failed", "err", err)
	}
}

func handleDownload(stream *mux.Stream, payload []byte, logger *slog.Logger) {
	deadline := time.Now().Add(10 * time.Second)
	var totalBytes int64

	for time.Now().Before(deadline) {
		if err := writeHeader(stream, CmdData, uint32(len(payload))); err != nil {
			logger.Debug("download write header failed", "err", err)
			return
		}
		n, err := stream.Write(payload)
		totalBytes += int64(n)
		if err != nil {
			logger.Debug("download write failed", "err", err)
			return
		}
	}

	if err := writeHeader(stream, CmdDone, 0); err != nil {
		logger.Debug("download done failed", "err", err)
	}
	logger.Info("download phase complete", "bytes", totalBytes)
}

func handleUpload(stream *mux.Stream, logger *slog.Logger) {
	buf := make([]byte, mux.MaxFramePayload)
	var totalBytes int64
	start := time.Now()

	for {
		cmd, length, err := readHeader(stream)
		if err != nil {
			logger.Debug("upload read header failed", "err", err)
			break
		}
		if cmd == CmdDone {
			break
		}
		if cmd != CmdData {
			logger.Warn("upload unexpected cmd", "cmd", cmd)
			break
		}

		remaining := int(length)
		for remaining > 0 {
			toRead := remaining
			if toRead > len(buf) {
				toRead = len(buf)
			}
			n, err := stream.Read(buf[:toRead])
			totalBytes += int64(n)
			remaining -= n
			if err != nil {
				logger.Debug("upload read failed", "err", err)
				return
			}
		}
	}

	duration := time.Since(start)
	durationMs := uint32(duration.Milliseconds())

	// Send Result: 8 bytes [totalBytes(4) + durationMs(4)]
	var resultBuf [8]byte
	binary.BigEndian.PutUint32(resultBuf[0:4], uint32(totalBytes))
	binary.BigEndian.PutUint32(resultBuf[4:8], durationMs)
	if err := writeHeader(stream, CmdResult, 8); err != nil {
		logger.Debug("upload result header failed", "err", err)
		return
	}
	if _, err := stream.Write(resultBuf[:]); err != nil {
		logger.Debug("upload result write failed", "err", err)
	}
	logger.Info("upload phase complete", "bytes", totalBytes, "duration", duration)
}

// connTest holds results for a single connection's speed test stream.
type connTest struct {
	index    int
	stream   *mux.Stream
	ping     *PingResult
	download *TransferResult
	upload   *TransferResult
	err      error
}

// RunClient runs a full speed test on the given Mux using N parallel streams
// (one per active connection) to measure aggregate throughput.
// Reports per-connection results and aggregated totals.
func RunClient(m *mux.Mux, cb Callback, logger *slog.Logger) error {
	n := m.ActiveConns()
	if n == 0 {
		return fmt.Errorf("no connections available")
	}

	// Open N parallel streams — MUX SWRR pins each to a different connection.
	streams := make([]*mux.Stream, n)
	for i := range n {
		s, err := m.OpenStream(StreamIDBase + uint32(i))
		if err != nil {
			// Close already opened streams.
			for j := 0; j < i; j++ {
				streams[j].Close()
			}
			return fmt.Errorf("open speed test stream %d: %w", i, err)
		}
		streams[i] = s
	}
	defer func() {
		for _, s := range streams {
			s.Close()
		}
	}()

	results := make([]connTest, n)
	for i := range results {
		results[i].index = i
		results[i].stream = streams[i]
	}

	// Suppress health-based reconnects during speed test: the relay is
	// intentionally saturated and pong delivery may be delayed.
	m.SetSpeedTestActive(true)
	defer m.SetSpeedTestActive(false)

	// Temporarily increase idle timeout so connections don't die
	// when congested relay delays frame delivery.
	m.SetIdleTimeout(120 * time.Second)
	defer m.SetIdleTimeout(90 * time.Second)

	// Phase 1: Ping (sequential on stream 0 — measures single RTT)
	cb.OnPhase("ping")
	pingResult, err := runPing(streams[0], 20, logger)
	if err != nil {
		return fmt.Errorf("ping phase: %w", err)
	}
	for i := range results {
		results[i].ping = pingResult
	}
	pJSON, _ := json.Marshal(Progress{Phase: "ping"})
	cb.OnProgress(string(pJSON))

	// Phase 2: Download — all N streams in parallel
	cb.OnPhase("download")
	preDownStats := m.ConnStats()
	runParallelPhase(results, func(ct *connTest) {
		ct.download, ct.err = runDownload(ct.stream, nil, logger)
	}, cb, "download", logger)
	postDownStats := m.ConnStats()

	// Phase 3: Upload — all N streams in parallel
	cb.OnPhase("upload")
	runParallelPhase(results, func(ct *connTest) {
		ct.upload, ct.err = runUpload(ct.stream, nil, logger)
	}, cb, "upload", logger)
	postUpStats := m.ConnStats()

	// Build results
	var totalDownBytes, totalUpBytes int64
	var maxDownDur, maxUpDur float64
	var connResults []ConnResult

	for i, ct := range results {
		downBytes := postDownStats[i].BytesRecv - preDownStats[i].BytesRecv
		upBytes := postUpStats[i].BytesSent - postDownStats[i].BytesSent
		latMs := float64(postUpStats[i].LatencyNs) / float64(time.Millisecond)

		dr := ct.download
		ur := ct.upload
		if dr == nil {
			dr = &TransferResult{}
		}
		if ur == nil {
			ur = &TransferResult{}
		}

		connResults = append(connResults, ConnResult{
			Index:     i,
			DownBytes: downBytes,
			UpBytes:   upBytes,
			DownMbps:  dr.Mbps,
			UpMbps:    ur.Mbps,
			LatencyMs: math.Round(latMs*100) / 100,
		})

		totalDownBytes += dr.Bytes
		totalUpBytes += ur.Bytes
		if dr.DurationS > maxDownDur {
			maxDownDur = dr.DurationS
		}
		if ur.DurationS > maxUpDur {
			maxUpDur = ur.DurationS
		}
	}

	var downMbps, upMbps float64
	if maxDownDur > 0 {
		downMbps = float64(totalDownBytes) * 8 / maxDownDur / 1_000_000
	}
	if maxUpDur > 0 {
		upMbps = float64(totalUpBytes) * 8 / maxUpDur / 1_000_000
	}

	result := &Result{
		Ping: *pingResult,
		Download: TransferResult{
			Bytes:     totalDownBytes,
			DurationS: maxDownDur,
			Mbps:      math.Round(downMbps*100) / 100,
		},
		Upload: TransferResult{
			Bytes:     totalUpBytes,
			DurationS: maxUpDur,
			Mbps:      math.Round(upMbps*100) / 100,
		},
		Connections: connResults,
	}

	cb.OnComplete(result.JSON())
	return nil
}

// runParallelPhase runs fn on all connTests concurrently, reporting aggregate
// progress to cb every second.
func runParallelPhase(cts []connTest, fn func(*connTest), cb Callback, phase string, logger *slog.Logger) {
	var wg sync.WaitGroup
	for i := range cts {
		wg.Add(1)
		go func(ct *connTest) {
			defer wg.Done()
			fn(ct)
		}(&cts[i])
	}

	// Progress reporter — aggregate throughput across all streams.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	start := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			elapsed := time.Since(start)
			elapsedS := int(elapsed.Seconds())
			// Sum current stats from MUX isn't trivial here, just report elapsed time.
			p := Progress{Phase: phase, ElapsedS: elapsedS}
			pJSON, _ := json.Marshal(p)
			cb.OnProgress(string(pJSON))
		}
	}
}

func runPing(stream *mux.Stream, count int, logger *slog.Logger) (*PingResult, error) {
	var rtts []float64

	for i := 0; i < count; i++ {
		ts := time.Now().UnixNano()
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(ts))

		if err := writeHeader(stream, CmdPing, 8); err != nil {
			return nil, err
		}
		if _, err := stream.Write(buf[:]); err != nil {
			return nil, err
		}

		cmd, length, err := readHeader(stream)
		if err != nil {
			return nil, err
		}
		if cmd != CmdPing || length != 8 {
			return nil, fmt.Errorf("unexpected ping response: cmd=%d len=%d", cmd, length)
		}
		var echoBuf [8]byte
		if _, err := io.ReadFull(stream, echoBuf[:]); err != nil {
			return nil, err
		}

		rtt := float64(time.Now().UnixNano()-ts) / float64(time.Millisecond)
		rtts = append(rtts, rtt)
	}

	if len(rtts) == 0 {
		return &PingResult{}, nil
	}

	minRTT, maxRTT := rtts[0], rtts[0]
	var sum float64
	for _, r := range rtts {
		sum += r
		if r < minRTT {
			minRTT = r
		}
		if r > maxRTT {
			maxRTT = r
		}
	}
	avg := sum / float64(len(rtts))

	var varianceSum float64
	for _, r := range rtts {
		d := r - avg
		varianceSum += d * d
	}
	jitter := math.Sqrt(varianceSum / float64(len(rtts)))

	return &PingResult{
		MinMs:    math.Round(minRTT*100) / 100,
		AvgMs:    math.Round(avg*100) / 100,
		MaxMs:    math.Round(maxRTT*100) / 100,
		JitterMs: math.Round(jitter*100) / 100,
	}, nil
}

func runDownload(stream *mux.Stream, cb Callback, logger *slog.Logger) (*TransferResult, error) {
	if err := writeHeader(stream, CmdDownload, 0); err != nil {
		return nil, err
	}

	buf := make([]byte, mux.MaxFramePayload)
	var totalBytes int64
	start := time.Now()
	lastReport := start
	var lastBytes int64

	for {
		cmd, length, err := readHeader(stream)
		if err != nil {
			return nil, err
		}
		if cmd == CmdDone {
			break
		}
		if cmd != CmdData {
			return nil, fmt.Errorf("unexpected download cmd: %d", cmd)
		}

		remaining := int(length)
		for remaining > 0 {
			toRead := remaining
			if toRead > len(buf) {
				toRead = len(buf)
			}
			n, err := stream.Read(buf[:toRead])
			totalBytes += int64(n)
			remaining -= n
			if err != nil {
				return nil, err
			}
		}

		if time.Since(lastReport) >= time.Second {
			elapsed := time.Since(start)
			elapsedS := int(elapsed.Seconds())
			currentMbps := float64(totalBytes-lastBytes) * 8 / time.Since(lastReport).Seconds() / 1_000_000
			lastBytes = totalBytes
			lastReport = time.Now()

			p := Progress{Phase: "download", ElapsedS: elapsedS, CurrentMbps: math.Round(currentMbps*100) / 100, Bytes: totalBytes}
			pJSON, _ := json.Marshal(p)
			if cb != nil {
				cb.OnProgress(string(pJSON))
			}
		}
	}

	duration := time.Since(start)
	mbps := float64(totalBytes) * 8 / duration.Seconds() / 1_000_000

	return &TransferResult{
		Bytes:     totalBytes,
		DurationS: math.Round(duration.Seconds()*100) / 100,
		Mbps:      math.Round(mbps*100) / 100,
	}, nil
}

func runUpload(stream *mux.Stream, cb Callback, logger *slog.Logger) (*TransferResult, error) {
	if err := writeHeader(stream, CmdUpload, 0); err != nil {
		return nil, err
	}

	payload := make([]byte, mux.MaxFramePayload)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	var totalBytes int64
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	lastReport := start
	var lastBytes int64

	for time.Now().Before(deadline) {
		if err := writeHeader(stream, CmdData, uint32(len(payload))); err != nil {
			break
		}
		n, err := stream.Write(payload)
		totalBytes += int64(n)
		if err != nil {
			break
		}

		if time.Since(lastReport) >= time.Second {
			elapsed := time.Since(start)
			elapsedS := int(elapsed.Seconds())
			currentMbps := float64(totalBytes-lastBytes) * 8 / time.Since(lastReport).Seconds() / 1_000_000
			lastBytes = totalBytes
			lastReport = time.Now()

			p := Progress{Phase: "upload", ElapsedS: elapsedS, CurrentMbps: math.Round(currentMbps*100) / 100, Bytes: totalBytes}
			pJSON, _ := json.Marshal(p)
			if cb != nil {
				cb.OnProgress(string(pJSON))
			}
		}
	}

	if err := writeHeader(stream, CmdDone, 0); err != nil {
		return nil, err
	}

	cmd, length, err := readHeader(stream)
	if err != nil {
		return nil, err
	}
	if cmd != CmdResult || length != 8 {
		return nil, fmt.Errorf("unexpected upload result: cmd=%d len=%d", cmd, length)
	}
	var resultBuf [8]byte
	if _, err := io.ReadFull(stream, resultBuf[:]); err != nil {
		return nil, err
	}

	duration := time.Since(start)
	mbps := float64(totalBytes) * 8 / duration.Seconds() / 1_000_000

	return &TransferResult{
		Bytes:     totalBytes,
		DurationS: math.Round(duration.Seconds()*100) / 100,
		Mbps:      math.Round(mbps*100) / 100,
	}, nil
}

