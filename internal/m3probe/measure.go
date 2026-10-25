package m3probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MeasureConfig bounds one host-side measurement run. Zero values take the
// defaults; the whole run is capped by Timeout (at most 120s).
type MeasureConfig struct {
	Nonce        []byte
	EchoCount    int
	EchoPayload  int
	StreamBytes  int
	Timeout      time.Duration
	PingInterval time.Duration
}

const (
	defaultEchoCount    = 32
	defaultEchoPayload  = 32
	defaultStreamBytes  = 256 << 10
	defaultTimeout      = 120 * time.Second
	defaultPingInterval = 10 * time.Millisecond
	pingReplyTimeout    = time.Second
)

func (c MeasureConfig) withDefaults() MeasureConfig {
	if c.EchoCount == 0 {
		c.EchoCount = defaultEchoCount
	}
	if c.EchoPayload == 0 {
		c.EchoPayload = defaultEchoPayload
	}
	if c.StreamBytes == 0 {
		c.StreamBytes = defaultStreamBytes
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	if c.PingInterval == 0 {
		c.PingInterval = defaultPingInterval
	}
	return c
}

func (c MeasureConfig) validate() error {
	if len(c.Nonce) == 0 || len(c.Nonce) > MaxNonceBytes {
		return fmt.Errorf("%w: nonce length %d", ErrBadPayload, len(c.Nonce))
	}
	if c.EchoCount < 1 || c.EchoCount > 1000 {
		return fmt.Errorf("%w: echo count %d", ErrBadPayload, c.EchoCount)
	}
	if c.EchoPayload < 1 || c.EchoPayload > MaxPayload {
		return fmt.Errorf("%w: echo payload %d", ErrBadPayload, c.EchoPayload)
	}
	if c.StreamBytes < 1 || c.StreamBytes > MaxStreamBytes {
		return fmt.Errorf("%w: stream bytes %d", ErrBadPayload, c.StreamBytes)
	}
	if c.Timeout <= 0 || c.Timeout > 120*time.Second {
		return fmt.Errorf("%w: timeout %s", ErrBadPayload, c.Timeout)
	}
	if c.PingInterval <= 0 || c.PingInterval > time.Second {
		return fmt.Errorf("%w: ping interval %s", ErrBadPayload, c.PingInterval)
	}
	return nil
}

// StreamReport is the measured result for one logical stream id.
type StreamReport struct {
	ID             uint16 `json:"id"`
	Bytes          int    `json:"bytes"`
	Chunks         int    `json:"chunks"`
	SentSHA256     string `json:"sent_sha256"`
	ReceivedSHA256 string `json:"received_sha256"`
}

// Report holds only fixed non-secret hashes, metrics and errors. Counts are
// measurements; nothing here is a performance pass threshold. When no ping
// reply arrived while both streams were active, BulkPingObserved is false and
// the bulk ping metrics stay zero.
type Report struct {
	Version            int            `json:"version"`
	NonceSHA256        string         `json:"nonce_sha256"`
	EchoSamples        int            `json:"echo_samples"`
	EchoP50US          int64          `json:"echo_p50_us"`
	EchoP95US          int64          `json:"echo_p95_us"`
	EchoMaxUS          int64          `json:"echo_max_us"`
	BulkPingObserved   bool           `json:"bulk_ping_observed"`
	BulkPingSamples    int            `json:"bulk_ping_samples"`
	BulkPingP50US      int64          `json:"bulk_ping_p50_us"`
	BulkPingP95US      int64          `json:"bulk_ping_p95_us"`
	BulkPingMaxUS      int64          `json:"bulk_ping_max_us"`
	PingsSent          int            `json:"pings_sent"`
	PingsOK            int            `json:"pings_ok"`
	PingsMissed        int            `json:"pings_missed"`
	PingsLate          int            `json:"pings_late"`
	PingsAfterStream   int            `json:"pings_after_stream"`
	StreamBytes        int            `json:"stream_bytes_each"`
	StreamMs           int64          `json:"stream_ms"`
	AggregateBytesPerS int64          `json:"aggregate_bytes_per_sec"`
	Streams            []StreamReport `json:"streams"`
	InterleaveSwitches int            `json:"interleave_switches"`
	ElapsedMS          int64          `json:"elapsed_ms"`
	Errors             []string       `json:"errors"`
}

// Measure runs the bounded host-side scenario over one clean session: Hello
// with a fresh nonce, Ready echo, baseline echo RTTs, two interleaved streams
// with sequenced pings during output, then Finish/Ack. It takes ownership of
// rw and closes it on every exit.
func Measure(rw io.ReadWriteCloser, cfg MeasureConfig) (Report, error) {
	start := time.Now()
	cfg = cfg.withDefaults()
	sum := sha256.Sum256(cfg.Nonce)
	rep := Report{Version: 1, NonceSHA256: hex.EncodeToString(sum[:])}

	var aborted atomic.Bool
	timer := time.AfterFunc(cfg.Timeout, func() {
		aborted.Store(true)
		_ = rw.Close()
	})
	defer timer.Stop()

	readStop := make(chan struct{})
	readerDone := make(chan struct{})
	readerStarted := false
	var readOnce sync.Once
	stopReader := func() {
		readOnce.Do(func() {
			close(readStop)
			_ = rw.Close()
			if readerStarted {
				select {
				case <-readerDone:
				case <-time.After(2 * time.Second):
				}
			}
		})
	}
	defer stopReader()

	fail := func(err error) (Report, error) {
		rep.Errors = append(rep.Errors, err.Error())
		rep.ElapsedMS = time.Since(start).Milliseconds()
		return rep, err
	}
	ioErr := func(err error) error {
		if aborted.Load() {
			return fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return err
	}
	if err := cfg.validate(); err != nil {
		return fail(err)
	}

	if err := WriteFrame(rw, Frame{Version: Version, Type: TypeHello, ID: ControlID, Payload: cfg.Nonce}); err != nil {
		return fail(ioErr(fmt.Errorf("hello: %w", err)))
	}
	f, err := ReadFrame(rw)
	if err != nil {
		return fail(ioErr(fmt.Errorf("handshake: %w", err)))
	}
	if f.Type != TypeReady || f.ID != ControlID || !bytes.Equal(f.Payload, cfg.Nonce) {
		return fail(fmt.Errorf("%w: handshake type %d id %d", ErrBadPayload, f.Type, f.ID))
	}

	payload := make([]byte, cfg.EchoPayload)
	lat := make([]time.Duration, 0, cfg.EchoCount)
	for i := 0; i < cfg.EchoCount; i++ {
		for j := range payload {
			payload[j] = byte(i*31 + j)
		}
		begin := time.Now()
		if err := WriteFrame(rw, Frame{Version: Version, Type: TypeEcho, ID: ControlID, Payload: payload}); err != nil {
			return fail(ioErr(fmt.Errorf("echo %d write: %w", i, err)))
		}
		rf, err := ReadFrame(rw)
		if err != nil {
			return fail(ioErr(fmt.Errorf("echo %d read: %w", i, err)))
		}
		if rf.Type != TypeEchoReply || rf.ID != ControlID || !bytes.Equal(rf.Payload, payload) {
			return fail(fmt.Errorf("%w: echo %d reply mismatch", ErrBadPayload, i))
		}
		lat = append(lat, time.Since(begin))
	}

	type incoming struct {
		got       int
		chunks    int
		hash      hash.Hash
		ended     bool
		sentTotal int
		sentSum   [32]byte
	}
	inc := map[uint16]*incoming{
		FirstStreamID: {hash: sha256.New()},
		LastStreamID:  {hash: sha256.New()},
	}
	for _, id := range []uint16{FirstStreamID, LastStreamID} {
		if err := WriteFrame(rw, Frame{Version: Version, Type: TypeStart, ID: id, Payload: encodeStartBytes(cfg.StreamBytes)}); err != nil {
			return fail(ioErr(fmt.Errorf("start %d: %w", id, err)))
		}
	}
	streamBegin := time.Now()

	frames := make(chan Frame, 16)
	readErr := make(chan error, 1)
	readerStarted = true
	go func() {
		defer close(readerDone)
		for {
			f, err := ReadFrame(rw)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-readStop:
				return
			}
		}
	}()

	ticker := time.NewTicker(cfg.PingInterval)
	defer ticker.Stop()
	pingTimer := time.NewTimer(pingReplyTimeout)
	if !pingTimer.Stop() {
		<-pingTimer.C
	}
	var pingC <-chan time.Time
	var (
		seq                uint32
		outstanding        uint32
		outstandingPayload []byte
		pingSentAt         time.Time
		bulkLat            []time.Duration
		bulkSamples        int
		pingsSent          int
		pingsOK            int
		pingsMissed        int
		pingsLate          int
		pingsAfterStream   int
		streamsDone        bool
		streamDuration     time.Duration
	)
	sendPing := func() error {
		seq++
		payload := encodePing(seq)
		outstanding = seq
		outstandingPayload = payload
		pingSentAt = time.Now()
		pingsSent++
		if err := WriteFrame(rw, Frame{Version: Version, Type: TypeEcho, ID: ControlID, Payload: payload}); err != nil {
			return err
		}
		pingTimer.Reset(pingReplyTimeout)
		pingC = pingTimer.C
		return nil
	}
	clearPing := func() {
		outstanding = 0
		outstandingPayload = nil
		if !pingTimer.Stop() {
			select {
			case <-pingTimer.C:
			default:
			}
		}
		pingC = nil
	}

	if err := sendPing(); err != nil {
		return fail(ioErr(fmt.Errorf("ping write: %w", err)))
	}

	switches := 0
	prevID := uint16(ControlID)
	ended := 0
	for ended < len(inc) || outstanding != 0 {
		select {
		case rf := <-frames:
			switch rf.Type {
			case TypeEchoReply:
				if rf.ID != ControlID {
					return fail(fmt.Errorf("%w: ping id %d", ErrBadID, rf.ID))
				}
				rfSeq, ok := decodePing(rf.Payload)
				if !ok {
					return fail(fmt.Errorf("%w: malformed ping reply", ErrBadPayload))
				}
				if outstanding != 0 && rfSeq == outstanding {
					if !bytes.Equal(rf.Payload, outstandingPayload) {
						return fail(fmt.Errorf("%w: ping %d payload mismatch", ErrBadPayload, rfSeq))
					}
					if streamsDone {
						// Drain reply after both StreamEnds: counted in the OK
						// total but not as a bulk sample.
						pingsAfterStream++
						pingsOK++
					} else {
						bulkLat = append(bulkLat, time.Since(pingSentAt))
						bulkSamples++
						pingsOK++
					}
					clearPing()
				} else {
					pingsLate++
				}
			case TypeStream:
				st := inc[rf.ID]
				if st == nil || st.ended {
					return fail(fmt.Errorf("%w: stream id %d", ErrBadID, rf.ID))
				}
				if len(rf.Payload) == 0 || st.got+len(rf.Payload) > cfg.StreamBytes {
					return fail(fmt.Errorf("%w: stream %d length %d at %d", ErrBadPayload, rf.ID, len(rf.Payload), st.got))
				}
				for i, b := range rf.Payload {
					if b != streamByte(st.got+i, streamSeed(rf.ID)) {
						return fail(fmt.Errorf("%w: stream %d byte %d", ErrBadPayload, rf.ID, st.got+i))
					}
				}
				if _, err := st.hash.Write(rf.Payload); err != nil {
					return fail(fmt.Errorf("stream %d hash: %w", rf.ID, err))
				}
				st.got += len(rf.Payload)
				st.chunks++
				if rf.ID != prevID {
					switches++
				}
				prevID = rf.ID
			case TypeStreamEnd:
				st := inc[rf.ID]
				if st == nil || st.ended {
					return fail(fmt.Errorf("%w: stream end id %d", ErrBadID, rf.ID))
				}
				total, sentSum, err := decodeStreamEnd(rf.Payload)
				if err != nil {
					return fail(err)
				}
				if total != st.got || total != cfg.StreamBytes {
					return fail(fmt.Errorf("%w: stream %d end %d got %d want %d", ErrBadPayload, rf.ID, total, st.got, cfg.StreamBytes))
				}
				var gotSum [32]byte
				copy(gotSum[:], st.hash.Sum(nil))
				if gotSum != sentSum {
					return fail(fmt.Errorf("%w: stream %d hash mismatch", ErrBadPayload, rf.ID))
				}
				st.ended = true
				st.sentTotal = total
				st.sentSum = sentSum
				ended++
				if ended == len(inc) {
					streamsDone = true
					streamDuration = time.Since(streamBegin)
				}
			default:
				return fail(fmt.Errorf("%w: host received type %d", ErrBadType, rf.Type))
			}
		case err := <-readErr:
			return fail(ioErr(fmt.Errorf("stream read: %w", err)))
		case <-pingC:
			pingsMissed++
			clearPing()
		case <-ticker.C:
			if outstanding == 0 {
				if err := sendPing(); err != nil {
					return fail(ioErr(fmt.Errorf("ping write: %w", err)))
				}
			}
		}
	}
	ticker.Stop()

	if err := WriteFrame(rw, Frame{Version: Version, Type: TypeFinish, ID: ControlID}); err != nil {
		return fail(ioErr(fmt.Errorf("finish: %w", err)))
	}
	ackOK := func(rf Frame) bool {
		return rf.Type == TypeAck && rf.ID == ControlID && len(rf.Payload) == 0
	}
	select {
	case rf := <-frames:
		if !ackOK(rf) {
			return fail(fmt.Errorf("%w: finish reply type %d", ErrBadPayload, rf.Type))
		}
	case err := <-readErr:
		// The guest may close right after Ack; an Ack already read into the
		// buffered channel still wins over that EOF.
		select {
		case rf := <-frames:
			if !ackOK(rf) {
				return fail(fmt.Errorf("%w: finish reply type %d", ErrBadPayload, rf.Type))
			}
		default:
			return fail(ioErr(fmt.Errorf("ack read: %w", err)))
		}
	}

	if switches < 2 {
		return fail(fmt.Errorf("%w: interleave switches %d < 2", ErrBadPayload, switches))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	sort.Slice(bulkLat, func(i, j int) bool { return bulkLat[i] < bulkLat[j] })
	rep.EchoSamples = len(lat)
	rep.EchoP50US = lat[pctIndex(len(lat), 0.50)].Microseconds()
	rep.EchoP95US = lat[pctIndex(len(lat), 0.95)].Microseconds()
	rep.EchoMaxUS = lat[len(lat)-1].Microseconds()
	rep.BulkPingObserved = bulkSamples > 0
	rep.BulkPingSamples = bulkSamples
	if len(bulkLat) > 0 {
		rep.BulkPingP50US = bulkLat[pctIndex(len(bulkLat), 0.50)].Microseconds()
		rep.BulkPingP95US = bulkLat[pctIndex(len(bulkLat), 0.95)].Microseconds()
		rep.BulkPingMaxUS = bulkLat[len(bulkLat)-1].Microseconds()
	}
	rep.PingsSent = pingsSent
	rep.PingsOK = pingsOK
	rep.PingsMissed = pingsMissed
	rep.PingsLate = pingsLate
	rep.PingsAfterStream = pingsAfterStream
	rep.StreamBytes = cfg.StreamBytes
	rep.StreamMs = streamDuration.Milliseconds()
	if micros := streamDuration.Microseconds(); micros > 0 {
		rep.AggregateBytesPerS = int64(2*cfg.StreamBytes) * 1e6 / micros
	}
	rep.InterleaveSwitches = switches
	for _, id := range []uint16{FirstStreamID, LastStreamID} {
		st := inc[id]
		rep.Streams = append(rep.Streams, StreamReport{
			ID:             id,
			Bytes:          st.sentTotal,
			Chunks:         st.chunks,
			SentSHA256:     hex.EncodeToString(st.sentSum[:]),
			ReceivedSHA256: hex.EncodeToString(st.hash.Sum(nil)),
		})
	}
	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep, nil
}

func pctIndex(n int, p float64) int {
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}
