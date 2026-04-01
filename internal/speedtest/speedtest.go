package speedtest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
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

// RunClient runs a full speed test (ping + download + upload) on the given Mux.
// Measures single-connection throughput + ping/latency with per-connection breakdown.
func RunClient(m *mux.Mux, cb Callback, logger *slog.Logger) error {
	stream, err := m.OpenStream(StreamID)
	if err != nil {
		return fmt.Errorf("open speed test stream: %w", err)
	}
	defer stream.Close()

	preStats := m.ConnStats()

	// Phase 1: Ping
	cb.OnPhase("ping")
	pingResult, err := runPing(stream, 20, logger)
	if err != nil {
		return fmt.Errorf("ping phase: %w", err)
	}
	pJSON, _ := json.Marshal(Progress{Phase: "ping"})
	cb.OnProgress(string(pJSON))

	// Phase 2: Download
	cb.OnPhase("download")
	midStats := m.ConnStats()
	downloadResult, err := runDownload(stream, cb, logger)
	if err != nil {
		return fmt.Errorf("download phase: %w", err)
	}
	postDownStats := m.ConnStats()

	// Phase 3: Upload
	cb.OnPhase("upload")
	uploadResult, err := runUpload(stream, cb, logger)
	if err != nil {
		return fmt.Errorf("upload phase: %w", err)
	}
	postUpStats := m.ConnStats()

	connResults := buildConnResults(preStats, midStats, postDownStats, postUpStats,
		downloadResult.DurationS, uploadResult.DurationS)

	result := &Result{
		Ping:        *pingResult,
		Download:    *downloadResult,
		Upload:      *uploadResult,
		Connections: connResults,
	}

	cb.OnComplete(result.JSON())
	return nil
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
			cb.OnProgress(string(pJSON))
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
			cb.OnProgress(string(pJSON))
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

func buildConnResults(pre, mid, postDown, postUp []mux.ConnStat,
	downDuration, upDuration float64) []ConnResult {

	var results []ConnResult
	for i := range postUp {
		if i >= len(pre) || i >= len(mid) || i >= len(postDown) {
			break
		}
		if !postUp[i].Alive {
			continue
		}

		// Download: client receives data → use BytesRecv delta
		downBytes := postDown[i].BytesRecv - mid[i].BytesRecv
		// Upload: client sends data → use BytesSent delta
		upBytes := postUp[i].BytesSent - postDown[i].BytesSent

		var downMbps, upMbps float64
		if downDuration > 0 {
			downMbps = float64(downBytes) * 8 / downDuration / 1_000_000
		}
		if upDuration > 0 {
			upMbps = float64(upBytes) * 8 / upDuration / 1_000_000
		}
		latMs := float64(postUp[i].LatencyNs) / float64(time.Millisecond)

		results = append(results, ConnResult{
			Index:     i,
			DownBytes: downBytes,
			UpBytes:   upBytes,
			DownMbps:  math.Round(downMbps*100) / 100,
			UpMbps:    math.Round(upMbps*100) / 100,
			LatencyMs: math.Round(latMs*100) / 100,
		})
	}
	return results
}
