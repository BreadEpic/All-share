// Package testsource provides a capture backend that works on any platform.
//
// It replays a real, pre-encoded VP8 bitstream at a controlled frame rate. That
// matters: an end-to-end test can then assert that a browser genuinely decoded
// frames, which exercises the whole path — packetisation, congestion control,
// jitter buffer, hardware decode — rather than only proving that a peer
// connection opened.
//
// VP8 is used because it is the one video codec present in every Chromium
// build, including the headless builds used in CI, which ship without H.264.
//
// The backend cannot re-encode, so bitrate requests are recorded rather than
// obeyed and frame-rate requests are honoured by dropping frames. Tests assert
// on the control signals the adaptive controller produces, which is the honest
// thing to check here; whether an encoder then hits its target is the real
// backend's business.
package testsource

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmc/all-share/internal/capture"
	"github.com/mmc/all-share/shared/protocol"
)

//go:embed testdata/pattern.asvp
var patternData []byte

// Provider opens synthetic capture sources.
type Provider struct {
	frames  []frame
	width   int
	height  int
	fps     int
	mu      sync.Mutex
	sources []*Source
}

type frame struct {
	data     []byte
	keyframe bool
}

// New loads the embedded pattern and returns a provider.
func New() (*Provider, error) {
	frames, width, height, fps, err := parsePattern(patternData)
	if err != nil {
		return nil, err
	}
	return &Provider{frames: frames, width: width, height: height, fps: fps}, nil
}

// Name identifies the backend in logs and in the client's status panel.
func (p *Provider) Name() string { return "Test pattern" }

// Capabilities reports the single codec this backend can replay.
func (p *Provider) Capabilities() []capture.Capability {
	return []capture.Capability{{
		Codec:     capture.CodecVP8,
		Hardware:  false,
		Encoder:   "Pre-encoded test pattern",
		MaxWidth:  p.width,
		MaxHeight: p.height,
		MaxFPS:    p.fps,
	}}
}

// Monitors reports a single synthetic display.
func (p *Provider) Monitors() []protocol.Monitor {
	return []protocol.Monitor{{
		ID: 0, Name: "Test display", Width: p.width, Height: p.height,
		Primary: true, RefreshHz: p.fps, ScalePercent: 100,
	}}
}

// Open starts a replay session.
func (p *Provider) Open(opts capture.Options) (capture.Source, error) {
	if opts.Codec != "" && opts.Codec != capture.CodecVP8 {
		return nil, fmt.Errorf("%w: test pattern only produces VP8, not %s", capture.ErrUnsupported, opts.Codec)
	}
	fps := opts.FPS
	if fps <= 0 || fps > p.fps {
		fps = p.fps
	}
	source := &Source{
		provider: p,
		// A capture-to-transport queue is pure latency: a frame that waits
		// behind seven others is delivered a quarter of a second late for no
		// benefit, because by then a newer frame exists. Depth one lets the
		// producer stay a single frame ahead and no more.
		frames: make(chan capture.Frame, 1),
		audio:  make(chan capture.AudioFrame),
		cursor: make(chan capture.CursorUpdate, 8),
		done:   make(chan struct{}),
		info: capture.Info{
			Codec:         capture.CodecVP8,
			Encoder:       "Pre-encoded test pattern",
			Backend:       "Test pattern",
			Hardware:      false,
			Width:         p.width,
			Height:        p.height,
			FPS:           fps,
			Monitors:      p.Monitors(),
			ActiveMonitor: 0,
			SessionKind:   "desktop",
		},
	}
	source.targetFPS.Store(int64(fps))
	source.bitrate.Store(int64(opts.Bitrate))

	p.mu.Lock()
	p.sources = append(p.sources, source)
	p.mu.Unlock()

	go source.run()
	go source.emitCursor()
	return source, nil
}

// Close stops every source this provider opened.
func (p *Provider) Close() error {
	p.mu.Lock()
	sources := append([]*Source(nil), p.sources...)
	p.sources = nil
	p.mu.Unlock()
	for _, s := range sources {
		_ = s.Close()
	}
	return nil
}

// Source is one replay session.
type Source struct {
	provider *Provider
	frames   chan capture.Frame
	audio    chan capture.AudioFrame
	cursor   chan capture.CursorUpdate
	done     chan struct{}
	closeOne sync.Once

	info capture.Info

	targetFPS   atomic.Int64
	bitrate     atomic.Int64
	inputSeq    atomic.Uint32
	keyframeReq atomic.Bool

	// Counters the tests read to confirm the adaptive controller acted.
	bitrateCalls atomic.Int64
	fpsCalls     atomic.Int64
	keyframes    atomic.Int64
	sent         atomic.Int64
	dropped      atomic.Int64

	statsMu   sync.Mutex
	lastStats capture.Stats
}

// Frames returns the encoded video stream.
func (s *Source) Frames() <-chan capture.Frame { return s.frames }

// Audio returns the encoded audio stream. The test backend produces none.
func (s *Source) Audio() <-chan capture.AudioFrame { return s.audio }

// Cursor returns pointer updates.
func (s *Source) Cursor() <-chan capture.CursorUpdate { return s.cursor }

// SetBitrate records a requested bitrate. A canned stream cannot honour it, so
// the value is recorded for tests rather than silently ignored.
func (s *Source) SetBitrate(bitsPerSecond int) {
	s.bitrate.Store(int64(bitsPerSecond))
	s.bitrateCalls.Add(1)
}

// SetFrameRate changes the replay rate, dropping frames to reach it.
func (s *Source) SetFrameRate(fps int) {
	if fps <= 0 {
		return
	}
	if fps > s.provider.fps {
		fps = s.provider.fps
	}
	s.targetFPS.Store(int64(fps))
	s.fpsCalls.Add(1)
}

// SetResolution is not supported; the pattern is fixed.
func (s *Source) SetResolution(width, height int) error {
	return fmt.Errorf("%w: the test pattern is a fixed %dx%d", capture.ErrUnsupported, s.info.Width, s.info.Height)
}

// SetMonitor accepts only the single synthetic display.
func (s *Source) SetMonitor(id int) error {
	if id != 0 {
		return fmt.Errorf("%w: the test pattern has one display", capture.ErrUnsupported)
	}
	return nil
}

// SetPreset is accepted and ignored.
func (s *Source) SetPreset(protocol.QualityPreset) {}

// RequestKeyframe asks the replay to jump to the next keyframe.
func (s *Source) RequestKeyframe() {
	s.keyframeReq.Store(true)
	s.keyframes.Add(1)
}

// NoteInput records the newest applied input sequence.
func (s *Source) NoteInput(seq uint32) { s.inputSeq.Store(seq) }

// Info describes the running session.
func (s *Source) Info() capture.Info { return s.info }

// Stats reports the replay's own counters.
func (s *Source) Stats() capture.Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.lastStats
}

// Close stops the replay.
func (s *Source) Close() error {
	s.closeOne.Do(func() { close(s.done) })
	return nil
}

// Counters exposes replay counts to tests.
func (s *Source) Counters() (framesSent, framesDropped, bitrateCalls, fpsCalls, keyframeRequests int64) {
	return s.sent.Load(), s.dropped.Load(), s.bitrateCalls.Load(), s.fpsCalls.Load(), s.keyframes.Load()
}

// TargetBitrate reports the most recently requested bitrate.
func (s *Source) TargetBitrate() int { return int(s.bitrate.Load()) }

func (s *Source) run() {
	defer close(s.frames)
	defer close(s.audio)

	index := 0
	nativeFPS := s.provider.fps
	// Frames are paced against a fixed deadline rather than a sleep-per-frame,
	// so a slow tick does not accumulate drift the way repeated sleeps do.
	interval := time.Second / time.Duration(nativeFPS)
	next := time.Now()
	frameCount := 0
	statsWindow := time.Now()
	var sentInWindow int

	for {
		select {
		case <-s.done:
			return
		default:
		}

		wait := time.Until(next)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-s.done:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		next = next.Add(interval)

		item := s.provider.frames[index]
		index = (index + 1) % len(s.provider.frames)

		// A keyframe request is satisfied by skipping forward to the next one,
		// which is the closest a canned stream can get to a real encoder's IDR.
		if s.keyframeReq.Load() && !item.keyframe {
			for scan := 0; scan < len(s.provider.frames); scan++ {
				if s.provider.frames[index].keyframe {
					break
				}
				index = (index + 1) % len(s.provider.frames)
			}
			item = s.provider.frames[index]
			index = (index + 1) % len(s.provider.frames)
		}
		if item.keyframe {
			s.keyframeReq.Store(false)
		}

		// Frame-rate limiting: keep every nth frame, but never drop a keyframe,
		// because a decoder that misses one has nothing to build on.
		target := int(s.targetFPS.Load())
		frameCount++
		if target > 0 && target < nativeFPS && !item.keyframe {
			keepEvery := float64(nativeFPS) / float64(target)
			if float64(frameCount)-float64(int(float64(frameCount)/keepEvery)*int(keepEvery)) > 0.5 &&
				frameCount%int(keepEvery+0.5) != 0 {
				s.dropped.Add(1)
				continue
			}
		}

		out := capture.Frame{
			Data:           item.data,
			Keyframe:       item.keyframe,
			CapturedAt:     time.Now(),
			EncodeDuration: 2 * time.Millisecond,
			Width:          s.info.Width,
			Height:         s.info.Height,
			InputSeq:       s.inputSeq.Load(),
		}
		select {
		case s.frames <- out:
			s.sent.Add(1)
			sentInWindow++
		case <-s.done:
			return
		default:
			// The consumer is behind. Dropping the newest frame is right for a
			// live stream: a late frame is worth less than the next one.
			s.dropped.Add(1)
		}

		if elapsed := time.Since(statsWindow); elapsed >= time.Second {
			s.statsMu.Lock()
			s.lastStats = capture.Stats{
				CaptureFPS:    float32(float64(sentInWindow) / elapsed.Seconds()),
				EncodeFPS:     float32(float64(sentInWindow) / elapsed.Seconds()),
				CaptureMillis: 0.5,
				EncodeMillis:  2,
				Width:         s.info.Width,
				Height:        s.info.Height,
			}
			s.statsMu.Unlock()
			sentInWindow = 0
			statsWindow = time.Now()
		}
	}
}

// emitCursor publishes a synthetic pointer that moves in a slow circle, so the
// client's local cursor rendering has something real to follow.
func (s *Source) emitCursor() {
	defer close(s.cursor)
	shape, err := arrowPNG()
	if err != nil {
		return
	}
	select {
	case s.cursor <- capture.CursorUpdate{
		ShapeID: 1, Shape: shape, Width: 16, Height: 24, HotX: 0, HotY: 0,
		X: 32767, Y: 32767, Visible: true,
	}:
	case <-s.done:
		return
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			phase := time.Since(start).Seconds() / 6
			x := uint16(32767 + 20000*sin(phase*2*3.14159265))
			y := uint16(32767 + 12000*cos(phase*2*3.14159265))
			select {
			case s.cursor <- capture.CursorUpdate{ShapeID: 1, X: x, Y: y, Visible: true}:
			case <-s.done:
				return
			default:
			}
		}
	}
}

// arrowPNG draws a simple pointer so the client has a real bitmap to render.
func arrowPNG() ([]byte, error) {
	const w, h = 16, 24
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	black := color.NRGBA{A: 255}
	for y := 0; y < h; y++ {
		width := y * 2 / 3
		if width > w-2 {
			width = w - 2
		}
		for x := 0; x <= width; x++ {
			if x == 0 || x == width || y == h-1 {
				img.Set(x, y, black)
			} else {
				img.Set(x, y, white)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parsePattern reads the flat frame container produced by test/tools/mkpattern.
func parsePattern(data []byte) ([]frame, int, int, int, error) {
	const headerSize = 4 + 1 + 4 + 2 + 2 + 2 + 4
	if len(data) < headerSize {
		return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern file is truncated")
	}
	if string(data[0:4]) != "ASVP" || data[4] != 1 {
		return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern file has an unexpected header")
	}
	if string(data[5:8]) != "VP8" {
		return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern codec %q is not supported", data[5:8])
	}
	width := int(binary.LittleEndian.Uint16(data[9:]))
	height := int(binary.LittleEndian.Uint16(data[11:]))
	fps := int(binary.LittleEndian.Uint16(data[13:]))
	count := int(binary.LittleEndian.Uint32(data[15:]))
	if fps <= 0 || width <= 0 || height <= 0 || count <= 0 {
		return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern header is not usable")
	}

	frames := make([]frame, 0, count)
	pos := headerSize
	for i := 0; i < count; i++ {
		if pos+5 > len(data) {
			return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern ends after %d of %d frames", i, count)
		}
		size := int(binary.LittleEndian.Uint32(data[pos:]))
		keyframe := data[pos+4] != 0
		pos += 5
		if size < 0 || pos+size > len(data) {
			return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: frame %d declares %d bytes past the end of the file", i, size)
		}
		frames = append(frames, frame{data: data[pos : pos+size], keyframe: keyframe})
		pos += size
	}
	if len(frames) == 0 {
		return nil, 0, 0, 0, fmt.Errorf("allshare/testsource: pattern contains no frames")
	}
	return frames, width, height, fps, nil
}

// Small trig helpers, kept local so the package pulls in no maths dependency
// for what is only used to wiggle a test cursor.
func sin(x float64) float64 { return taylorSin(normalizeAngle(x)) }
func cos(x float64) float64 { return taylorSin(normalizeAngle(x + 1.5707963267948966)) }

func normalizeAngle(x float64) float64 {
	const twoPi = 6.283185307179586
	for x > 3.141592653589793 {
		x -= twoPi
	}
	for x < -3.141592653589793 {
		x += twoPi
	}
	return x
}

func taylorSin(x float64) float64 {
	x2 := x * x
	return x * (1 - x2/6*(1-x2/20*(1-x2/42*(1-x2/72))))
}
