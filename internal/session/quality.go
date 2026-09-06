package session

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mmc/all-share/internal/capture"
	"github.com/mmc/all-share/shared/protocol"
)

// QualityController turns a bandwidth estimate into concrete encoder settings.
//
// The hard part is not lowering quality — it is doing so without oscillating.
// Bitrate can follow the estimate closely because changing it is free, but
// every resolution change costs a keyframe and is plainly visible, so those are
// gated behind both a margin and a dwell time. The result is a stream that
// drifts down under pressure and climbs back deliberately, rather than pumping.
type QualityController struct {
	mu       sync.Mutex
	source   capture.Source
	log      *slog.Logger
	settings Settings

	preset     protocol.QualityPreset
	policy     protocol.ResolutionPolicy
	adaptive   bool
	maxFPS     int
	maxBitrate int
	minBitrate int
	fixedW     int
	fixedH     int

	nativeWidth  int
	nativeHeight int

	currentBitrate int
	currentFPS     int
	currentRung    int
	lastRungChange time.Time
	lowSince       time.Time
	highSince      time.Time
}

// QualityConfig configures a controller.
type QualityConfig struct {
	Source   capture.Source
	Settings Settings
	Log      *slog.Logger
}

// Tuning for the adaptive loop.
const (
	// rungDwell is the minimum time between resolution changes. Long enough
	// that a brief dip cannot trigger one, short enough that a real change in
	// conditions is followed within a couple of seconds.
	rungDwell = 4 * time.Second

	// downgradeAfter and upgradeAfter are how long a condition must persist.
	// Going down is quicker than coming up, because a stream that is too big
	// for the link is actively painful while one that is too small is merely
	// soft.
	downgradeAfter = 1500 * time.Millisecond
	upgradeAfter   = 6 * time.Second

	// upgradeMargin is the headroom required before climbing a rung, so the
	// step up does not immediately overshoot the link and step back down.
	upgradeMargin = 1.35

	minUsableFPS = 15
)

// resolutionRung is one step on the quality ladder.
type resolutionRung struct {
	scale float64
	// minBitrate is roughly what this rung needs to look right at 60 fps for
	// screen content. Below it, dropping a rung buys more than the bits saved.
	minBitrate int
}

// The ladder is deliberately coarse. Small resolution steps cost a keyframe for
// a change nobody can see, so each rung is a meaningful drop.
var ladder = []resolutionRung{
	{scale: 1.00, minBitrate: 12_000_000},
	{scale: 0.75, minBitrate: 6_000_000},
	{scale: 0.60, minBitrate: 3_500_000},
	{scale: 0.50, minBitrate: 2_000_000},
	{scale: 0.375, minBitrate: 1_000_000},
	{scale: 0.30, minBitrate: 500_000},
}

// NewQualityController starts a controller for a running source.
func NewQualityController(cfg QualityConfig) *QualityController {
	info := cfg.Source.Info()
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	q := &QualityController{
		source:         cfg.Source,
		log:            log,
		settings:       cfg.Settings,
		preset:         cfg.Settings.Preset,
		policy:         protocol.ResolutionAuto,
		adaptive:       true,
		maxFPS:         cfg.Settings.MaxFPS,
		maxBitrate:     cfg.Settings.MaxBitrate,
		minBitrate:     cfg.Settings.MinBitrate,
		nativeWidth:    info.Width,
		nativeHeight:   info.Height,
		currentBitrate: cfg.Settings.StartBitrate,
		currentFPS:     cfg.Settings.MaxFPS,
		lastRungChange: time.Now(),
	}
	if q.maxFPS <= 0 {
		q.maxFPS = 60
	}
	return q
}

// Apply takes the client's explicit quality request.
func (q *QualityController) Apply(request protocol.SetQuality) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if request.Preset != "" {
		q.preset = request.Preset
		q.source.SetPreset(request.Preset)
	}
	if request.Resolution != "" {
		q.policy = request.Resolution
	}
	q.fixedW, q.fixedH = request.Width, request.Height
	q.adaptive = request.Adaptive

	if request.MaxFPS > 0 {
		q.maxFPS = clampInt(request.MaxFPS, minUsableFPS, 240)
	}
	if request.MaxKbps > 0 {
		q.maxBitrate = clampInt(request.MaxKbps*1000, 300_000, 200_000_000)
	}
	if request.MinKbps > 0 {
		q.minBitrate = clampInt(request.MinKbps*1000, 100_000, q.maxBitrate)
	}

	// The gaming preset trades still-image clarity for motion: a lower starting
	// rung means more bits per frame at a high frame rate, which is what makes
	// movement readable. The desktop preset does the reverse, because static
	// text at native resolution is the thing being optimised for.
	switch q.preset {
	case protocol.PresetGaming:
		q.setFPSLocked(q.maxFPS)
	case protocol.PresetDesktop:
		q.setFPSLocked(minInt(q.maxFPS, 60))
		q.applyRungLocked(0, "desktop preset prefers full resolution")
	}

	q.applyPolicyLocked()
	q.log.Info("quality settings applied",
		"preset", q.preset, "resolution", q.policy,
		"maxFps", q.maxFPS, "maxKbps", q.maxBitrate/1000, "adaptive", q.adaptive)
}

// Update runs one step of the adaptive loop against the current estimate.
func (q *QualityController) Update(estimatedBitrate int, viewport *protocol.Viewport) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if viewport != nil {
		q.considerViewportLocked(viewport)
	}
	if !q.adaptive || estimatedBitrate <= 0 {
		return
	}

	target := clampInt(estimatedBitrate, q.minBitrate, q.maxBitrate)
	// Bitrate tracks the estimate directly: changing it is free, and the
	// estimator has already smoothed the measurement.
	if abs(target-q.currentBitrate) > q.currentBitrate/20 {
		q.currentBitrate = target
		q.source.SetBitrate(target)
	}

	now := time.Now()
	rung := ladder[q.currentRung]

	switch {
	case target < rung.minBitrate:
		q.highSince = time.Time{}
		if q.lowSince.IsZero() {
			q.lowSince = now
		}
		if now.Sub(q.lowSince) >= downgradeAfter && now.Sub(q.lastRungChange) >= rungDwell {
			q.stepDownLocked(target)
		}

	case q.currentRung > 0 && float64(target) > float64(ladder[q.currentRung-1].minBitrate)*upgradeMargin:
		q.lowSince = time.Time{}
		if q.highSince.IsZero() {
			q.highSince = now
		}
		if now.Sub(q.highSince) >= upgradeAfter && now.Sub(q.lastRungChange) >= rungDwell {
			q.stepUpLocked(target)
		}

	default:
		q.lowSince = time.Time{}
		q.highSince = time.Time{}
	}
}

// stepDownLocked reduces quality by one step.
//
// Which dimension gives way first depends on what the user is doing. In a game,
// frame rate is the thing that must not drop, so resolution goes first. On a
// desktop, a sharp still image matters more than smooth motion, so frame rate
// goes first and native resolution is held as long as possible.
func (q *QualityController) stepDownLocked(target int) {
	if q.preset == protocol.PresetDesktop && q.currentFPS > 30 {
		q.setFPSLocked(30)
		q.lastRungChange = time.Now()
		q.lowSince = time.Time{}
		q.log.Info("lowering frame rate to protect image quality", "fps", 30, "targetKbps", target/1000)
		return
	}
	if q.currentRung < len(ladder)-1 {
		q.applyRungLocked(q.currentRung+1, "not enough bandwidth for the current size")
		return
	}
	// The lowest rung is already in use: frame rate is all that is left.
	if q.currentFPS > minUsableFPS {
		q.setFPSLocked(maxInt(minUsableFPS, q.currentFPS/2))
		q.lastRungChange = time.Now()
		q.lowSince = time.Time{}
		q.log.Info("lowering frame rate at the smallest size", "fps", q.currentFPS)
	}
}

func (q *QualityController) stepUpLocked(target int) {
	if q.currentFPS < q.maxFPS {
		q.setFPSLocked(q.maxFPS)
		q.lastRungChange = time.Now()
		q.highSince = time.Time{}
		q.log.Info("restoring frame rate", "fps", q.maxFPS, "targetKbps", target/1000)
		return
	}
	if q.currentRung > 0 {
		q.applyRungLocked(q.currentRung-1, "bandwidth recovered")
	}
}

func (q *QualityController) applyRungLocked(rung int, reason string) {
	rung = clampInt(rung, 0, len(ladder)-1)
	if rung == q.currentRung {
		return
	}
	if q.policy == protocol.ResolutionNative || q.policy == protocol.ResolutionFixed {
		// The user asked for a specific size; honour it and let bitrate absorb
		// the pressure instead of silently overriding their choice.
		return
	}
	width, height := q.sizeForRungLocked(rung)
	if err := q.source.SetResolution(width, height); err != nil {
		// A backend that cannot rescale is not a failure; bitrate adaptation
		// still applies. Log once at debug rather than repeating every tick.
		q.log.Debug("this backend cannot change resolution", "err", err)
		q.currentRung = rung
		q.lastRungChange = time.Now()
		return
	}
	q.currentRung = rung
	q.lastRungChange = time.Now()
	q.lowSince = time.Time{}
	q.highSince = time.Time{}
	q.log.Info("stream size changed", "size", itoa(width)+"x"+itoa(height), "reason", reason)
}

// sizeForRungLocked computes an encoder-friendly size for a ladder rung.
//
// Both dimensions are rounded to a multiple of 16: hardware encoders pad to
// macroblock boundaries anyway, and an unpadded size costs a crop that shows up
// as a soft edge.
func (q *QualityController) sizeForRungLocked(rung int) (int, int) {
	scale := ladder[rung].scale
	width := roundTo(int(float64(q.nativeWidth)*scale), 16)
	height := roundTo(int(float64(q.nativeHeight)*scale), 16)
	return maxInt(width, 320), maxInt(height, 180)
}

func (q *QualityController) applyPolicyLocked() {
	switch q.policy {
	case protocol.ResolutionNative:
		if err := q.source.SetResolution(q.nativeWidth, q.nativeHeight); err == nil {
			q.currentRung = 0
		}
	case protocol.ResolutionFixed:
		if q.fixedW > 0 && q.fixedH > 0 {
			if err := q.source.SetResolution(roundTo(q.fixedW, 16), roundTo(q.fixedH, 16)); err != nil {
				q.log.Debug("this backend cannot use a fixed size", "err", err)
			}
		}
	}
}

// considerViewportLocked matches the stream to the space it will be drawn in.
//
// Sending more pixels than the client can display wastes bandwidth on detail
// that is thrown away by the downscale, and — more importantly for a remote
// desktop — resampling is the main reason text goes soft. Matching the viewport
// means the browser draws the stream one-to-one.
func (q *QualityController) considerViewportLocked(viewport *protocol.Viewport) {
	if q.policy != protocol.ResolutionFit || viewport.Width <= 0 || viewport.Height <= 0 {
		return
	}
	width := roundTo(minInt(viewport.Width, q.nativeWidth), 16)
	height := roundTo(minInt(viewport.Height, q.nativeHeight), 16)
	info := q.source.Info()
	if info.Width == width && info.Height == height {
		return
	}
	if time.Since(q.lastRungChange) < rungDwell {
		return
	}
	if err := q.source.SetResolution(width, height); err == nil {
		q.lastRungChange = time.Now()
		q.log.Info("matched the stream to the client's window", "size", itoa(width)+"x"+itoa(height))
	}
}

func (q *QualityController) setFPSLocked(fps int) {
	fps = clampInt(fps, minUsableFPS, 240)
	if fps == q.currentFPS {
		return
	}
	q.currentFPS = fps
	q.source.SetFrameRate(fps)
}

// Snapshot reports the controller's current decisions, for tests and status.
func (q *QualityController) Snapshot() (bitrate, fps, rung int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.currentBitrate, q.currentFPS, q.currentRung
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func roundTo(value, multiple int) int {
	if multiple <= 1 {
		return value
	}
	return ((value + multiple/2) / multiple) * multiple
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
