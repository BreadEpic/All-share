package protocol

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestMouseMoveAbsRoundTrip(t *testing.T) {
	in := MouseMoveAbs{Seq: 0xDEADBEEF, TSMilli: 123456, X: 65535, Y: 0, Buttons: ButtonLeft | ButtonX2}
	enc := in.Encode(nil)
	if len(enc) != SizeMouseMoveAbs {
		t.Fatalf("encoded size = %d, want %d", len(enc), SizeMouseMoveAbs)
	}
	if enc[0] != TypeMouseMoveAbs {
		t.Fatalf("type byte = 0x%02x, want 0x%02x", enc[0], TypeMouseMoveAbs)
	}
	out, err := DecodeMouseMoveAbs(enc[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestMouseMoveRelRoundTripNegative(t *testing.T) {
	in := MouseMoveRel{Seq: 7, TSMilli: 9, DX: -32768, DY: 32767, Buttons: ButtonRight}
	out, err := DecodeMouseMoveRel(in.Encode(nil)[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestMouseWheelRoundTripNegative(t *testing.T) {
	in := MouseWheel{Seq: 1, TSMilli: 2, DX: -WheelTicksPerNotch * 3, DY: WheelTicksPerNotch}
	out, err := DecodeMouseWheel(in.Encode(nil)[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestKeyRoundTripCarriesFullState(t *testing.T) {
	var state KeyBitmap
	state.Set(HIDShiftLeft, true)
	state.Set(0x1A, true) // "w"
	in := Key{Seq: 42, TSMilli: 99, Usage: 0x1A, Down: true, State: state}

	enc := in.Encode(nil)
	if len(enc) != SizeKey {
		t.Fatalf("encoded size = %d, want %d", len(enc), SizeKey)
	}
	out, err := DecodeKey(enc[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Seq != in.Seq || out.Usage != in.Usage || out.Down != in.Down || out.State != in.State {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	if !out.State.Get(HIDShiftLeft) || !out.State.Get(0x1A) {
		t.Fatal("decoded state lost held keys")
	}
}

func TestKeyBitmapDiffProducesMinimalTransitions(t *testing.T) {
	var before, after KeyBitmap
	before.Set(0x1A, true) // w held
	before.Set(0x04, true) // a held
	after.Set(0x1A, true)  // w still held
	after.Set(0x07, true)  // d newly held; a released

	got := map[uint16]bool{}
	before.Diff(&after, func(usage uint16, down bool) { got[usage] = down })

	want := map[uint16]bool{0x04: false, 0x07: true}
	if len(got) != len(want) {
		t.Fatalf("diff produced %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("diff[%#x] = %v, want %v (full diff %v)", k, got[k], v, got)
		}
	}
}

// A dropped key packet must not leave a key stuck: the next packet's state
// snapshot has to restore the agent to the truth on its own.
func TestKeyStateSnapshotHealsDroppedPacket(t *testing.T) {
	var agentState KeyBitmap

	apply := func(snapshot KeyBitmap) {
		agentState.Diff(&snapshot, func(usage uint16, down bool) { agentState.Set(usage, down) })
	}

	var clientState KeyBitmap
	clientState.Set(0x1A, true) // press w — this packet is "lost"
	// (deliberately not applied)

	clientState.Set(0x07, true) // press d — this packet arrives
	apply(clientState)

	if !agentState.Get(0x1A) {
		t.Fatal("agent did not learn about the key from the dropped packet")
	}
	if !agentState.Get(0x07) {
		t.Fatal("agent missed the key from the delivered packet")
	}

	clientState.Set(0x1A, false)
	clientState.Set(0x07, false)
	apply(clientState)
	if agentState.Any() {
		t.Fatal("release snapshot left keys held")
	}
}

func TestDecodeRejectsShortBuffers(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]byte) error
		size int
	}{
		{"MouseMoveAbs", func(b []byte) error { _, err := DecodeMouseMoveAbs(b); return err }, SizeMouseMoveAbs - 1},
		{"MouseMoveRel", func(b []byte) error { _, err := DecodeMouseMoveRel(b); return err }, SizeMouseMoveRel - 1},
		{"MouseButton", func(b []byte) error { _, err := DecodeMouseButton(b); return err }, SizeMouseButton - 1},
		{"MouseWheel", func(b []byte) error { _, err := DecodeMouseWheel(b); return err }, SizeMouseWheel - 1},
		{"Key", func(b []byte) error { _, err := DecodeKey(b); return err }, SizeKey - 1},
		{"KeyStateSync", func(b []byte) error { _, err := DecodeKeyStateSync(b); return err }, SizeKeyStateSync - 1},
		{"InputPing", func(b []byte) error { _, err := DecodeInputPing(b); return err }, SizeInputPing - 1},
		{"CursorState", func(b []byte) error { _, err := DecodeCursorState(b); return err }, SizeCursorState - 1},
		{"FrameMark", func(b []byte) error { _, err := DecodeFrameMark(b); return err }, SizeFrameMark - 1},
		{"Pong", func(b []byte) error { _, err := DecodePong(b); return err }, SizePong - 1},
	}
	for _, tc := range cases {
		for n := 0; n < tc.size; n++ {
			if err := tc.fn(make([]byte, n)); err == nil {
				t.Errorf("%s accepted a %d-byte payload, want ErrShort", tc.name, n)
			}
		}
	}
}

func TestDecodeTextInputRejectsOversizeLength(t *testing.T) {
	// A hostile peer declaring a huge length must not make us allocate.
	b := make([]byte, 8)
	b[4], b[5], b[6], b[7] = 0xFF, 0xFF, 0xFF, 0xFF
	if _, err := DecodeTextInput(b, MaxTextInputBytes); err == nil {
		t.Fatal("accepted a 4 GiB declared text length")
	}
}

func TestCursorShapeRoundTrip(t *testing.T) {
	png := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 64)
	meta := CursorShapeMeta{ShapeID: 5, Width: 32, Height: 32, HotX: 3, HotY: 4, Format: "png"}
	enc, err := EncodeCursorShape(meta, png)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gotMeta, gotPNG, err := DecodeCursorShape(enc[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotMeta.ShapeID != meta.ShapeID || gotMeta.HotX != meta.HotX || gotMeta.Bytes != len(png) {
		t.Fatalf("metadata mismatch: %+v", gotMeta)
	}
	if !bytes.Equal(gotPNG, png) {
		t.Fatal("cursor bitmap corrupted in round trip")
	}
}

func TestNormalizeCoordEndpointsAndRoundTrip(t *testing.T) {
	const w = 1920
	if got := NormalizeCoord(0, w); got != 0 {
		t.Fatalf("left edge normalized to %d, want 0", got)
	}
	if got := NormalizeCoord(w-1, w); got != 65535 {
		t.Fatalf("right edge normalized to %d, want 65535", got)
	}
	if got := NormalizeCoord(9999, w); got != 65535 {
		t.Fatalf("out-of-range coordinate normalized to %d, want clamp to 65535", got)
	}
	for _, px := range []int{0, 1, 640, 959, 960, 1919} {
		if got := DenormalizeCoord(NormalizeCoord(px, w), w); got != px {
			t.Errorf("round trip of pixel %d gave %d", px, got)
		}
	}
}

// Normalized coordinates must survive a resolution change without the cursor
// drifting, which is the whole reason absolute positions are not sent in pixels.
func TestNormalizedCoordsSurviveResolutionChange(t *testing.T) {
	norm := NormalizeCoord(1920/2, 1920)
	at1280 := DenormalizeCoord(norm, 1280)
	if at1280 < 638 || at1280 > 642 {
		t.Fatalf("mid-screen mapped to %d of 1280, want ~640", at1280)
	}
}

func TestScanCodeCoverageForCommonKeys(t *testing.T) {
	must := []uint16{
		0x04, 0x1D, // a, z
		0x1E, 0x27, // 1, 0
		HIDEnter, HIDEsc, HIDBack, HIDTab, HIDSpace, HIDCapsLock,
		HIDF1, HIDF12, HIDInsert, HIDHome, HIDPageUp, HIDDelete, HIDEnd, HIDPageDown,
		HIDRight, HIDLeft, HIDDown, HIDUp, HIDNumLock, HIDPrintScreen, HIDScrollLock,
		HIDControlLeft, HIDShiftLeft, HIDAltLeft, HIDMetaLeft,
		HIDControlRight, HIDShiftRight, HIDAltRight, HIDMetaRight,
	}
	for _, u := range must {
		if _, ok := ScanCodeFor(u); !ok {
			t.Errorf("HID usage %#x has no scancode mapping", u)
		}
	}
	if _, ok := ScanCodeFor(HIDMax); ok {
		t.Error("out-of-range usage returned a scancode")
	}
}

func TestExtendedScanCodesFlagged(t *testing.T) {
	extended := []uint16{HIDRight, HIDLeft, HIDUp, HIDDown, HIDInsert, HIDDelete, HIDHome, HIDEnd, HIDPageUp, HIDPageDown, HIDMetaLeft, HIDControlRight, HIDAltRight}
	for _, u := range extended {
		sc, ok := ScanCodeFor(u)
		if !ok || !sc.Extended() {
			t.Errorf("HID usage %#x: scancode %#x should be flagged extended", u, sc)
		}
	}
	sc, _ := ScanCodeFor(0x04) // "a" is not extended
	if sc.Extended() {
		t.Error("letter key wrongly flagged as extended")
	}
}

func TestJSONControlRoundTrip(t *testing.T) {
	in := Hello{
		VersionMajor: VersionMajor, VersionMinor: VersionMinor,
		DeviceName: "My Gaming PC", StreamWidth: 1920, StreamHeight: 1080,
		Codec: "video/H264", Monitors: []Monitor{{ID: 0, Name: "DELL U2720Q", Width: 3840, Height: 2160, Primary: true, RefreshHz: 60}},
	}
	enc, err := EncodeJSON(TypeHello, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc[0] != TypeHello {
		t.Fatalf("type byte = 0x%02x", enc[0])
	}
	var out Hello
	if err := DecodeJSON(enc[1:], &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DeviceName != in.DeviceName || len(out.Monitors) != 1 || out.Monitors[0].Width != 3840 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestDecodeJSONRejectsOversizeMessage(t *testing.T) {
	huge := make([]byte, MaxCtrlMessage+1)
	var v map[string]any
	if err := DecodeJSON(huge, &v); err == nil {
		t.Fatal("accepted an oversize control message")
	}
}

// Fuzz-ish sweep: every decoder must reject or cleanly parse arbitrary input,
// and must never panic. A remote peer controls these bytes.
func TestDecodersNeverPanicOnRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	decoders := []func([]byte){
		func(b []byte) { _, _ = DecodeMouseMoveAbs(b) },
		func(b []byte) { _, _ = DecodeMouseMoveRel(b) },
		func(b []byte) { _, _ = DecodeMouseButton(b) },
		func(b []byte) { _, _ = DecodeMouseWheel(b) },
		func(b []byte) { _, _ = DecodeKey(b) },
		func(b []byte) { _, _ = DecodeKeyStateSync(b) },
		func(b []byte) { _, _ = DecodeInputPing(b) },
		func(b []byte) { _, _ = DecodeCursorState(b) },
		func(b []byte) { _, _ = DecodeFrameMark(b) },
		func(b []byte) { _, _ = DecodePong(b) },
		func(b []byte) { _, _, _ = DecodeCursorShape(b) },
		func(b []byte) { _, _ = DecodeTextInput(b, MaxTextInputBytes) },
	}
	buf := make([]byte, 128)
	for i := 0; i < 4000; i++ {
		n := rng.Intn(len(buf))
		rng.Read(buf[:n])
		for _, d := range decoders {
			d(buf[:n])
		}
	}
}

// TestWriteVectors emits the cross-language conformance corpus. The JavaScript
// client decodes and re-encodes every entry, which is what proves the two
// independent implementations of this protocol actually agree.
func TestWriteVectors(t *testing.T) {
	type vector struct {
		Name  string `json:"name"`
		Type  uint8  `json:"type"`
		Hex   string `json:"hex"`
		Value any    `json:"value"`
	}
	var state KeyBitmap
	state.Set(0x1A, true)
	state.Set(HIDShiftLeft, true)

	vectors := []vector{}
	add := func(name string, typ uint8, enc []byte, val any) {
		vectors = append(vectors, vector{Name: name, Type: typ, Hex: hexOf(enc), Value: val})
	}

	m1 := MouseMoveAbs{Seq: 1, TSMilli: 2, X: 32768, Y: 16384, Buttons: ButtonLeft}
	add("mouseMoveAbs", TypeMouseMoveAbs, m1.Encode(nil), map[string]any{"seq": m1.Seq, "ts": m1.TSMilli, "x": m1.X, "y": m1.Y, "buttons": m1.Buttons})

	m2 := MouseMoveRel{Seq: 3, TSMilli: 4, DX: -5, DY: 6, Buttons: ButtonRight | ButtonMiddle}
	add("mouseMoveRel", TypeMouseMoveRel, m2.Encode(nil), map[string]any{"seq": m2.Seq, "ts": m2.TSMilli, "dx": m2.DX, "dy": m2.DY, "buttons": m2.Buttons})

	m3 := MouseButton{Seq: 7, TSMilli: 8, Button: 2, Down: true, Buttons: ButtonMiddle}
	add("mouseButton", TypeMouseButton, m3.Encode(nil), map[string]any{"seq": m3.Seq, "ts": m3.TSMilli, "button": m3.Button, "down": m3.Down, "buttons": m3.Buttons})

	m4 := MouseWheel{Seq: 9, TSMilli: 10, DX: -120, DY: 240}
	add("mouseWheel", TypeMouseWheel, m4.Encode(nil), map[string]any{"seq": m4.Seq, "ts": m4.TSMilli, "dx": m4.DX, "dy": m4.DY})

	m5 := Key{Seq: 11, TSMilli: 12, Usage: 0x1A, Down: true, State: state}
	add("key", TypeKey, m5.Encode(nil), map[string]any{"seq": m5.Seq, "ts": m5.TSMilli, "usage": m5.Usage, "down": m5.Down, "state": hexOf(state[:])})

	m6 := KeyStateSync{Seq: 13, TSMilli: 14, Buttons: 0, State: KeyBitmap{}}
	add("keyStateSync", TypeKeyStateSync, m6.Encode(nil), map[string]any{"seq": m6.Seq, "ts": m6.TSMilli, "buttons": m6.Buttons, "state": hexOf(m6.State[:])})

	m7 := InputPing{Seq: 15, TSMic: 1_767_225_600_123_456} // realistic: microseconds since the Unix epoch
	add("inputPing", TypeInputPing, m7.Encode(nil), map[string]any{"seq": m7.Seq, "tsMicro": m7.TSMic})

	m8 := CursorState{Seq: 16, ShapeID: 17, X: 100, Y: 200, Visible: true, Relative: false}
	add("cursorState", TypeCursorState, m8.Encode(nil), map[string]any{"seq": m8.Seq, "shapeId": m8.ShapeID, "x": m8.X, "y": m8.Y, "visible": m8.Visible, "relative": m8.Relative})

	m9 := FrameMark{RTPTimestamp: 0xAABBCCDD, InputSeq: 18, CaptureMicro: 1_767_225_600_654_321, EncodeMicro: 3000, SizeBytes: 45678, Keyframe: true}
	add("frameMark", TypeFrameMark, m9.Encode(nil), map[string]any{"rtpTimestamp": m9.RTPTimestamp, "inputSeq": m9.InputSeq, "captureMicro": m9.CaptureMicro, "encodeMicro": m9.EncodeMicro, "sizeBytes": m9.SizeBytes, "keyframe": m9.Keyframe})

	m10 := Pong{Seq: 19, ClientTSMic: 0x1000, AgentTSMic: 0x2000}
	add("pong", TypePong, m10.Encode(nil), map[string]any{"seq": m10.Seq, "clientTSMicro": m10.ClientTSMic, "agentTSMicro": m10.AgentTSMic})

	m11 := TextInput{Seq: 20, Text: "héllo\nwörld"}
	add("textInput", TypeTextInput, m11.Encode(nil), map[string]any{"seq": m11.Seq, "text": m11.Text})

	out, err := json.MarshalIndent(map[string]any{
		"note":               "Generated by shared/protocol TestWriteVectors. The JS client validates against this.",
		"versionMajor":       VersionMajor,
		"versionMinor":       VersionMinor,
		"keyBitmapBytes":     KeyBitmapBytes,
		"maxCtrlMessage":     MaxCtrlMessage,
		"maxClipboardBytes":  MaxClipboardBytes,
		"wheelTicksPerNotch": WheelTicksPerNotch,
		"vectors":            vectors,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal vectors: %v", err)
	}
	path := filepath.Join("testdata", "vectors.json")
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write vectors: %v", err)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0F])
	}
	return string(out)
}
