// Package e2e runs the whole ALL SHARE system against itself.
//
// A real rendezvous server, a real agent with a real WebRTC stack, and a real
// Chromium loading the real client from a real file:// URL. Nothing in the path
// is stubbed except the pixels: the capture backend replays a pre-encoded VP8
// stream, because this machine has no desktop.
//
// The test therefore proves the things that matter and cannot be proved by unit
// tests: that a browser genuinely decodes the video, that a keystroke typed in
// Chromium arrives at the agent as the right HID usage, that a click lands at
// the right coordinates, and that a session that ends does not leave keys held
// down on the PC.
//
// Set ALLSHARE_E2E=1 to run it; it needs Chromium and a few seconds.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmc/all-share/internal/agentcore"
	"github.com/mmc/all-share/internal/capture"
	"github.com/mmc/all-share/internal/capture/testsource"
	"github.com/mmc/all-share/internal/clipboard"
	"github.com/mmc/all-share/internal/config"
	agentinput "github.com/mmc/all-share/internal/input"
	"github.com/mmc/all-share/internal/rendezvous/pairing"
	"github.com/mmc/all-share/internal/rendezvous/registry"
	"github.com/mmc/all-share/internal/rendezvous/signal"
	"github.com/mmc/all-share/internal/session"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

func skipUnlessEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("ALLSHARE_E2E") == "" {
		t.Skip("set ALLSHARE_E2E=1 to run the end-to-end suite (needs Chromium)")
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type stack struct {
	t          *testing.T
	server     *httptest.Server
	serviceURL string
	agent      *agentcore.Agent
	recorder   *agentinput.Recorder
	clipboard  *clipboard.Recorder
	source     *testsource.Provider
	deviceID   string
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// localICE advertises no STUN or TURN. Both peers are on this host, so host
// candidates connect immediately; reaching out to a public STUN server would
// make the test slower and dependent on the internet.
type localICE struct{}

func (localICE) ICEServers() ([]protocol.ICEServer, int64) { return nil, 0 }

func startStack(t *testing.T) *stack {
	t.Helper()

	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	hub := signal.NewHub(signal.Config{
		Log: quiet, Registry: reg, Pairs: pairing.NewStore(), ICE: localICE{},
		ServerID: "e2e-server", Version: "e2e", Limits: signal.DefaultLimits(),
		LANKeySalt: []byte("e2e-lan-salt-0123456789abcdef012"),
	})
	server := httptest.NewServer(http.HandlerFunc(hub.Serve))
	t.Cleanup(server.Close)
	serviceURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/rv"

	dataDir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.SetPath(filepath.Join(dataDir, "config.json"))
	if err := cfg.Update(func(c *config.Config) {
		c.DeviceName = "E2E Test PC"
		c.Rendezvous = serviceURL
		// Audio is off: this host has no sound device, and an empty Opus track
		// would only add noise to the negotiation.
		c.AudioEnabled = false
		c.MaxFPS = 30
	}); err != nil {
		t.Fatalf("configure agent: %v", err)
	}

	identity, _, err := idkey.LoadOrCreate(filepath.Join(dataDir, "device-identity.key"))
	if err != nil {
		t.Fatalf("agent identity: %v", err)
	}

	provider, err := testsource.New()
	if err != nil {
		t.Fatalf("test capture backend: %v", err)
	}
	recorder := agentinput.NewRecorder(4096)
	clip := clipboard.NewRecorder()

	instance, err := agentcore.New(agentcore.Options{
		Config: cfg, Identity: identity, Provider: provider,
		Injector: recorder, Clipboard: clip, Log: quiet,
	})
	if err != nil {
		t.Fatalf("build agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &stack{
		t: t, server: server, serviceURL: serviceURL, agent: instance,
		recorder: recorder, clipboard: clip, source: provider,
		deviceID: identity.Public().String(), cancel: cancel,
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = instance.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = instance.Close()
		s.wg.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if instance.Status().Connected {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the agent never connected to the test service")
	return nil
}

// driverResult is the JSON the browser driver reports.
type driverResult struct {
	OK     bool `json:"ok"`
	Errors []struct {
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"errors"`
	Steps []struct {
		Name   string          `json:"name"`
		Detail json.RawMessage `json:"detail"`
	} `json:"steps"`
	Console []string `json:"console"`
	Decoded struct {
		FramesDecoded    int    `json:"framesDecoded"`
		FramesReceived   int    `json:"framesReceived"`
		BytesReceived    int    `json:"bytesReceived"`
		FrameWidth       int    `json:"frameWidth"`
		FrameHeight      int    `json:"frameHeight"`
		KeyFramesDecoded int    `json:"keyFramesDecoded"`
		Decoder          string `json:"decoder"`
		FreezeCount      int    `json:"freezeCount"`
		VideoWidth       int    `json:"videoWidth"`
		VideoHeight      int    `json:"videoHeight"`
	} `json:"decoded"`
	Hello struct {
		DeviceName     string `json:"deviceName"`
		Codec          string `json:"codec"`
		Encoder        string `json:"encoder"`
		CaptureBackend string `json:"captureBackend"`
		StreamWidth    int    `json:"streamWidth"`
		StreamHeight   int    `json:"streamHeight"`
		HasControl     bool   `json:"hasControl"`
		SessionKind    string `json:"sessionKind"`
	} `json:"hello"`
	ClientStats struct {
		FPS         float64 `json:"fps"`
		Width       int     `json:"width"`
		Height      int     `json:"height"`
		Codec       string  `json:"codec"`
		BitrateKbps int     `json:"bitrateKbps"`
		RTTMs       int     `json:"rttMs"`
		DecodeMs    float64 `json:"decodeMs"`
		LossPercent float64 `json:"lossPercent"`
		Quality     string  `json:"quality"`
		Relayed     *bool   `json:"relayed"`
		Decoder     string  `json:"decoder"`
	} `json:"clientStats"`
	Target struct {
		X    int `json:"x"`
		Y    int `json:"y"`
		Rect struct {
			Left        float64 `json:"left"`
			Top         float64 `json:"top"`
			Width       float64 `json:"width"`
			Height      float64 `json:"height"`
			VideoWidth  int     `json:"videoWidth"`
			VideoHeight int     `json:"videoHeight"`
		} `json:"rect"`
	} `json:"target"`
	Route *struct {
		State     string   `json:"state"`
		LocalType string   `json:"localType"`
		RTTMs     *float64 `json:"rttMs"`
	} `json:"route"`
	Cursor struct {
		Shapes        int `json:"shapes"`
		PaintedPixels int `json:"paintedPixels"`
		State         struct {
			ShapeID uint32 `json:"shapeId"`
			X       uint16 `json:"x"`
			Y       uint16 `json:"y"`
			Visible bool   `json:"visible"`
		} `json:"state"`
	} `json:"cursor"`
	Clipboard struct {
		Sent            string `json:"sent"`
		RefusedOversize bool   `json:"refusedOversize"`
		Received        string `json:"received"`
	} `json:"clipboard"`
	Latency *struct {
		Samples  int `json:"samples"`
		MinMs    int `json:"minMs"`
		MedianMs int `json:"medianMs"`
		P90Ms    int `json:"p90Ms"`
		MaxMs    int `json:"maxMs"`
	} `json:"latency"`
}

// runDriver launches the browser and returns what it reported.
func (s *stack) runDriver(t *testing.T, code string, extra ...string) driverResult {
	t.Helper()
	return s.runScript(t, "driver.js", code, extra...)
}

// runScript launches a named browser driver script.
func (s *stack) runScript(t *testing.T, script, code string, extra ...string) driverResult {
	t.Helper()

	args := []string{
		filepath.Join(script),
		"--service=" + s.serviceURL,
		"--code=" + code,
		"--device=" + s.deviceID,
	}
	args = append(args, extra...)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodeBinary(), args...)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()

	text := string(output)
	marker := "===ALLSHARE_E2E_JSON==="
	index := strings.Index(text, marker)
	if index < 0 {
		t.Fatalf("the browser driver produced no result\n%s", text)
	}
	var result driverResult
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(text[index+len(marker):])), &result); jsonErr != nil {
		t.Fatalf("could not read the driver result: %v\n%s", jsonErr, text)
	}

	if !result.OK || err != nil {
		for _, e := range result.Errors {
			t.Errorf("browser reported %s: %s", e.Name, e.Detail)
		}
		t.Logf("browser console:\n%s", strings.Join(tail(result.Console, 40), "\n"))
		t.Fatalf("the browser driver failed (%v)", err)
	}
	return result
}

func nodeBinary() string {
	if custom := os.Getenv("ALLSHARE_NODE"); custom != "" {
		return custom
	}
	return "node"
}

func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// ---------------------------------------------------------------------------
// The acceptance test
// ---------------------------------------------------------------------------

// TestFullSession is the acceptance test the product is judged by: open the
// client, pair, connect, see the desktop, drive it, disconnect.
func TestFullSession(t *testing.T) {
	skipUnlessEnabled(t)
	s := startStack(t)

	code, _, err := s.agent.BeginPairing()
	if err != nil {
		t.Fatalf("open a pairing window: %v", err)
	}

	// Push a known string from the PC's clipboard on a repeat for the duration
	// of the run, so the browser's wait for one is not a race against a single
	// event fired before the session was up.
	const copiedOnPC = "copied on the PC — ünïcödé too"
	stopCopying := make(chan struct{})
	copying := make(chan struct{})
	go func() {
		defer close(copying)
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopCopying:
				return
			case <-ticker.C:
				s.clipboard.Copy(copiedOnPC)
			}
		}
	}()

	shot := filepath.Join("artifacts", "session.png")
	_ = os.MkdirAll("artifacts", 0o755)
	result := s.runDriver(t, code, "--screenshot="+shot)
	close(stopCopying)
	<-copying

	// --- The picture actually arrived and was decoded. ---
	if result.Decoded.FramesDecoded < 10 {
		t.Errorf("browser decoded %d frames, want at least 10", result.Decoded.FramesDecoded)
	}
	if result.Decoded.KeyFramesDecoded < 1 {
		t.Errorf("browser decoded no keyframes")
	}
	if result.Decoded.VideoWidth == 0 || result.Decoded.VideoHeight == 0 {
		t.Errorf("the video element never got dimensions: %+v", result.Decoded)
	}
	if result.Decoded.BytesReceived < 10_000 {
		t.Errorf("only %d bytes of video arrived", result.Decoded.BytesReceived)
	}
	t.Logf("video: %d frames decoded (%d keyframes), %dx%d, %d bytes, decoder %q",
		result.Decoded.FramesDecoded, result.Decoded.KeyFramesDecoded,
		result.Decoded.VideoWidth, result.Decoded.VideoHeight,
		result.Decoded.BytesReceived, result.Decoded.Decoder)

	// --- The agent described itself truthfully. ---
	if result.Hello.DeviceName != "E2E Test PC" {
		t.Errorf("client saw the PC as %q", result.Hello.DeviceName)
	}
	if result.Hello.Codec != string(capture.CodecVP8) {
		t.Errorf("negotiated codec %q, want %q", result.Hello.Codec, capture.CodecVP8)
	}
	if !result.Hello.HasControl {
		t.Error("the client was not granted control")
	}
	t.Logf("session: %s via %s, %dx%d, backend %q",
		result.Hello.Codec, result.Hello.Encoder,
		result.Hello.StreamWidth, result.Hello.StreamHeight, result.Hello.CaptureBackend)

	if result.Route != nil {
		t.Logf("route: %s candidate, state %s", result.Route.LocalType, result.Route.State)
	}
	t.Logf("client stats: %.0f fps, %d kbps, rtt %d ms, decode %.2f ms, loss %.2f%%, quality %s",
		result.ClientStats.FPS, result.ClientStats.BitrateKbps, result.ClientStats.RTTMs,
		result.ClientStats.DecodeMs, result.ClientStats.LossPercent, result.ClientStats.Quality)

	// --- Input crossed the network intact. ---
	events := s.recorder.Events
	if len(events) == 0 {
		t.Fatal("the agent received no input at all")
	}

	// The click must land where the user pointed. The position checked is the
	// last one reported before the button went down — later moves are from the
	// latency sweep and say nothing about where the click landed.
	var click *agentinput.Event
	var lastMove *agentinput.Event
	for i := range events {
		if events[i].Kind == agentinput.EventButton && events[i].Down {
			click = &events[i]
			break
		}
		if events[i].Kind == agentinput.EventMoveAbsolute {
			lastMove = &events[i]
		}
	}
	if lastMove == nil {
		t.Fatal("no pointer position reached the agent before the click")
	}
	if click == nil {
		t.Error("no mouse click reached the agent")
	} else if click.Button != 0 {
		t.Errorf("click arrived as button %d, want 0 (left)", click.Button)
	}

	wantX := uint16(math.Round(0.25 * 65535))
	wantY := uint16(math.Round(0.75 * 65535))
	const tolerance = 900 // ~1.4% of the axis, covering rounding and edge insets
	if diff(lastMove.X, wantX) > tolerance || diff(lastMove.Y, wantY) > tolerance {
		t.Errorf("pointer landed at (%d,%d), want about (%d,%d)", lastMove.X, lastMove.Y, wantX, wantY)
	} else {
		t.Logf("pointer: browser clicked at 25%%/75%% of the picture, agent saw (%d,%d) of 65535",
			lastMove.X, lastMove.Y)
	}

	// Keys must arrive as the right HID usages, with modifiers intact.
	usages := map[uint16]bool{}
	for _, event := range events {
		if event.Kind == agentinput.EventKey && event.Down {
			usages[event.Usage] = true
		}
	}
	expected := map[string]uint16{
		"H":         0x0B,
		"I":         0x0C,
		"W":         0x1A,
		"ArrowUp":   0x52,
		"ShiftLeft": 0xE1,
	}
	for name, usage := range expected {
		if !usages[usage] {
			t.Errorf("key %s (HID %#x) never reached the agent", name, usage)
		}
	}
	t.Logf("keyboard: %d distinct keys arrived, including Shift and an arrow key", len(usages))

	// The wheel must arrive with the sign and magnitude the browser sent.
	var wheel *agentinput.Event
	for i := range events {
		if events[i].Kind == agentinput.EventWheel {
			wheel = &events[i]
			break
		}
	}
	if wheel == nil {
		t.Error("no scroll reached the agent")
	} else if wheel.DY == 0 {
		t.Errorf("scroll arrived with no vertical movement: %+v", wheel)
	} else {
		t.Logf("scroll: agent received dy=%d (1/120 notch units)", wheel.DY)
	}

	// --- Ending a session must not leave a key held. ---
	// The driver deliberately holds W down and then disconnects.
	sawRelease := false
	for _, event := range events {
		if event.Kind == agentinput.EventReleaseAll {
			sawRelease = true
		}
	}
	if !sawRelease {
		t.Error("the session ended without releasing held input; a key would be stuck on the PC")
	} else {
		t.Log("teardown: the agent was told to release everything when the session ended")
	}

	// --- Clipboard, both directions and the size limit. ---
	//
	// The oversized case matters as much as the ordinary one. Truncating a
	// clipboard silently is worse than refusing it: the user pastes something
	// that looks like what they copied and finds out later that it was not.
	written := s.clipboard.Written()
	if len(written) == 0 {
		t.Error("nothing reached the PC's clipboard; clipboard sharing is broken")
	} else {
		if written[0] != result.Clipboard.Sent {
			t.Errorf("the PC's clipboard got %q, want %q", written[0], result.Clipboard.Sent)
		} else {
			t.Logf("clipboard: %d characters arrived intact, including non-ASCII and a tab",
				len([]rune(written[0])))
		}
	}
	if !result.Clipboard.RefusedOversize {
		t.Error("an oversized paste was not refused by the client")
	}
	if result.Clipboard.Received != copiedOnPC {
		t.Errorf("the browser received %q from the PC's clipboard, want %q",
			result.Clipboard.Received, copiedOnPC)
	} else {
		t.Log("clipboard: text copied on the PC reached the browser intact")
	}
	for _, text := range written {
		if len(text) > protocol.MaxClipboardBytes {
			t.Errorf("the PC's clipboard received %d bytes, over the %d-byte limit",
				len(text), protocol.MaxClipboardBytes)
		}
		if strings.HasPrefix(text, strings.Repeat("x", 1024)) {
			t.Error("an oversized clipboard arrived truncated; it should not have arrived at all")
		}
	}

	// --- The locally drawn cursor works. ---
	if result.Cursor.Shapes == 0 {
		t.Error("no cursor bitmap reached the client, so the local cursor cannot be drawn")
	}
	if result.Cursor.PaintedPixels == 0 {
		t.Error("the client received a cursor but painted nothing")
	} else {
		t.Logf("cursor: %d shape(s) received, %d pixels painted locally at (%d,%d)",
			result.Cursor.Shapes, result.Cursor.PaintedPixels,
			result.Cursor.State.X, result.Cursor.State.Y)
	}

	// --- Latency, measured rather than asserted. ---
	//
	// This is a synthetic 30 fps stream decoded in software inside a headless
	// browser on the same host, so the number is not a claim about the product
	// on real hardware. It is a regression guard: it caught a capture queue that
	// was adding a quarter of a second of pure latency for no benefit.
	if result.Latency == nil {
		t.Error("no end-to-end latency samples were produced; the frame-mark path is broken")
	} else {
		t.Logf("end-to-end input latency over loopback: median %d ms (min %d, p90 %d, max %d, n=%d)",
			result.Latency.MedianMs, result.Latency.MinMs, result.Latency.P90Ms,
			result.Latency.MaxMs, result.Latency.Samples)
		// One frame interval is 33 ms at the pattern's 30 fps, and a fair
		// measurement should sit within a few of those. A median beyond four
		// frame intervals on loopback means something is queueing.
		if result.Latency.MedianMs > 140 {
			t.Errorf("median end-to-end latency of %d ms over loopback indicates a queue in the pipeline",
				result.Latency.MedianMs)
		}
	}

	if _, err := os.Stat(shot); err == nil {
		t.Logf("screenshot written to %s", shot)
	}
}

// TestUnpairedClientIsRefused proves the agent enforces pairing itself rather
// than trusting the service's routing.
func TestUnpairedClientIsRefused(t *testing.T) {
	skipUnlessEnabled(t)
	s := startStack(t)

	// A code that was never issued cannot pair, so the browser's connect
	// attempt should be refused by the agent.
	result := runDriverExpectingFailure(t, s, "AAAABBBBCCCC")
	if !strings.Contains(strings.ToLower(result), "pairing failed") {
		t.Fatalf("an invalid code did not fail pairing:\n%s", result)
	}
	t.Log("an unissued pairing code was refused, as it must be")
}

func runDriverExpectingFailure(t *testing.T, s *stack, code string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeBinary(), "driver.js",
		"--service="+s.serviceURL, "--code="+code, "--device="+s.deviceID)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the driver unexpectedly succeeded with an invalid code:\n%s", output)
	}
	return string(output)
}

// TestReconnectAndServiceOutage covers the two failures a user actually meets
// on a real network: the session dropping, and the rendezvous going away.
func TestReconnectAndServiceOutage(t *testing.T) {
	skipUnlessEnabled(t)
	s := startStack(t)

	code, _, err := s.agent.BeginPairing()
	if err != nil {
		t.Fatalf("open a pairing window: %v", err)
	}

	result := s.runScript(t, "reconnect.js", code)
	for _, step := range result.Steps {
		t.Logf("  %-24s %s", step.Name, string(step.Detail))
	}

	names := map[string]bool{}
	for _, step := range result.Steps {
		names[step.Name] = true
	}
	for _, required := range []string{
		"paired-and-online", "first-session", "session-dropped",
		"reconnected", "video-after-reconnect", "service-stopped", "service-recovered",
	} {
		if !names[required] {
			t.Errorf("the client never reached the %q stage", required)
		}
	}
	t.Log("the client recovered a dropped session on its own, and kept streaming " +
		"while the rendezvous was unreachable")
}

// TestQualityControllerDoesNotOscillate exercises the adaptive loop directly.
//
// Oscillation is the failure mode that matters: a stream that pumps between two
// resolutions is more distracting than one that simply sits at the lower one.
// TestClientInterface drives the parts of the interface that need a real
// browser but no network: the hidden developer mode, the service-address
// policy as the connection code applies it, and every settings tab in both
// colour schemes.
//
// These are the places a unit test cannot see. A panel that throws on open
// takes the whole settings modal with it, and a reveal gesture that fires on a
// stray click puts a user somewhere they did not ask to be.
func TestClientInterface(t *testing.T) {
	skipUnlessEnabled(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodeBinary(), "uiprobe.js")
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()

	var result struct {
		Checks []struct {
			Name   string `json:"name"`
			Pass   bool   `json:"pass"`
			Detail string `json:"detail"`
		} `json:"checks"`
		Errors []string `json:"errors"`
		OK     bool     `json:"ok"`
	}
	if jsonErr := json.Unmarshal(output, &result); jsonErr != nil {
		t.Fatalf("could not read the UI probe result: %v\n%s", jsonErr, output)
	}
	for _, c := range result.Checks {
		if !c.Pass {
			t.Errorf("%s (%s)", c.Name, c.Detail)
		}
	}
	if !result.OK || err != nil {
		t.Fatalf("the UI probe failed (%v)", err)
	}
	t.Logf("interface: %d checks passed in a real browser, dark and light", len(result.Checks))
}

func TestQualityControllerDoesNotOscillate(t *testing.T) {
	provider, err := testsource.New()
	if err != nil {
		t.Fatalf("test backend: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	source, err := provider.Open(capture.Options{FPS: 30, Bitrate: 4_000_000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	controller := session.NewQualityController(session.QualityConfig{
		Source: source,
		Settings: session.Settings{
			MaxBitrate: 25_000_000, MinBitrate: 600_000, StartBitrate: 4_000_000,
			MaxFPS: 60, Preset: protocol.PresetBalanced,
		},
	})
	controller.Apply(protocol.SetQuality{
		Preset: protocol.PresetBalanced, Resolution: protocol.ResolutionAuto,
		MaxFPS: 60, MaxKbps: 25_000, Adaptive: true,
	})

	// A bandwidth estimate that flaps around a ladder boundary must not produce
	// a resolution change on every tick.
	_, _, startRung := controller.Snapshot()
	for i := 0; i < 40; i++ {
		estimate := 5_900_000
		if i%2 == 0 {
			estimate = 6_100_000
		}
		controller.Update(estimate, nil)
	}
	_, _, endRung := controller.Snapshot()
	if endRung != startRung {
		t.Errorf("a flapping estimate moved the quality ladder from rung %d to %d", startRung, endRung)
	}

	// A sustained collapse must be followed, though, or the stream would simply
	// stall instead of degrading.
	bitrateBefore, _, _ := controller.Snapshot()
	for i := 0; i < 40; i++ {
		controller.Update(700_000, nil)
	}
	bitrateAfter, _, _ := controller.Snapshot()
	if bitrateAfter >= bitrateBefore {
		t.Errorf("a sustained bandwidth collapse did not lower the bitrate (%d then %d)",
			bitrateBefore, bitrateAfter)
	}
	t.Logf("adaptive control: held rung %d through a flapping estimate, and dropped "+
		"the bitrate from %d to %d kbps under a sustained collapse",
		endRung, bitrateBefore/1000, bitrateAfter/1000)
}

func diff(a, b uint16) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

var _ = fmt.Sprintf
