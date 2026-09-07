//go:build windows

// Windows screen capture, bridged to the native Direct3D and Media Foundation
// pipeline in agent/native.
//
// Go never calls into Direct3D. The native layer owns a dedicated capture
// thread — Desktop Duplication and an asynchronous encoder MFT both want a
// single COM apartment and predictable timing — and this file only drains it.
package capture

/*
#cgo CXXFLAGS: -std=c++17 -O2
#cgo CFLAGS: -I${SRCDIR}/../../agent/native
// The C++ runtime is linked statically on purpose. A one-click installer that
// also has to place libstdc++-6.dll next to the executable is a support burden
// and a class of "it works on my machine" failure; -Wl,-Bstatic around -lstdc++
// is what actually forces the archive, because the -static-libstdc++ driver
// option is a g++ flag and cgo links through gcc.
#cgo LDFLAGS: -L${SRCDIR}/../../agent/native/build -lallshare_capture -lopus
#cgo LDFLAGS: -Wl,-Bstatic -lstdc++ -Wl,-Bdynamic
#cgo LDFLAGS: -ld3d11 -ldxgi -lmfplat -lmfuuid -lmf -lole32 -loleaut32 -luuid -lwinmm
#cgo LDFLAGS: -static-libgcc

#include <stdlib.h>
#include "allshare_capture.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/mmc/all-share/shared/protocol"
)

// WindowsProvider opens capture sessions backed by the native pipeline.
type WindowsProvider struct {
	log *slog.Logger

	mu       sync.Mutex
	sessions []*windowsSource
	closed   bool
}

// NewWindowsProvider initialises the native layer.
func NewWindowsProvider(log *slog.Logger) (*WindowsProvider, error) {
	if log == nil {
		log = slog.Default()
	}
	errBuf := make([]C.char, C.AS_MAX_ERROR)
	if rc := C.as_initialize(&errBuf[0], C.int32_t(len(errBuf))); rc != C.AS_OK {
		return nil, fmt.Errorf("allshare/capture: %s", C.GoString(&errBuf[0]))
	}
	return &WindowsProvider{log: log}, nil
}

// Name identifies the backend in logs and in the client's status panel.
func (p *WindowsProvider) Name() string { return "Desktop Duplication" }

// Capabilities lists the encoders this machine can actually use.
func (p *WindowsProvider) Capabilities() []Capability {
	const max = 4
	buf := make([]C.as_capability, max)
	n := int(C.as_query_capabilities(&buf[0], C.int32_t(max)))
	if n <= 0 {
		return nil
	}
	out := make([]Capability, 0, n)
	for i := 0; i < n; i++ {
		entry := buf[i]
		capability := Capability{
			Hardware:  entry.hardware != 0,
			Encoder:   goString(entry.encoder[:]),
			MaxWidth:  int(entry.max_width),
			MaxHeight: int(entry.max_height),
			MaxFPS:    int(entry.max_fps),
		}
		switch entry.codec {
		case C.AS_CODEC_H264:
			capability.Codec = CodecH264
		case C.AS_CODEC_H265:
			capability.Codec = CodecH265
		default:
			continue
		}
		capability.Profiles = splitFields(goString(entry.profiles[:]))
		out = append(out, capability)
	}
	return out
}

// Monitors lists the displays attached to this machine.
func (p *WindowsProvider) Monitors() []protocol.Monitor {
	const max = 16
	buf := make([]C.as_monitor, max)
	n := int(C.as_enumerate_monitors(&buf[0], C.int32_t(max)))
	if n <= 0 {
		return nil
	}
	out := make([]protocol.Monitor, 0, n)
	for i := 0; i < n; i++ {
		entry := buf[i]
		out = append(out, protocol.Monitor{
			ID:           int(entry.id),
			Name:         goString(entry.name[:]),
			Width:        int(entry.width),
			Height:       int(entry.height),
			X:            int(entry.x),
			Y:            int(entry.y),
			Primary:      entry.primary != 0,
			RefreshHz:    int(entry.refresh_hz),
			ScalePercent: int(entry.scale_percent),
		})
	}
	return out
}

// Open starts a capture session.
func (p *WindowsProvider) Open(opts Options) (Source, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("allshare/capture: the capture provider is closed")
	}
	p.mu.Unlock()

	codec := C.int32_t(C.AS_CODEC_H264)
	if opts.Codec == CodecH265 {
		codec = C.AS_CODEC_H265
	} else if opts.Codec != "" && opts.Codec != CodecH264 {
		return nil, fmt.Errorf("%w: this PC cannot encode %s", ErrUnsupported, opts.Codec)
	}

	preset := C.int32_t(C.AS_PRESET_BALANCED)
	switch opts.Preset {
	case protocol.PresetGaming:
		preset = C.AS_PRESET_GAMING
	case protocol.PresetDesktop:
		preset = C.AS_PRESET_DESKTOP
	}

	options := C.as_open_options{
		codec:          codec,
		width:          C.int32_t(opts.Width),
		height:         C.int32_t(opts.Height),
		fps:            C.int32_t(opts.FPS),
		bitrate:        C.int32_t(opts.Bitrate),
		monitor_id:     C.int32_t(opts.MonitorID),
		exclude_cursor: boolToC(opts.ExcludeCursor),
		preset:         preset,
	}

	errBuf := make([]C.char, C.AS_MAX_ERROR)
	handle := C.as_open(&options, &errBuf[0], C.int32_t(len(errBuf)))
	if handle == nil {
		return nil, fmt.Errorf("allshare/capture: %s", C.GoString(&errBuf[0]))
	}

	source := &windowsSource{
		handle:   handle,
		provider: p,
		log:      p.log,
		frames:   make(chan Frame, 1),
		cursor:   make(chan CursorUpdate, 8),
		done:     make(chan struct{}),
		monitors: p.Monitors(),
	}
	source.readInfo()
	if opts.AudioEnabled {
		source.startAudio(128000)
		source.readInfo()
	}

	p.mu.Lock()
	p.sessions = append(p.sessions, source)
	p.mu.Unlock()

	source.wg.Add(2)
	go source.pumpFrames()
	go source.pumpCursor()
	return source, nil
}

// Close shuts down every session and the native layer.
func (p *WindowsProvider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sessions := append([]*windowsSource(nil), p.sessions...)
	p.sessions = nil
	p.mu.Unlock()

	for _, source := range sessions {
		_ = source.Close()
	}
	C.as_shutdown()
	return nil
}

// windowsSource is one running capture session.
type windowsSource struct {
	handle   *C.as_capture
	provider *WindowsProvider
	log      *slog.Logger

	frames      chan Frame
	cursor      chan CursorUpdate
	audio       chan AudioFrame
	audioHandle *C.as_audio
	hasAudio    bool
	done        chan struct{}
	wg          sync.WaitGroup

	closeOnce sync.Once
	closed    atomic.Bool

	infoMu   sync.RWMutex
	info     Info
	monitors []protocol.Monitor
}

func (s *windowsSource) Frames() <-chan Frame        { return s.frames }
func (s *windowsSource) Audio() <-chan AudioFrame    { return s.audio }
func (s *windowsSource) Cursor() <-chan CursorUpdate { return s.cursor }

// startAudio opens system audio capture alongside the video pipeline.
//
// Audio is a separate handle on purpose: it should keep playing across a
// display change or a capture restart, and muting should stop the capture
// entirely rather than encode sound nobody is listening to.
func (s *windowsSource) startAudio(bitrate int) {
	if C.as_audio_available() == 0 {
		s.log.Info("this build of ALL SHARE has no audio support")
		return
	}
	errBuf := make([]C.char, C.AS_MAX_ERROR)
	handle := C.as_audio_open(C.int32_t(bitrate), &errBuf[0], C.int32_t(len(errBuf)))
	if handle == nil {
		// Sound is a convenience; losing it must not cost the user their
		// session, so this is reported and the video stream carries on.
		s.log.Warn("sound is unavailable", "reason", C.GoString(&errBuf[0]))
		return
	}
	s.audioHandle = handle
	s.audio = make(chan AudioFrame, 8)
	s.hasAudio = true
	s.wg.Add(1)
	go s.pumpAudio()
}

func (s *windowsSource) pumpAudio() {
	defer s.wg.Done()
	defer close(s.audio)

	// A 20 ms Opus frame arrives every 20 ms; polling at 5 ms keeps latency
	// well under one frame without spinning.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	var frame C.as_audio_frame
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		for C.as_audio_next(s.audioHandle, &frame) == 1 {
			if frame.size <= 0 || frame.data == nil {
				continue
			}
			out := AudioFrame{
				Data:       C.GoBytes(unsafe.Pointer(frame.data), C.int(frame.size)),
				Duration:   time.Duration(frame.duration_us) * time.Microsecond,
				CapturedAt: time.UnixMicro(int64(frame.capture_time_us)),
			}
			select {
			case s.audio <- out:
			case <-s.done:
				return
			default:
				// Audio that has queued is audio that will arrive out of sync.
			}
		}
	}
}

func (s *windowsSource) pumpFrames() {
	defer s.wg.Done()
	defer close(s.frames)

	var frame C.as_frame
	for {
		select {
		case <-s.done:
			return
		default:
		}

		// A short timeout keeps this loop responsive to Close without polling
		// hot. AS_TIMEOUT is the normal case on a still desktop, not an error.
		rc := C.as_next_frame(s.handle, 100, &frame)
		switch rc {
		case C.AS_OK:
		case C.AS_TIMEOUT:
			continue
		case C.AS_ERR_CLOSED:
			return
		default:
			s.log.Warn("screen capture reported a problem", "code", int(rc))
			continue
		}
		if frame.size <= 0 || frame.data == nil {
			continue
		}

		// The native buffer is only valid until the next call, so the bytes are
		// copied here rather than handed onward as a borrowed pointer.
		payload := C.GoBytes(unsafe.Pointer(frame.data), C.int(frame.size))

		out := Frame{
			Data:           payload,
			Keyframe:       frame.keyframe != 0,
			CapturedAt:     time.UnixMicro(int64(frame.capture_time_us)),
			EncodeDuration: time.Duration(frame.encode_us) * time.Microsecond,
			Width:          int(frame.width),
			Height:         int(frame.height),
		}
		select {
		case s.frames <- out:
		case <-s.done:
			return
		default:
			// The consumer is behind. A stale frame is worth less than the next
			// one, so it is dropped rather than queued.
		}
	}
}

func (s *windowsSource) pumpCursor() {
	defer s.wg.Done()
	defer close(s.cursor)

	ticker := time.NewTicker(8 * time.Millisecond)
	defer ticker.Stop()

	var update C.as_cursor
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		for C.as_next_cursor(s.handle, &update) == 1 {
			out := CursorUpdate{
				ShapeID: uint32(update.shape_id),
				Width:   int(update.width),
				Height:  int(update.height),
				HotX:    int(update.hot_x),
				HotY:    int(update.hot_y),
				X:       uint16(clamp32(int32(update.x), 0, 65535)),
				Y:       uint16(clamp32(int32(update.y), 0, 65535)),
				Visible: update.visible != 0,
			}
			if update.bgra != nil && out.Width > 0 && out.Height > 0 {
				raw := C.GoBytes(unsafe.Pointer(update.bgra), C.int(out.Width*out.Height*4))
				// PNG encoding happens here rather than in C++: Go has an
				// encoder in its standard library, and a cursor changes rarely
				// enough that the cost is irrelevant.
				if encoded, err := encodeCursorPNG(raw, out.Width, out.Height); err == nil {
					out.Shape = encoded
				}
			}
			select {
			case s.cursor <- out:
			case <-s.done:
				return
			default:
				// Only the newest pointer position matters.
			}
		}
	}
}

// encodeCursorPNG converts the BGRA bitmap Windows produces into a PNG the
// browser can draw.
func encodeCursorPNG(bgra []byte, width, height int) ([]byte, error) {
	if len(bgra) < width*height*4 {
		return nil, errors.New("allshare/capture: cursor bitmap is truncated")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 4
			img.SetNRGBA(x, y, color.NRGBA{
				R: bgra[i+2], G: bgra[i+1], B: bgra[i+0], A: bgra[i+3],
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *windowsSource) SetBitrate(bitsPerSecond int) {
	if s.closed.Load() || bitsPerSecond <= 0 {
		return
	}
	C.as_set_bitrate(s.handle, C.int32_t(bitsPerSecond))
}

func (s *windowsSource) SetFrameRate(fps int) {
	if s.closed.Load() || fps <= 0 {
		return
	}
	C.as_set_framerate(s.handle, C.int32_t(fps))
	s.readInfo()
}

func (s *windowsSource) SetResolution(width, height int) error {
	if s.closed.Load() {
		return errors.New("allshare/capture: the session is closed")
	}
	if rc := C.as_set_resolution(s.handle, C.int32_t(width), C.int32_t(height)); rc != C.AS_OK {
		return fmt.Errorf("%w: this PC cannot stream at %dx%d", ErrUnsupported, width, height)
	}
	s.readInfo()
	return nil
}

func (s *windowsSource) SetMonitor(id int) error {
	if s.closed.Load() {
		return errors.New("allshare/capture: the session is closed")
	}
	if rc := C.as_set_monitor(s.handle, C.int32_t(id)); rc != C.AS_OK {
		return fmt.Errorf("%w: display %d could not be captured", ErrUnsupported, id)
	}
	// The pipeline is rebuilt asynchronously; give it a moment so the reported
	// size matches the new display rather than the old one.
	time.Sleep(150 * time.Millisecond)
	s.readInfo()
	return nil
}

func (s *windowsSource) SetPreset(preset protocol.QualityPreset) {
	if s.closed.Load() {
		return
	}
	value := C.int32_t(C.AS_PRESET_BALANCED)
	switch preset {
	case protocol.PresetGaming:
		value = C.AS_PRESET_GAMING
	case protocol.PresetDesktop:
		value = C.AS_PRESET_DESKTOP
	}
	C.as_set_preset(s.handle, value)
}

func (s *windowsSource) RequestKeyframe() {
	if !s.closed.Load() {
		C.as_request_keyframe(s.handle)
	}
}

func (s *windowsSource) NoteInput(seq uint32) {
	if !s.closed.Load() {
		C.as_note_input(s.handle, C.uint32_t(seq))
	}
}

func (s *windowsSource) readInfo() {
	var info C.as_info
	if C.as_get_info(s.handle, &info) != C.AS_OK {
		return
	}
	codec := CodecH264
	if info.codec == C.AS_CODEC_H265 {
		codec = CodecH265
	}
	s.infoMu.Lock()
	s.info = Info{
		Codec:          codec,
		Profile:        goString(info.profile[:]),
		Encoder:        goString(info.encoder[:]),
		Backend:        goString(info.backend[:]),
		Hardware:       info.hardware != 0,
		Width:          int(info.width),
		Height:         int(info.height),
		FPS:            int(info.fps),
		Monitors:       s.monitors,
		ActiveMonitor:  int(info.monitor_id),
		HasAudio:       s.hasAudio,
		CursorEmbedded: info.cursor_embedded != 0,
		SessionKind:    "desktop",
	}
	s.infoMu.Unlock()
}

func (s *windowsSource) Info() Info {
	s.infoMu.RLock()
	defer s.infoMu.RUnlock()
	return s.info
}

func (s *windowsSource) Stats() Stats {
	if s.closed.Load() {
		return Stats{}
	}
	var stats C.as_stats
	if C.as_get_stats(s.handle, &stats) != C.AS_OK {
		return Stats{}
	}
	return Stats{
		CaptureFPS:    float32(stats.capture_fps),
		EncodeFPS:     float32(stats.encode_fps),
		CaptureMillis: float32(stats.capture_ms),
		EncodeMillis:  float32(stats.encode_ms),
		QueueMillis:   float32(stats.queue_ms),
		QP:            float32(stats.qp),
		IdleSkipped:   uint32(stats.idle_skipped),
		Width:         int(stats.width),
		Height:        int(stats.height),
	}
}

func (s *windowsSource) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
		s.wg.Wait()
		if s.audioHandle != nil {
			C.as_audio_close(s.audioHandle)
			s.audioHandle = nil
		}
		C.as_close(s.handle)
		s.handle = nil
	})
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func goString(buf []C.char) string {
	out := make([]byte, 0, len(buf))
	for _, c := range buf {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return out
}

func boolToC(v bool) C.int32_t {
	if v {
		return 1
	}
	return 0
}

func clamp32(v, low, high int32) int32 {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
