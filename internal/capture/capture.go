// Package capture defines the boundary between "getting an encoded frame off
// this machine's screen" and everything else in the agent.
//
// Keeping it an interface buys two things. Real backends can be selected at
// runtime from what the hardware actually supports, and a synthetic backend can
// stand in on any platform — which is what lets the end-to-end suite prove that
// a browser really decoded frames, rather than merely that a peer connection
// opened.
package capture

import (
	"errors"
	"time"

	"github.com/mmc/all-share/shared/protocol"
)

// Codec identifies a video codec by its RTP media type, matching the strings
// the browser reports from RTCRtpReceiver.getCapabilities.
type Codec string

// Supported video codecs.
const (
	CodecH264 Codec = "video/H264"
	CodecH265 Codec = "video/H265"
	CodecAV1  Codec = "video/AV1"
	CodecVP9  Codec = "video/VP9"
	CodecVP8  Codec = "video/VP8"
)

// ErrUnsupported reports that a backend cannot satisfy a request.
var ErrUnsupported = errors.New("allshare/capture: not supported by this backend")

// Frame is one encoded picture.
//
// Data is a complete access unit in the codec's own elementary-stream form:
// Annex-B for H.264 and HEVC, an OBU sequence for AV1, a frame payload for VP8
// and VP9. The transport layer packetises it; capture never sees RTP.
type Frame struct {
	Data     []byte
	Keyframe bool

	// CapturedAt is when the pixels were grabbed, not when encoding finished.
	// It is what makes an end-to-end latency figure meaningful.
	CapturedAt time.Time
	// EncodeDuration is how long the encoder held the frame.
	EncodeDuration time.Duration

	Width  int
	Height int

	// InputSeq is the newest input event applied before this frame was
	// captured. The client matches it against the frame that reaches the
	// screen to measure true "key press to pixels" latency.
	InputSeq uint32
}

// AudioFrame is one encoded audio packet, always Opus.
type AudioFrame struct {
	Data       []byte
	Duration   time.Duration
	CapturedAt time.Time
}

// CursorUpdate reports a change to the pointer.
//
// The pointer is sent out of band rather than drawn into the video so the
// client can render it locally at zero latency; Shape is only populated when
// the bitmap itself changed.
type CursorUpdate struct {
	ShapeID  uint32
	Shape    []byte // PNG, only when the shape is new
	Width    int
	Height   int
	HotX     int
	HotY     int
	X        uint16 // normalized 0..65535 across the captured surface
	Y        uint16
	Visible  bool
	Relative bool
}

// Capability describes one encoding path a backend can offer.
type Capability struct {
	Codec Codec
	// Profiles are codec-specific identifiers, ordered best first. For H.264
	// these are profile-level-id values; the agent offers only profiles the
	// client also listed, so negotiation is real rather than assumed.
	Profiles  []string
	Hardware  bool
	Encoder   string
	MaxWidth  int
	MaxHeight int
	MaxFPS    int
}

// Options configure a capture session.
type Options struct {
	Codec     Codec
	Profile   string
	Width     int
	Height    int
	FPS       int
	Bitrate   int
	MonitorID int
	// ExcludeCursor keeps the pointer out of the video so the client can draw
	// it locally. Backends that cannot separate it report CursorEmbedded.
	ExcludeCursor bool
	// AudioEnabled avoids capturing sound nobody is listening to.
	AudioEnabled bool
	// Preset biases the encoder between motion and still-image clarity.
	Preset protocol.QualityPreset
}

// Info describes a running capture session.
type Info struct {
	Codec          Codec
	Profile        string
	Encoder        string
	Backend        string
	Hardware       bool
	Width          int
	Height         int
	FPS            int
	Monitors       []protocol.Monitor
	ActiveMonitor  int
	HasAudio       bool
	CursorEmbedded bool
	SessionKind    string
}

// Stats is the backend's own view of its pipeline, merged into what the client
// sees so the user gets one latency budget rather than two halves of one.
type Stats struct {
	CaptureFPS    float32
	EncodeFPS     float32
	CaptureMillis float32
	EncodeMillis  float32
	QueueMillis   float32
	QP            float32
	// IdleSkipped counts frames not sent because nothing on screen changed.
	// This is the single biggest bandwidth saving on a desktop, and the reason
	// still text converges to near-lossless instead of being re-sent forever.
	IdleSkipped uint32
	Width       int
	Height      int
}

// Source is a running capture and encode pipeline.
//
// Frames, Audio and Cursor are closed when the source stops. Every setter is
// advisory: a backend clamps to what its hardware can do and reports the truth
// through Info and Stats.
type Source interface {
	Frames() <-chan Frame
	Audio() <-chan AudioFrame
	Cursor() <-chan CursorUpdate

	SetBitrate(bitsPerSecond int)
	SetFrameRate(fps int)
	SetResolution(width, height int) error
	SetMonitor(id int) error
	SetPreset(preset protocol.QualityPreset)
	RequestKeyframe()

	// NoteInput records the newest input sequence the machine has applied, so
	// the next captured frame can be tagged with it.
	NoteInput(seq uint32)

	Info() Info
	Stats() Stats
	Close() error
}

// Provider opens capture sources. One exists per platform backend.
type Provider interface {
	// Name identifies the backend in logs and in the client's status panel.
	Name() string
	// Capabilities lists what this machine can encode, best first.
	Capabilities() []Capability
	// Monitors lists the displays available before a session starts.
	Monitors() []protocol.Monitor
	// Open starts a capture session.
	Open(Options) (Source, error)
	// Close releases any provider-level resources.
	Close() error
}

// SelectCodec intersects what this machine can encode with what the client can
// decode, and returns the best match.
//
// Order comes from the backend's capability list, which is ordered by what the
// hardware does well, not by a fixed preference. That is why the same agent
// picks H.264 for a Chromebook with a hardware decoder and VP8 for a Chromium
// build that has no H.264 at all, without either side guessing.
func SelectCodec(caps []Capability, clientCodecs []protocol.CodecCapability) (Codec, string, bool) {
	type clientEntry struct {
		fmtp string
	}
	byCodec := map[string][]clientEntry{}
	for _, c := range clientCodecs {
		mime := normalizeMime(c.MimeType)
		byCodec[mime] = append(byCodec[mime], clientEntry{fmtp: c.SDPFmtpLine})
	}

	for _, capability := range caps {
		entries, ok := byCodec[normalizeMime(string(capability.Codec))]
		if !ok {
			continue
		}
		// A codec with no profile list matches on the codec alone.
		if len(capability.Profiles) == 0 {
			return capability.Codec, "", true
		}
		for _, profile := range capability.Profiles {
			for _, entry := range entries {
				if profile == "" || fmtpMentions(entry.fmtp, profile) {
					return capability.Codec, profile, true
				}
			}
		}
		// The client listed this codec but none of our profiles. Falling back
		// to our first profile would produce a stream it cannot decode, so we
		// keep looking instead.
	}
	return "", "", false
}

func normalizeMime(mime string) string {
	out := make([]byte, 0, len(mime))
	for i := 0; i < len(mime); i++ {
		c := mime[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// fmtpMentions reports whether an SDP fmtp line advertises a profile value.
func fmtpMentions(fmtp, profile string) bool {
	if fmtp == "" {
		return false
	}
	lowerFmtp := normalizeMime(fmtp)
	lowerProfile := normalizeMime(profile)
	return containsSub(lowerFmtp, lowerProfile)
}

func containsSub(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
