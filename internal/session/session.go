// Package session runs one client's live remote-desktop connection.
//
// It owns the peer connection, the two data channels, the capture pipeline and
// the adaptive quality loop. One Session is created per connecting client and
// torn down completely when that client goes away, so a failed session can
// never leave the encoder running.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/mmc/all-share/internal/capture"
	agentinput "github.com/mmc/all-share/internal/input"
	"github.com/mmc/all-share/shared/protocol"
)

// Transport tuning.
const (
	// rtpMTU keeps a packet inside a 1280-byte IPv6 minimum MTU after DTLS and
	// SRTP overhead. Fragmenting at the IP layer would turn one lost fragment
	// into a lost frame.
	rtpMTU = 1150

	videoClockRate = 90000
	audioClockRate = 48000

	statsInterval    = 500 * time.Millisecond
	connectTimeout   = 30 * time.Second
	ctrlChannelName  = "ctrl"
	inputChannelName = "input"
)

// Config describes one session.
type Config struct {
	SessionID string
	ClientID  string
	Label     string

	Provider  capture.Provider
	Injector  agentinput.Injector
	Clipboard Clipboard

	// ClientCodecs is what the browser reported it can decode.
	ClientCodecs []protocol.CodecCapability
	ICEServers   []webrtc.ICEServer

	DeviceName   string
	AgentVersion string
	SessionKind  string

	// HasControl is false for a viewer, which receives video but cannot inject.
	HasControl bool

	// Send delivers a signalling payload to the client. The agent signs it.
	Send func(protocol.SignalPayload) error

	// OnClosed is called once when the session finishes, for any reason.
	OnClosed func(reason string)

	Log      *slog.Logger
	Settings Settings
}

// Settings are the agent-side defaults a session starts with.
type Settings struct {
	MaxBitrate   int
	MinBitrate   int
	StartBitrate int
	MaxFPS       int
	Preset       protocol.QualityPreset
	AudioEnabled bool
	ClipboardIn  bool
	ClipboardOut bool
	LocalCursor  bool
	IdleFPS      int
	RelayOnly    bool
}

// Clipboard is the host clipboard, if the platform provides one.
type Clipboard interface {
	Read() (string, error)
	Write(text string) error
	Changes() <-chan string
}

// Session is one live client connection.
type Session struct {
	cfg Config
	log *slog.Logger

	pc        *webrtc.PeerConnection
	video     *webrtc.TrackLocalStaticRTP
	audio     *webrtc.TrackLocalStaticRTP
	ctrl      *webrtc.DataChannel
	input     *webrtc.DataChannel
	estimator cc.BandwidthEstimator

	source  capture.Source
	quality *QualityController

	packetizer  rtp.Packetizer
	audioPacker rtp.Packetizer

	codec   capture.Codec
	profile string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once
	closed    atomic.Bool

	keyState     protocol.KeyBitmap
	buttonState  uint8
	lastInputSeq atomic.Uint32
	lastInputAt  atomic.Int64
	inputMu      sync.Mutex

	cursorShapes  map[uint32]bool
	pendingShapes map[uint32]capture.CursorUpdate
	cursorMu      sync.Mutex

	sentBytes  atomic.Uint64
	sentFrames atomic.Uint64

	helloSent atomic.Bool
	viewport  atomic.Pointer[protocol.Viewport]
}

// New builds a session and produces the offer the client will answer.
//
// The agent offers rather than answers so it controls the codec, the RTP header
// extensions and the data-channel reliability settings — all of which affect
// latency, and none of which should be left to whatever the browser proposes.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Send == nil {
		return nil, errors.New("allshare/session: no signalling sender")
	}

	codec, profile, ok := capture.SelectCodec(cfg.Provider.Capabilities(), cfg.ClientCodecs)
	if !ok {
		return nil, fmt.Errorf("allshare/session: this PC cannot produce any video format that device can play")
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	s := &Session{
		cfg:           cfg,
		log:           cfg.Log.With("session", cfg.SessionID),
		codec:         codec,
		profile:       profile,
		ctx:           sessionCtx,
		cancel:        cancel,
		cursorShapes:  map[uint32]bool{},
		pendingShapes: map[uint32]capture.CursorUpdate{},
	}

	if err := s.buildPeerConnection(); err != nil {
		cancel()
		return nil, err
	}
	if err := s.buildChannelsAndTracks(); err != nil {
		cancel()
		_ = s.pc.Close()
		return nil, err
	}

	// The capture pipeline is opened in parallel with ICE. Encoder start-up on
	// Windows costs a few hundred milliseconds, and doing it while candidates
	// are still being gathered removes that from time-to-first-frame.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.openSource()
	}()

	if err := s.createOffer(); err != nil {
		s.Close("could not create an offer")
		return nil, err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchConnect()
	}()

	return s, nil
}

func (s *Session) buildPeerConnection() error {
	engine := &webrtc.MediaEngine{}
	if err := s.registerVideoCodec(engine); err != nil {
		return err
	}
	if s.cfg.Settings.AudioEnabled {
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: audioClockRate, Channels: 2,
				SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1",
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return fmt.Errorf("allshare/session: register audio codec: %w", err)
		}
	}

	// Header extensions are chosen deliberately.
	//
	// abs-send-time gives the receiver a sender clock reference, and mid keeps
	// the bundled streams identifiable. transport-wide-cc is registered by
	// ConfigureTWCCHeaderExtensionSender below rather than here.
	//
	// The colour-space extension is *not* registered. Chromium falls back from
	// its hardware decoder to software when it is present, which is exactly the
	// wrong trade for a remote desktop.
	for _, uri := range []string{
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time",
		"urn:ietf:params:rtp-hdrext:sdes:mid",
	} {
		if err := engine.RegisterHeaderExtension(
			webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeVideo,
		); err != nil {
			s.log.Debug("header extension not registered", "uri", uri, "err", err)
		}
	}

	registry := &interceptor.Registry{}

	// Google Congestion Control. Without it the agent would be guessing at the
	// available bandwidth, which on a home upload link means either wasting
	// half of it or drowning it.
	settings := s.cfg.Settings
	ccFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(settings.StartBitrate),
			gcc.SendSideBWEMinBitrate(settings.MinBitrate),
			gcc.SendSideBWEMaxBitrate(settings.MaxBitrate),
		)
	})
	if err != nil {
		return fmt.Errorf("allshare/session: configure congestion control: %w", err)
	}
	estimatorReady := make(chan cc.BandwidthEstimator, 1)
	ccFactory.OnNewPeerConnection(func(_ string, estimator cc.BandwidthEstimator) {
		estimatorReady <- estimator
	})

	// Registration order is load-bearing and easy to get wrong.
	//
	// An interceptor registered later wraps the ones registered before it, so
	// the *last* one added is the first to see an outgoing packet. Congestion
	// control records a transport sequence number for every packet it paces and
	// reads that number back out of the header, so the extension writer has to
	// run before it — which means being registered after it. With the two the
	// other way round every packet is rejected with "missing transport layer cc
	// header extension" and no video is ever sent.
	registry.Add(ccFactory)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(engine, registry); err != nil {
		return fmt.Errorf("allshare/session: configure transport feedback: %w", err)
	}
	if err := webrtc.ConfigureNack(engine, registry); err != nil {
		return fmt.Errorf("allshare/session: configure retransmission: %w", err)
	}
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return fmt.Errorf("allshare/session: configure RTCP reports: %w", err)
	}

	config := webrtc.Configuration{ICEServers: s.cfg.ICEServers}
	if s.cfg.Settings.RelayOnly {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry))
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("allshare/session: create peer connection: %w", err)
	}
	s.pc = pc

	select {
	case s.estimator = <-estimatorReady:
	case <-time.After(2 * time.Second):
		s.log.Warn("congestion control did not start; falling back to a fixed bitrate")
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		payload := protocol.SignalPayload{
			Kind:      "candidate",
			Candidate: init.Candidate,
			Nonce:     newNonce(),
			Timestamp: time.Now().UnixMilli(),
		}
		if init.SDPMid != nil {
			payload.SDPMid = *init.SDPMid
		}
		if init.SDPMLineIndex != nil {
			payload.SDPMLine = init.SDPMLineIndex
		}
		if err := s.cfg.Send(payload); err != nil {
			s.log.Debug("could not send a candidate", "err", err)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.log.Info("peer connection", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			s.onConnected()
		case webrtc.PeerConnectionStateFailed:
			s.Close("the connection failed")
		case webrtc.PeerConnectionStateClosed:
			s.Close("the connection closed")
		case webrtc.PeerConnectionStateDisconnected:
			// Not fatal: ICE can recover on its own, and tearing the encoder
			// down here would make a brief Wi-Fi glitch cost a full restart.
			s.log.Info("connection interrupted, waiting for ICE to recover")
		}
	})

	return nil
}

// registerVideoCodec registers exactly the negotiated codec.
//
// Offering only what will actually be sent keeps the SDP small and removes any
// chance of the browser selecting a codec this machine cannot encode.
func (s *Session) registerVideoCodec(engine *webrtc.MediaEngine) error {
	var params webrtc.RTPCodecParameters
	switch s.codec {
	case capture.CodecH264:
		fmtp := "level-asymmetry-allowed=1;packetization-mode=1"
		if s.profile != "" {
			fmtp += ";profile-level-id=" + s.profile
		}
		params = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: videoClockRate,
				SDPFmtpLine:  fmtp,
				RTCPFeedback: videoFeedback(),
			},
			PayloadType: 102,
		}
	case capture.CodecH265:
		params = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH265, ClockRate: videoClockRate,
				RTCPFeedback: videoFeedback(),
			},
			PayloadType: 106,
		}
	case capture.CodecAV1:
		params = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeAV1, ClockRate: videoClockRate,
				SDPFmtpLine:  "level-idx=5;profile=0;tier=0",
				RTCPFeedback: videoFeedback(),
			},
			PayloadType: 45,
		}
	case capture.CodecVP9:
		params = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP9, ClockRate: videoClockRate,
				SDPFmtpLine: "profile-id=0", RTCPFeedback: videoFeedback(),
			},
			PayloadType: 98,
		}
	case capture.CodecVP8:
		params = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP8, ClockRate: videoClockRate,
				RTCPFeedback: videoFeedback(),
			},
			PayloadType: 96,
		}
	default:
		return fmt.Errorf("allshare/session: unsupported codec %q", s.codec)
	}
	if err := engine.RegisterCodec(params, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("allshare/session: register video codec: %w", err)
	}
	return nil
}

func videoFeedback() []webrtc.RTCPFeedback {
	return []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "ccm", Parameter: "fir"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	}
}

func (s *Session) buildChannelsAndTracks() error {
	ordered := true
	unordered := false
	noRetransmit := uint16(0)

	// The control channel is reliable and ordered: configuration, clipboard and
	// cursor bitmaps must all arrive, in order.
	ctrl, err := s.pc.CreateDataChannel(ctrlChannelName, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return fmt.Errorf("allshare/session: create control channel: %w", err)
	}
	s.ctrl = ctrl
	ctrl.OnOpen(func() { s.onCtrlOpen() })
	ctrl.OnMessage(func(msg webrtc.DataChannelMessage) { s.onCtrlMessage(msg.Data) })

	// The input channel is unordered and never retransmitted. A retransmitted
	// mouse position is stale by definition, and every input packet carries the
	// complete key and button state, so a lost packet is corrected by the next
	// one rather than needing to be re-sent.
	input, err := s.pc.CreateDataChannel(inputChannelName, &webrtc.DataChannelInit{
		Ordered:        &unordered,
		MaxRetransmits: &noRetransmit,
	})
	if err != nil {
		return fmt.Errorf("allshare/session: create input channel: %w", err)
	}
	s.input = input
	input.OnMessage(func(msg webrtc.DataChannelMessage) { s.onInputMessage(msg.Data) })

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: mimeFor(s.codec), ClockRate: videoClockRate},
		"video", "allshare")
	if err != nil {
		return fmt.Errorf("allshare/session: create video track: %w", err)
	}
	s.video = videoTrack
	sender, err := s.pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("allshare/session: add video track: %w", err)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.readRTCP(sender)
	}()

	if s.cfg.Settings.AudioEnabled {
		audioTrack, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: audioClockRate, Channels: 2},
			"audio", "allshare")
		if err != nil {
			return fmt.Errorf("allshare/session: create audio track: %w", err)
		}
		s.audio = audioTrack
		if _, err := s.pc.AddTrack(audioTrack); err != nil {
			return fmt.Errorf("allshare/session: add audio track: %w", err)
		}
		s.audioPacker = rtp.NewPacketizer(rtpMTU, 111, rand.Uint32(),
			&codecs.OpusPayloader{}, rtp.NewRandomSequencer(), audioClockRate)
	}

	s.packetizer = rtp.NewPacketizer(rtpMTU, payloadTypeFor(s.codec), rand.Uint32(),
		payloaderFor(s.codec), rtp.NewRandomSequencer(), videoClockRate)
	return nil
}

func mimeFor(codec capture.Codec) string {
	switch codec {
	case capture.CodecH264:
		return webrtc.MimeTypeH264
	case capture.CodecH265:
		return webrtc.MimeTypeH265
	case capture.CodecAV1:
		return webrtc.MimeTypeAV1
	case capture.CodecVP9:
		return webrtc.MimeTypeVP9
	default:
		return webrtc.MimeTypeVP8
	}
}

func payloadTypeFor(codec capture.Codec) uint8 {
	switch codec {
	case capture.CodecH264:
		return 102
	case capture.CodecH265:
		return 106
	case capture.CodecAV1:
		return 45
	case capture.CodecVP9:
		return 98
	default:
		return 96
	}
}

func payloaderFor(codec capture.Codec) rtp.Payloader {
	switch codec {
	case capture.CodecH264:
		return &codecs.H264Payloader{}
	case capture.CodecH265:
		return &codecs.H265Payloader{}
	case capture.CodecAV1:
		return &codecs.AV1Payloader{}
	case capture.CodecVP9:
		return &codecs.VP9Payloader{}
	default:
		return &codecs.VP8Payloader{EnablePictureID: true}
	}
}

func (s *Session) createOffer() error {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("allshare/session: create offer: %w", err)
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("allshare/session: set local description: %w", err)
	}
	return s.cfg.Send(protocol.SignalPayload{
		Kind:      "offer",
		SDP:       offer.SDP,
		Nonce:     newNonce(),
		Timestamp: time.Now().UnixMilli(),
	})
}

// HandleSignal applies a verified signalling payload from the client.
// The caller has already checked its signature against the paired identity.
func (s *Session) HandleSignal(payload protocol.SignalPayload) error {
	switch payload.Kind {
	case "answer":
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer, SDP: payload.SDP,
		}); err != nil {
			return fmt.Errorf("allshare/session: apply answer: %w", err)
		}
		return nil
	case "candidate":
		if payload.Candidate == "" {
			return nil
		}
		init := webrtc.ICECandidateInit{Candidate: payload.Candidate}
		if payload.SDPMid != "" {
			mid := payload.SDPMid
			init.SDPMid = &mid
		}
		if payload.SDPMLine != nil {
			index := *payload.SDPMLine
			init.SDPMLineIndex = &index
		}
		if err := s.pc.AddICECandidate(init); err != nil {
			// A candidate that arrives before the answer, or one for a closed
			// transport, is normal during negotiation and not worth failing on.
			s.log.Debug("candidate not applied", "err", err)
		}
		return nil
	case "bye":
		s.Close(payload.Reason)
		return nil
	default:
		return fmt.Errorf("allshare/session: unknown signalling kind %q", payload.Kind)
	}
}

func (s *Session) watchConnect() {
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
	case <-timer.C:
		if s.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			s.log.Warn("the client never completed the connection")
			s.Close("the client did not connect in time")
		}
	}
}

func (s *Session) openSource() {
	settings := s.cfg.Settings
	monitors := s.cfg.Provider.Monitors()
	width, height := 0, 0
	if len(monitors) > 0 {
		width, height = monitors[0].Width, monitors[0].Height
	}
	source, err := s.cfg.Provider.Open(capture.Options{
		Codec:         s.codec,
		Profile:       s.profile,
		Width:         width,
		Height:        height,
		FPS:           settings.MaxFPS,
		Bitrate:       settings.StartBitrate,
		MonitorID:     0,
		ExcludeCursor: settings.LocalCursor,
		AudioEnabled:  settings.AudioEnabled,
		Preset:        settings.Preset,
	})
	if err != nil {
		s.log.Error("could not start screen capture", "err", err)
		s.sendNotice(protocol.NoticeError, "capture_failed",
			"ALL SHARE could not capture this PC's screen.", err.Error())
		s.Close("screen capture failed")
		return
	}
	s.source = source

	// Tell the injector which display the client is looking at, so a click is
	// mapped onto the right monitor rather than the primary one.
	if aware, ok := s.cfg.Injector.(agentinput.RectAware); ok {
		info := source.Info()
		for _, monitor := range info.Monitors {
			if monitor.ID != info.ActiveMonitor {
				continue
			}
			aware.SetCaptureRect(monitor.X, monitor.Y, monitor.Width, monitor.Height)
			break
		}
	}

	s.quality = NewQualityController(QualityConfig{
		Source:   source,
		Settings: settings,
		Log:      s.log,
	})

	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.pumpVideo() }()
	go func() { defer s.wg.Done(); s.pumpAudio() }()
	go func() { defer s.wg.Done(); s.pumpCursor() }()
	s.log.Info("capture started",
		"backend", source.Info().Backend, "codec", source.Info().Codec,
		"encoder", source.Info().Encoder, "hardware", source.Info().Hardware,
		"size", fmt.Sprintf("%dx%d", source.Info().Width, source.Info().Height))

	s.maybeSendHello()
}

func (s *Session) onConnected() {
	s.maybeSendHello()
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.pumpStats() }()
	go func() { defer s.wg.Done(); s.pumpQuality() }()
	if s.cfg.Settings.ClipboardOut && s.cfg.Clipboard != nil {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.pumpClipboard() }()
	}
}

func (s *Session) onCtrlOpen() {
	s.log.Info("control channel open")
	s.maybeSendHello()
}

// maybeSendHello sends the opening description once both the capture pipeline
// and the control channel are ready, whichever finishes last.
func (s *Session) maybeSendHello() {
	if s.source == nil || s.ctrl == nil || s.ctrl.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	if !s.helloSent.CompareAndSwap(false, true) {
		return
	}
	info := s.source.Info()
	hello := protocol.Hello{
		VersionMajor:    protocol.VersionMajor,
		VersionMinor:    protocol.VersionMinor,
		AgentVersion:    s.cfg.AgentVersion,
		DeviceName:      s.cfg.DeviceName,
		Monitors:        info.Monitors,
		ActiveMonitor:   info.ActiveMonitor,
		StreamWidth:     info.Width,
		StreamHeight:    info.Height,
		Codec:           string(info.Codec),
		CodecProfile:    s.profile,
		Encoder:         info.Encoder,
		CaptureBackend:  info.Backend,
		HardwareEncoded: info.Hardware,
		HasAudio:        info.HasAudio && s.cfg.Settings.AudioEnabled,
		HasClipboard:    s.cfg.Clipboard != nil,
		CanSetMonitor:   len(info.Monitors) > 1,
		CursorEmbedded:  info.CursorEmbedded,
		HasControl:      s.cfg.HasControl,
		SessionKind:     info.SessionKind,
	}
	s.sendCtrlJSON(protocol.TypeHello, hello)
	s.log.Info("session ready", "client", shortID(s.cfg.ClientID), "codec", info.Codec)
}

// ---------------------------------------------------------------------------
// Media pumps
// ---------------------------------------------------------------------------

func (s *Session) pumpVideo() {
	frames := s.source.Frames()
	var lastCapture time.Time

	for {
		select {
		case <-s.ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok {
				s.Close("screen capture stopped")
				return
			}

			// RTP timestamps advance by the real elapsed time between captures,
			// not by a nominal frame interval. That keeps playback correctly
			// paced when the source skips frames because nothing changed —
			// which, on a desktop, is most of the time.
			samples := uint32(videoClockRate / 60)
			if !lastCapture.IsZero() {
				delta := frame.CapturedAt.Sub(lastCapture)
				if delta > 0 && delta < time.Second {
					samples = uint32(delta.Seconds() * videoClockRate)
				}
			}
			lastCapture = frame.CapturedAt
			if samples == 0 {
				samples = 1
			}

			packets := s.packetizer.Packetize(frame.Data, samples)
			if len(packets) == 0 {
				continue
			}
			rtpTimestamp := packets[0].Header.Timestamp
			for _, pkt := range packets {
				if err := s.video.WriteRTP(pkt); err != nil {
					if s.closed.Load() || errors.Is(err, io.ErrClosedPipe) {
						return
					}
					s.log.Debug("could not send a video packet", "err", err)
				}
			}
			s.sentFrames.Add(1)
			s.sentBytes.Add(uint64(len(frame.Data)))

			// Tell the client which input this frame reflects. It matches the
			// timestamp against the frame the browser actually displays, which
			// is what makes a real "key press to pixels" measurement possible.
			s.sendCtrlBytes(protocol.FrameMark{
				RTPTimestamp: rtpTimestamp,
				InputSeq:     frame.InputSeq,
				CaptureMicro: uint64(frame.CapturedAt.UnixMicro()),
				EncodeMicro:  uint32(frame.EncodeDuration.Microseconds()),
				SizeBytes:    uint32(len(frame.Data)),
				Keyframe:     frame.Keyframe,
			}.Encode(nil))
		}
	}
}

func (s *Session) pumpAudio() {
	audio := s.source.Audio()
	if audio == nil || s.audio == nil {
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame, ok := <-audio:
			if !ok {
				return
			}
			samples := uint32(frame.Duration.Seconds() * audioClockRate)
			if samples == 0 {
				samples = audioClockRate / 50 // a 20 ms Opus frame
			}
			for _, pkt := range s.audioPacker.Packetize(frame.Data, samples) {
				if err := s.audio.WriteRTP(pkt); err != nil {
					if s.closed.Load() {
						return
					}
					s.log.Debug("could not send an audio packet", "err", err)
				}
			}
		}
	}
}

func (s *Session) pumpCursor() {
	cursor := s.source.Cursor()
	if cursor == nil {
		return
	}
	var seq uint32
	for {
		select {
		case <-s.ctx.Done():
			return
		case update, ok := <-cursor:
			if !ok {
				return
			}
			// A bitmap is sent once per shape; after that only the position
			// moves, which is a handful of bytes at frame rate.
			if len(update.Shape) > 0 {
				s.rememberShape(update)
			}
			// Re-send any shape the client has not acknowledged receiving yet.
			// The first shape usually arrives before the control channel is
			// open, and without this retry the client would hold a shape id it
			// has no bitmap for and draw nothing at all.
			s.resendPendingShapes()

			seq++
			s.sendCtrlBytes(protocol.CursorState{
				Seq: seq, ShapeID: update.ShapeID,
				X: update.X, Y: update.Y,
				Visible: update.Visible, Relative: update.Relative,
			}.Encode(nil))
		}
	}
}

// rememberShape stores a cursor bitmap so it can be sent, and re-sent if the
// control channel was not ready the first time.
func (s *Session) rememberShape(update capture.CursorUpdate) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	if s.cursorShapes[update.ShapeID] {
		return
	}
	if len(s.pendingShapes) > 32 {
		// A pathological cursor storm should not grow this without bound.
		s.pendingShapes = map[uint32]capture.CursorUpdate{}
	}
	s.pendingShapes[update.ShapeID] = update
}

func (s *Session) resendPendingShapes() {
	s.cursorMu.Lock()
	pending := make([]capture.CursorUpdate, 0, len(s.pendingShapes))
	for _, update := range s.pendingShapes {
		pending = append(pending, update)
	}
	s.cursorMu.Unlock()
	if len(pending) == 0 {
		return
	}

	for _, update := range pending {
		msg, err := protocol.EncodeCursorShape(protocol.CursorShapeMeta{
			ShapeID: update.ShapeID, Width: update.Width, Height: update.Height,
			HotX: update.HotX, HotY: update.HotY, Format: "png",
		}, update.Shape)
		if err != nil {
			s.cursorMu.Lock()
			delete(s.pendingShapes, update.ShapeID)
			s.cursorMu.Unlock()
			continue
		}
		if !s.sendCtrlBytes(msg) {
			continue // the channel is not ready; try again on the next update
		}
		s.cursorMu.Lock()
		delete(s.pendingShapes, update.ShapeID)
		s.cursorShapes[update.ShapeID] = true
		if len(s.cursorShapes) > 128 {
			s.cursorShapes = map[uint32]bool{update.ShapeID: true}
		}
		s.cursorMu.Unlock()
	}
}

func (s *Session) pumpClipboard() {
	changes := s.cfg.Clipboard.Changes()
	if changes == nil {
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case text, ok := <-changes:
			if !ok {
				return
			}
			// Refuse rather than truncate. A silently shortened clipboard is
			// worse than none at all: the user pastes something that looks
			// like what they copied and only discovers the missing half
			// later, somewhere it matters.
			if len(text) > protocol.MaxClipboardBytes {
				s.sendNotice(protocol.NoticeWarning, "clipboard_too_large",
					"That copied text was too large to share, so it was left on the PC.",
					fmt.Sprintf("%d bytes, limit %d", len(text), protocol.MaxClipboardBytes))
				continue
			}
			s.sendCtrlJSON(protocol.TypeClipboardOut, protocol.Clipboard{Text: text})
		}
	}
}

func (s *Session) pumpStats() {
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	var lastBytes uint64
	last := time.Now()

	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			if s.source == nil {
				continue
			}
			stats := s.source.Stats()
			elapsed := now.Sub(last).Seconds()
			last = now
			bytes := s.sentBytes.Load()
			kbps := float32(0)
			if elapsed > 0 {
				kbps = float32(float64(bytes-lastBytes) * 8 / elapsed / 1000)
			}
			lastBytes = bytes

			target := 0
			if s.estimator != nil {
				target = s.estimator.GetTargetBitrate()
			}
			s.sendCtrlJSON(protocol.TypeStats, protocol.Stats{
				TSMilli:          uint32(now.UnixMilli()),
				CaptureFPS:       stats.CaptureFPS,
				EncodeFPS:        stats.EncodeFPS,
				SentKbps:         kbps,
				TargetKbps:       float32(target) / 1000,
				EncoderQP:        stats.QP,
				Width:            stats.Width,
				Height:           stats.Height,
				IdleSkipped:      stats.IdleSkipped,
				CaptureMs:        stats.CaptureMillis,
				EncodeMs:         stats.EncodeMillis,
				QueueMs:          stats.QueueMillis,
				LastInputSeq:     s.lastInputSeq.Load(),
				LastInputTSMilli: uint32(s.lastInputAt.Load()),
			})
		}
	}
}

func (s *Session) pumpQuality() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.quality == nil {
				continue
			}
			target := 0
			if s.estimator != nil {
				target = s.estimator.GetTargetBitrate()
			}
			s.quality.Update(target, s.viewport.Load())
		}
	}
}

// readRTCP drains the sender's RTCP stream. Reading is mandatory — the
// interceptors that implement retransmission and congestion control only see
// feedback if someone pulls it — and picture-loss reports are turned into
// keyframe requests here.
func (s *Session) readRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if s.source != nil {
					s.source.RequestKeyframe()
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Control channel
// ---------------------------------------------------------------------------

func (s *Session) sendCtrlJSON(msgType uint8, value any) {
	data, err := protocol.EncodeJSON(msgType, value)
	if err != nil {
		s.log.Error("could not encode a control message", "type", msgType, "err", err)
		return
	}
	s.sendCtrlBytes(data)
}

// sendCtrlBytes sends on the control channel, reporting whether it went out.
//
// The return value matters for the cursor: a shape bitmap is sent once and then
// referenced by id forever after, so recording it as delivered when the channel
// was not open would leave the client with a cursor it can never draw.
func (s *Session) sendCtrlBytes(data []byte) bool {
	if s.ctrl == nil || s.ctrl.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	// A backlog on the control channel means the client is not keeping up.
	// Dropping is better than growing an unbounded queue of stale cursor
	// positions and stats.
	if s.ctrl.BufferedAmount() > 512*1024 {
		return false
	}
	if err := s.ctrl.Send(data); err != nil {
		s.log.Debug("could not send a control message", "err", err)
		return false
	}
	return true
}

func (s *Session) sendNotice(severity protocol.NoticeSeverity, code, message, detail string) {
	s.sendCtrlJSON(protocol.TypeNotice, protocol.Notice{
		Severity: severity, Code: code, Message: message, Detail: detail,
	})
}

func (s *Session) onCtrlMessage(data []byte) {
	if len(data) < 1 {
		return
	}
	msgType := data[0]
	body := data[1:]

	switch msgType {
	case protocol.TypeSetQuality:
		var q protocol.SetQuality
		if err := protocol.DecodeJSON(body, &q); err != nil {
			return
		}
		if s.quality != nil {
			s.quality.Apply(q)
		}
		s.log.Debug("quality updated", "preset", q.Preset, "resolution", q.Resolution, "maxFps", q.MaxFPS)

	case protocol.TypeViewport:
		var v protocol.Viewport
		if err := protocol.DecodeJSON(body, &v); err != nil {
			return
		}
		if v.Width > 0 && v.Height > 0 && v.Width < 16384 && v.Height < 16384 {
			s.viewport.Store(&v)
		}

	case protocol.TypeSelectMonitor:
		var m protocol.SelectMonitor
		if err := protocol.DecodeJSON(body, &m); err != nil {
			return
		}
		if s.source == nil {
			return
		}
		if err := s.source.SetMonitor(m.MonitorID); err != nil {
			s.sendNotice(protocol.NoticeWarning, "monitor_switch_failed",
				"That display could not be shown.", err.Error())
			return
		}
		info := s.source.Info()
		if aware, ok := s.cfg.Injector.(agentinput.RectAware); ok {
			for _, monitor := range info.Monitors {
				if monitor.ID != info.ActiveMonitor {
					continue
				}
				aware.SetCaptureRect(monitor.X, monitor.Y, monitor.Width, monitor.Height)
				break
			}
		}
		s.sendCtrlJSON(protocol.TypeDisplayChanged, protocol.DisplayChanged{
			Monitors: info.Monitors, ActiveMonitor: info.ActiveMonitor,
			StreamWidth: info.Width, StreamHeight: info.Height, SessionKind: info.SessionKind,
		})

	case protocol.TypeRequestKeyframe:
		if s.source != nil {
			s.source.RequestKeyframe()
		}

	case protocol.TypeClipboardIn:
		s.handleClipboardIn(body)

	case protocol.TypeSetCursorMode:
		var mode protocol.SetCursorMode
		if err := protocol.DecodeJSON(body, &mode); err != nil {
			return
		}
		s.log.Debug("cursor mode", "local", mode.LocalCursor)

	case protocol.TypeSetPointerMode:
		var mode protocol.SetPointerMode
		if err := protocol.DecodeJSON(body, &mode); err != nil {
			return
		}
		if s.cfg.Injector != nil {
			s.cfg.Injector.SetPointerMode(mode.Mode == "relative")
		}

	case protocol.TypeSystemAction:
		s.handleSystemAction(body)

	case protocol.TypeDisconnect:
		s.Close("the client disconnected")

	case protocol.TypeCtrlPing:
		// Answered on the control channel so a ping still works when the
		// unreliable input channel is congested.
		var ping struct {
			Seq   uint32 `json:"seq"`
			TSMic uint64 `json:"tsMicro"`
		}
		if err := protocol.DecodeJSON(body, &ping); err == nil {
			s.sendCtrlBytes(protocol.Pong{
				Seq: ping.Seq, ClientTSMic: ping.TSMic,
				AgentTSMic: uint64(time.Now().UnixMicro()),
			}.Encode(nil))
		}

	default:
		s.log.Debug("ignoring control message", "type", fmt.Sprintf("0x%02x", msgType))
	}
}

func (s *Session) handleClipboardIn(body []byte) {
	if !s.cfg.Settings.ClipboardIn || s.cfg.Clipboard == nil || !s.cfg.HasControl {
		return
	}
	var clip protocol.Clipboard
	if err := protocol.DecodeJSON(body, &clip); err != nil {
		return
	}
	// Same reasoning as the outgoing direction: half a clipboard is a trap.
	if len(clip.Text) > protocol.MaxClipboardBytes {
		s.sendNotice(protocol.NoticeWarning, "clipboard_too_large",
			"That text was too large to send to the PC, so the PC's clipboard is unchanged.",
			fmt.Sprintf("%d bytes, limit %d", len(clip.Text), protocol.MaxClipboardBytes))
		return
	}
	if err := s.cfg.Clipboard.Write(clip.Text); err != nil {
		s.log.Debug("could not write to the clipboard", "err", err)
	}
}

func (s *Session) handleSystemAction(body []byte) {
	if !s.cfg.HasControl || s.cfg.Injector == nil {
		return
	}
	var action protocol.SystemAction
	if err := protocol.DecodeJSON(body, &action); err != nil {
		return
	}
	if err := s.cfg.Injector.SystemAction(action.Action); err != nil {
		s.sendNotice(protocol.NoticeWarning, "system_action_failed",
			"That could not be done on this PC.", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Input channel
// ---------------------------------------------------------------------------

func (s *Session) onInputMessage(data []byte) {
	if len(data) < 1 {
		return
	}
	// A viewer receives video but may not drive the machine. Enforced here
	// rather than trusted to the client, which could simply not ask.
	if !s.cfg.HasControl || s.cfg.Injector == nil {
		return
	}
	msgType := data[0]
	body := data[1:]

	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	switch msgType {
	case protocol.TypeMouseMoveAbs:
		msg, err := protocol.DecodeMouseMoveAbs(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		s.applyButtons(msg.Buttons)
		s.cfg.Injector.MoveAbsolute(msg.X, msg.Y)

	case protocol.TypeMouseMoveRel:
		msg, err := protocol.DecodeMouseMoveRel(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		s.applyButtons(msg.Buttons)
		s.cfg.Injector.MoveRelative(int(msg.DX), int(msg.DY))

	case protocol.TypeMouseButton:
		msg, err := protocol.DecodeMouseButton(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		s.cfg.Injector.MouseButton(msg.Button, msg.Down)
		s.buttonState = msg.Buttons

	case protocol.TypeMouseWheel:
		msg, err := protocol.DecodeMouseWheel(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		s.cfg.Injector.Wheel(int(msg.DX), int(msg.DY))

	case protocol.TypeKey:
		msg, err := protocol.DecodeKey(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		// The packet's own state snapshot is authoritative. Applying the diff
		// rather than just the one transition is what makes the unreliable
		// input channel self-healing: a dropped key event is corrected here.
		s.applyKeyState(&msg.State)

	case protocol.TypeKeyStateSync:
		msg, err := protocol.DecodeKeyStateSync(body)
		if err != nil {
			return
		}
		s.noteInput(msg.Seq, msg.TSMilli)
		s.applyKeyState(&msg.State)
		s.applyButtons(msg.Buttons)

	case protocol.TypeTextInput:
		msg, err := protocol.DecodeTextInput(body, protocol.MaxTextInputBytes)
		if err != nil {
			return
		}
		s.cfg.Injector.TypeText(msg.Text)

	case protocol.TypeInputPing:
		msg, err := protocol.DecodeInputPing(body)
		if err != nil {
			return
		}
		pong := protocol.Pong{
			Seq: msg.Seq, ClientTSMic: msg.TSMic,
			AgentTSMic: uint64(time.Now().UnixMicro()),
		}.Encode(nil)
		if s.input != nil && s.input.ReadyState() == webrtc.DataChannelStateOpen {
			_ = s.input.Send(pong)
		}
	}
}

func (s *Session) noteInput(seq, tsMilli uint32) {
	// Sequence numbers wrap; only move forward on a plausible advance so a
	// reordered packet on the unordered channel cannot rewind the counter.
	previous := s.lastInputSeq.Load()
	if seq > previous || previous-seq > 1<<31 {
		s.lastInputSeq.Store(seq)
		s.lastInputAt.Store(int64(tsMilli))
		if s.source != nil {
			s.source.NoteInput(seq)
		}
	}
}

func (s *Session) applyKeyState(next *protocol.KeyBitmap) {
	s.keyState.Diff(next, func(usage uint16, down bool) {
		s.cfg.Injector.Key(usage, down)
	})
	s.keyState = *next
}

func (s *Session) applyButtons(mask uint8) {
	if mask == s.buttonState {
		return
	}
	for index := uint8(0); index < 5; index++ {
		bit := uint8(1) << index
		was := s.buttonState&bit != 0
		now := mask&bit != 0
		if was != now {
			s.cfg.Injector.MouseButton(index, now)
		}
	}
	s.buttonState = mask
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

// Close ends the session and releases every resource it owns.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.log.Info("session ending", "reason", reason)
		s.cancel()

		// Release anything the client left held, so a client that vanishes
		// mid-keystroke cannot leave a key down on the machine.
		if s.cfg.Injector != nil && s.cfg.HasControl {
			s.cfg.Injector.ReleaseAll()
		}
		if s.source != nil {
			_ = s.source.Close()
		}
		for _, channel := range []*webrtc.DataChannel{s.ctrl, s.input} {
			if channel != nil {
				_ = channel.Close()
			}
		}
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.cfg.OnClosed != nil {
			s.cfg.OnClosed(reason)
		}
	})
}

// Wait blocks until every goroutine the session owns has finished.
func (s *Session) Wait() { s.wg.Wait() }

// SessionID identifies this session on the rendezvous.
func (s *Session) SessionID() string { return s.cfg.SessionID }

// ClientID is the paired client driving this session.
func (s *Session) ClientID() string { return s.cfg.ClientID }

// Codec reports the negotiated video codec.
func (s *Session) Codec() (capture.Codec, string) { return s.codec, s.profile }

// ConnectionState reports the peer connection state, for status reporting.
func (s *Session) ConnectionState() webrtc.PeerConnectionState {
	if s.pc == nil {
		return webrtc.PeerConnectionStateNew
	}
	return s.pc.ConnectionState()
}

// Stats returns a snapshot for the agent's own status display.
func (s *Session) Stats() (frames uint64, bytes uint64, targetBitrate int) {
	target := 0
	if s.estimator != nil {
		target = s.estimator.GetTargetBitrate()
	}
	return s.sentFrames.Load(), s.sentBytes.Load(), target
}

func newNonce() string {
	buf := make([]byte, 12)
	for i := range buf {
		buf[i] = byte(rand.Intn(256))
	}
	return fmt.Sprintf("%x", buf)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
