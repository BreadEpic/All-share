// Command mkpattern builds the synthetic video stream used by the agent's test
// capture backend and by the end-to-end suite.
//
// It renders a moving test pattern, encodes it to VP8 with ffmpeg, then strips
// the WebM container down to a flat list of encoded frames. The result lets the
// agent replay a *real* encoded bitstream on any platform, so an end-to-end
// test can assert that a browser genuinely decoded frames rather than merely
// that a peer connection opened.
//
// VP8 is used because it is the one video codec every Chromium build carries,
// including the headless builds used in CI, which omit H.264.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	var (
		ffmpeg   = flag.String("ffmpeg", "ffmpeg", "path to ffmpeg")
		out      = flag.String("out", "", "output .asvp file")
		width    = flag.Int("width", 960, "frame width")
		height   = flag.Int("height", 540, "frame height")
		fps      = flag.Int("fps", 30, "frame rate")
		seconds  = flag.Int("seconds", 4, "loop length in seconds")
		bitrate  = flag.String("bitrate", "1200k", "target bitrate")
		keyframe = flag.Int("gop", 60, "keyframe interval in frames")
	)
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "mkpattern: -out is required")
		os.Exit(2)
	}

	tmp, err := os.CreateTemp("", "allshare-pattern-*.webm")
	if err != nil {
		fatal(err)
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)

	// Frames are drawn here and piped in as JPEG rather than asking ffmpeg to
	// synthesise them, so the pattern is under this program's control and the
	// tool works against a minimal ffmpeg build with no filter support.
	total := *fps * *seconds
	cmd := exec.Command(*ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "image2pipe", "-c:v", "mjpeg", "-r", fmt.Sprint(*fps), "-i", "pipe:0",
		"-c:v", "libvpx", "-b:v", *bitrate,
		"-deadline", "realtime", "-cpu-used", "4",
		"-g", fmt.Sprint(*keyframe),
		"-pix_fmt", "yuv420p",
		"-f", "webm", tmpName)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal(err)
	}
	if err := cmd.Start(); err != nil {
		fatal(fmt.Errorf("start ffmpeg: %w", err))
	}
	writeErr := make(chan error, 1)
	go func() {
		defer stdin.Close()
		for i := 0; i < total; i++ {
			frame := renderPattern(*width, *height, i, total)
			if err := jpeg.Encode(stdin, frame, &jpeg.Options{Quality: 95}); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	if err := cmd.Wait(); err != nil {
		fatal(fmt.Errorf("run ffmpeg: %w", err))
	}
	if err := <-writeErr; err != nil {
		fatal(fmt.Errorf("write frames: %w", err))
	}

	data, err := os.ReadFile(tmpName)
	if err != nil {
		fatal(err)
	}
	frames, err := extractSimpleBlocks(data)
	if err != nil {
		fatal(err)
	}
	if len(frames) == 0 {
		fatal(fmt.Errorf("ffmpeg produced no frames"))
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	// Header: magic, version, codec tag, width, height, fps, frame count.
	head := make([]byte, 0, 32)
	head = append(head, 'A', 'S', 'V', 'P', 1)
	head = append(head, 'V', 'P', '8', 0)
	head = binary.LittleEndian.AppendUint16(head, uint16(*width))
	head = binary.LittleEndian.AppendUint16(head, uint16(*height))
	head = binary.LittleEndian.AppendUint16(head, uint16(*fps))
	head = binary.LittleEndian.AppendUint32(head, uint32(len(frames)))
	if _, err := f.Write(head); err != nil {
		fatal(err)
	}

	keyframes := 0
	for _, frame := range frames {
		if frame.keyframe {
			keyframes++
		}
		rec := make([]byte, 0, 5+len(frame.data))
		rec = binary.LittleEndian.AppendUint32(rec, uint32(len(frame.data)))
		if frame.keyframe {
			rec = append(rec, 1)
		} else {
			rec = append(rec, 0)
		}
		if _, err := f.Write(rec); err != nil {
			fatal(err)
		}
		if _, err := f.Write(frame.data); err != nil {
			fatal(err)
		}
	}
	info, _ := f.Stat()
	fmt.Printf("wrote %s: %d frames (%d keyframes), %dx%d @ %d fps, %d bytes\n",
		*out, len(frames), keyframes, *width, *height, *fps, info.Size())
}

// renderPattern draws one frame of the synthetic desktop.
//
// It deliberately mixes large flat areas, hard edges and fine one-pixel detail:
// flat areas compress to nothing, edges exercise the encoder, and the fine
// detail is what would blur first if a bitrate change went wrong. A moving
// element guarantees every frame differs, so a decoder that stalls is visible.
func renderPattern(width, height, index, total int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	phase := float64(index) / float64(total)

	// Background: a soft vertical gradient, the "desktop wallpaper".
	for y := 0; y < height; y++ {
		shade := uint8(18 + (float64(y)/float64(height))*46)
		row := color.RGBA{R: shade / 2, G: shade / 2, B: shade, A: 255}
		for x := 0; x < width; x++ {
			img.Set(x, y, row)
		}
	}

	// A one-pixel grid: the finest detail in the frame.
	grid := color.RGBA{R: 60, G: 66, B: 88, A: 255}
	for x := 0; x < width; x += 32 {
		for y := 0; y < height; y++ {
			img.Set(x, y, grid)
		}
	}
	for y := 0; y < height; y += 32 {
		for x := 0; x < width; x++ {
			img.Set(x, y, grid)
		}
	}

	// Static "window" with hard edges and text-like bars.
	fill(img, width/12, height/8, width/2, height/3, color.RGBA{R: 235, G: 238, B: 246, A: 255})
	fill(img, width/12, height/8, width/2, 22, color.RGBA{R: 79, G: 125, B: 255, A: 255})
	for line := 0; line < 6; line++ {
		y := height/8 + 40 + line*16
		w := width/2 - 40 - (line%3)*60
		fill(img, width/12+20, y, w, 6, color.RGBA{R: 40, G: 46, B: 62, A: 255})
	}

	// A moving block, so no two frames are identical.
	bx := int(float64(width-140) * (0.5 + 0.5*math.Sin(phase*2*math.Pi)))
	by := height - height/3
	fill(img, bx, by, 120, 80, color.RGBA{R: 154, G: 107, B: 255, A: 255})

	// A frame counter drawn as a binary bar, readable from a screenshot.
	for bit := 0; bit < 12; bit++ {
		if index&(1<<bit) == 0 {
			continue
		}
		fill(img, 12+bit*18, height-26, 14, 14, color.RGBA{R: 52, G: 211, B: 153, A: 255})
	}
	return img
}

func fill(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			px, py := x+dx, y+dy
			if px < 0 || py < 0 || px >= img.Rect.Dx() || py >= img.Rect.Dy() {
				continue
			}
			img.Set(px, py, c)
		}
	}
}

type blockFrame struct {
	data     []byte
	keyframe bool
}

// EBML element IDs we care about. Everything else is skipped by length.
var (
	idSegment     = []byte{0x18, 0x53, 0x80, 0x67}
	idCluster     = []byte{0x1F, 0x43, 0xB6, 0x75}
	idSimpleBlock = []byte{0xA3}
	idBlockGroup  = []byte{0xA0}
	idBlock       = []byte{0xA1}
)

// extractSimpleBlocks pulls encoded frames out of a WebM file.
//
// This is a deliberately small Matroska reader rather than a dependency: it
// only has to walk into Segment and Cluster and read SimpleBlock payloads from
// a file this program itself just produced.
func extractSimpleBlocks(data []byte) ([]blockFrame, error) {
	var frames []blockFrame
	var walk func(buf []byte, depth int) error

	walk = func(buf []byte, depth int) error {
		if depth > 8 {
			return nil
		}
		pos := 0
		for pos < len(buf) {
			id, idLen, err := readElementID(buf[pos:])
			if err != nil {
				return nil // trailing padding; stop cleanly
			}
			size, sizeLen, unknown, err := readVInt(buf[pos+idLen:])
			if err != nil {
				return nil
			}
			bodyStart := pos + idLen + sizeLen
			bodyEnd := len(buf)
			if !unknown {
				bodyEnd = bodyStart + int(size)
				if bodyEnd > len(buf) {
					bodyEnd = len(buf)
				}
			}
			body := buf[bodyStart:bodyEnd]

			switch {
			case equal(id, idSegment), equal(id, idCluster), equal(id, idBlockGroup):
				if err := walk(body, depth+1); err != nil {
					return err
				}
			case equal(id, idSimpleBlock), equal(id, idBlock):
				frame, ok := parseBlock(body, equal(id, idSimpleBlock))
				if ok {
					frames = append(frames, frame)
				}
			}
			pos = bodyEnd
			if bodyEnd <= bodyStart && size == 0 {
				pos = bodyStart
			}
			if pos <= bodyStart-1 {
				return nil
			}
		}
		return nil
	}

	if err := walk(data, 0); err != nil {
		return nil, err
	}
	return frames, nil
}

// parseBlock reads a SimpleBlock: track number, 16-bit timecode, flags, payload.
func parseBlock(body []byte, simple bool) (blockFrame, bool) {
	if len(body) < 4 {
		return blockFrame{}, false
	}
	_, n, _, err := readVInt(body)
	if err != nil || len(body) < n+3 {
		return blockFrame{}, false
	}
	flags := body[n+2]
	payload := body[n+3:]
	if len(payload) == 0 {
		return blockFrame{}, false
	}
	keyframe := true
	if simple {
		// Bit 0x80 of the flags marks a keyframe in a SimpleBlock.
		keyframe = flags&0x80 != 0
	}
	out := make([]byte, len(payload))
	copy(out, payload)
	return blockFrame{data: out, keyframe: keyframe}, true
}

func readElementID(buf []byte) ([]byte, int, error) {
	if len(buf) == 0 {
		return nil, 0, fmt.Errorf("empty")
	}
	first := buf[0]
	length := 0
	switch {
	case first&0x80 != 0:
		length = 1
	case first&0x40 != 0:
		length = 2
	case first&0x20 != 0:
		length = 3
	case first&0x10 != 0:
		length = 4
	default:
		return nil, 0, fmt.Errorf("invalid element id")
	}
	if len(buf) < length {
		return nil, 0, fmt.Errorf("truncated element id")
	}
	return buf[:length], length, nil
}

func readVInt(buf []byte) (value uint64, length int, unknown bool, err error) {
	if len(buf) == 0 {
		return 0, 0, false, fmt.Errorf("empty")
	}
	first := buf[0]
	mask := byte(0x80)
	for length = 1; length <= 8; length++ {
		if first&mask != 0 {
			break
		}
		mask >>= 1
	}
	if length > 8 || len(buf) < length {
		return 0, 0, false, fmt.Errorf("invalid variable integer")
	}
	value = uint64(first & (mask - 1))
	allOnes := value == uint64(mask-1)
	for i := 1; i < length; i++ {
		value = value<<8 | uint64(buf[i])
		if buf[i] != 0xFF {
			allOnes = false
		}
	}
	return value, length, allOnes, nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mkpattern:", err)
	os.Exit(1)
}
