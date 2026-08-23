// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

package videotoolbox

import (
	"bytes"
	"errors"
	"image"
	"strings"
	"testing"
	"time"
)

// withSeams swaps the platform seams for fakes and restores them afterwards, so
// these tests run identically on darwin (where init() wired VideoToolbox) and on
// any other platform.
func withSeams(t *testing.T,
	create func(Config, [][]byte, Options) (handle, error),
	decode func(handle, Sample) ([]*Frame, error),
	flush func(handle) ([]*Frame, error),
	closeFn func(handle) error,
) {
	t.Helper()
	n, d, f, c := newSession, decodeSample, flushSession, closeSession
	t.Cleanup(func() { newSession, decodeSample, flushSession, closeSession = n, d, f, c })
	if create != nil {
		newSession = create
	}
	if decode != nil {
		decodeSample = decode
	}
	if flush != nil {
		flushSession = flush
	}
	if closeFn != nil {
		closeSession = closeFn
	}
}

// h264 is a configuration that passes validation: one SPS, one PPS.
func h264() Config {
	return Config{Codec: H264, SPS: [][]byte{{0x67, 1, 2}}, PPS: [][]byte{{0x68, 3}}}
}

func TestCodecString(t *testing.T) {
	for _, tc := range []struct {
		in   Codec
		want string
	}{
		{H264, "h264"},
		{HEVC, "hevc"},
		{Codec(9), "Codec(9)"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Codec(%d).String() = %q, want %q", uint8(tc.in), got, tc.want)
		}
	}
}

func TestCodecFor(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  Codec
		ok    bool
	}{
		{"avc1", H264, true},
		{"avc3", H264, true},
		{"hvc1", HEVC, true},
		{"hev1", HEVC, true},
		{"vp09", 0, false},
		{"", 0, false},
	} {
		got, ok := CodecFor(tc.entry)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CodecFor(%q) = %v, %v; want %v, %v", tc.entry, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBitstreamString(t *testing.T) {
	for _, tc := range []struct {
		in   Bitstream
		want string
	}{
		{AVCC, "avcc"},
		{AnnexB, "annexb"},
		{Bitstream(7), "Bitstream(7)"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Bitstream(%d).String() = %q, want %q", uint8(tc.in), got, tc.want)
		}
	}
}

func TestPixelFormatString(t *testing.T) {
	if got := BGRA.String(); got != "BGRA" {
		t.Errorf("BGRA.String() = %q, want %q", got, "BGRA")
	}
	if got := RGBA.String(); got != "RGBA" {
		t.Errorf("RGBA.String() = %q, want %q", got, "RGBA")
	}
	// A code with a non-printable byte must not be rendered as mojibake.
	if got := PixelFormat(0x00000001).String(); !strings.HasPrefix(got, "PixelFormat(") {
		t.Errorf("non-printable format rendered as %q, want the numeric form", got)
	}
}

func TestStatusErrorNamesWhatItCan(t *testing.T) {
	named := (&StatusError{Op: "VTDecompressionSessionDecodeFrame", Status: -12909}).Error()
	if !strings.Contains(named, "kVTVideoDecoderBadDataErr") ||
		!strings.Contains(named, "VTDecompressionSessionDecodeFrame") {
		t.Errorf("Error() = %q, want it to name both the call and the status", named)
	}
	// An unknown status must still say which call produced it: that is the
	// half of the message the caller cannot look up.
	unknown := (&StatusError{Op: "CMSampleBufferCreateReady", Status: -1}).Error()
	if !strings.Contains(unknown, "CMSampleBufferCreateReady") || !strings.Contains(unknown, "-1") {
		t.Errorf("Error() = %q, want the call and the raw status", unknown)
	}
	if strings.Contains(unknown, "kVT") {
		t.Errorf("Error() = %q, want no invented name", unknown)
	}
}

func TestStatusIsNilForNoErr(t *testing.T) {
	if err := status("op", 0); err != nil {
		t.Errorf("status(op, 0) = %v, want nil: noErr is not a failure", err)
	}
	err := status("op", -12903)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != -12903 || se.Op != "op" {
		t.Errorf("status(op, -12903) = %v, want a *StatusError carrying both", err)
	}
}

func TestConfigLengthSize(t *testing.T) {
	for _, n := range []int{0, 1, 2, 4} {
		got, err := Config{NALUnitLengthSize: n}.lengthSize()
		want := n
		if n == 0 {
			want = 4
		}
		if err != nil || got != want {
			t.Errorf("lengthSize(%d) = %d, %v; want %d, nil", n, got, err, want)
		}
	}
	for _, n := range []int{3, 5, -1, 8} {
		if _, err := (Config{NALUnitLengthSize: n}).lengthSize(); !errors.Is(err, ErrNALUnitLength) {
			t.Errorf("lengthSize(%d) = %v, want ErrNALUnitLength", n, err)
		}
	}
}

func TestConfigParameterSets(t *testing.T) {
	// H.264: SPS and PPS, in that order, start codes taken off.
	cfg := Config{
		Codec: H264,
		SPS:   [][]byte{{0, 0, 0, 1, 0x67, 'a'}},
		PPS:   [][]byte{{0, 0, 1, 0x68, 'b'}},
		VPS:   [][]byte{{0x40, 0x99}},
	}
	sets, err := cfg.parameterSets()
	if err != nil {
		t.Fatalf("parameterSets() = %v", err)
	}
	want := [][]byte{{0x67, 'a'}, {0x68, 'b'}}
	if len(sets) != len(want) {
		t.Fatalf("got %d sets, want %d (h.264 must not carry its VPS)", len(sets), len(want))
	}
	for i := range want {
		if !bytes.Equal(sets[i], want[i]) {
			t.Errorf("set %d = % x, want % x", i, sets[i], want[i])
		}
	}

	// HEVC: VPS first, because that is the order a decoder sets itself up in.
	hevc := Config{
		Codec: HEVC,
		VPS:   [][]byte{{0x40, 'v'}},
		SPS:   [][]byte{{0x42, 's'}},
		PPS:   [][]byte{{0x44, 'p'}},
	}
	sets, err = hevc.parameterSets()
	if err != nil {
		t.Fatalf("hevc parameterSets() = %v", err)
	}
	if len(sets) != 3 || sets[0][0] != 0x40 || sets[1][0] != 0x42 || sets[2][0] != 0x44 {
		t.Errorf("hevc sets = %v, want VPS, SPS, PPS in that order", sets)
	}
}

func TestConfigParameterSetsRefusesWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"hevc without vps", Config{Codec: HEVC, SPS: [][]byte{{1}}, PPS: [][]byte{{2}}}, "VPS"},
		{"no sps", Config{Codec: H264, PPS: [][]byte{{2}}}, "0 SPS"},
		{"no pps", Config{Codec: H264, SPS: [][]byte{{1}}}, "0 PPS"},
		{"empty set", Config{Codec: H264, SPS: [][]byte{{}}, PPS: [][]byte{{2}}}, "empty"},
		{"start code only", Config{Codec: H264, SPS: [][]byte{{0, 0, 0, 1}}, PPS: [][]byte{{2}}}, "empty"},
	} {
		_, err := tc.cfg.parameterSets()
		if !errors.Is(err, ErrParameterSets) {
			t.Errorf("%s: err = %v, want ErrParameterSets", tc.name, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	cfg, sets, err := h264().validate()
	if err != nil {
		t.Fatalf("validate() = %v", err)
	}
	if cfg.NALUnitLengthSize != 4 {
		t.Errorf("validate left NALUnitLengthSize at %d, want the default 4", cfg.NALUnitLengthSize)
	}
	if len(sets) != 2 {
		t.Errorf("validate returned %d parameter sets, want 2", len(sets))
	}

	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"no codec", Config{SPS: [][]byte{{1}}, PPS: [][]byte{{2}}}, ErrUnsupportedCodec},
		{"unknown codec", Config{Codec: Codec(9)}, ErrUnsupportedCodec},
		{"unknown bitstream", Config{Codec: H264, Bitstream: Bitstream(9)}, ErrSample},
		{"bad length size", Config{Codec: H264, NALUnitLengthSize: 3}, ErrNALUnitLength},
		{"no parameter sets", Config{Codec: H264}, ErrParameterSets},
	} {
		if _, _, err := tc.cfg.validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: validate() = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestOptionsFormatDefaultsToBGRA(t *testing.T) {
	if got := (Options{}).format(); got != BGRA {
		t.Errorf("zero Options asked for %v, want BGRA (the only format that comes back readable)", got)
	}
	if got := (Options{Format: RGBA}).format(); got != RGBA {
		t.Errorf("explicit format = %v, want RGBA", got)
	}
}

func TestFrameRelease(t *testing.T) {
	var f *Frame
	f.Release() // must not panic on a nil frame
	if !f.Released() {
		t.Error("a nil frame should report itself released")
	}

	released := 0
	g := &Frame{Pix: []byte{1, 2, 3, 4}, release: func() { released++ }}
	if g.Released() {
		t.Error("a fresh frame reports itself released")
	}
	g.Release()
	if released != 1 || !g.Released() || g.Pix != nil {
		t.Errorf("after Release: calls=%d released=%v pix=%v", released, g.Released(), g.Pix)
	}
	g.Release() // idempotent: must not hand the buffer back twice
	if released != 1 {
		t.Errorf("Release called the platform release %d times, want 1", released)
	}

	// A frame with no release function must still mark itself released.
	h := &Frame{Pix: []byte{9}}
	h.Release()
	if !h.Released() {
		t.Error("a frame without a release func did not mark itself released")
	}
}

func TestReleaseAll(t *testing.T) {
	var n int
	frames := []*Frame{
		{release: func() { n++ }},
		{release: func() { n++ }},
		nil,
	}
	ReleaseAll(frames)
	if n != 2 {
		t.Errorf("ReleaseAll released %d frames, want 2 (and no panic on the nil one)", n)
	}
}

// bgraFrame builds a 2x2 frame whose stride is deliberately LARGER than
// width*4, because that is the real case: decoders pad rows, and indexing by
// width instead of stride shears the picture progressively down the frame.
func bgraFrame(format PixelFormat) *Frame {
	const w, h, stride = 2, 2, 12 // 12 > 2*4, so there is padding
	pix := make([]byte, stride*h)
	// Row 0: two pixels, B,G,R,A. First is pure blue in BGRA, second pure red.
	copy(pix[0:], []byte{255, 0, 0, 0, 0, 0, 255, 0})
	// Row 1: green, then white.
	copy(pix[stride:], []byte{0, 255, 0, 0, 255, 255, 255, 0})
	return &Frame{Width: w, Height: h, Stride: stride, Format: format, Pix: pix}
}

func TestFrameToRGBASwapsBGRA(t *testing.T) {
	f := bgraFrame(BGRA)
	img := f.ToRGBA(nil)
	if img == nil {
		t.Fatal("ToRGBA returned nil for a live frame")
	}
	if img.Rect.Dx() != 2 || img.Rect.Dy() != 2 {
		t.Fatalf("image is %v, want 2x2", img.Rect)
	}
	want := [][4]byte{
		{0, 0, 255, 255},     // blue
		{255, 0, 0, 255},     // red
		{0, 255, 0, 255},     // green
		{255, 255, 255, 255}, // white
	}
	for i, w := range want {
		x, y := i%2, i/2
		off := y*img.Stride + x*4
		got := [4]byte(img.Pix[off : off+4])
		if got != w {
			t.Errorf("pixel (%d,%d) = %v, want %v", x, y, got, w)
		}
	}
}

func TestFrameToRGBACopiesWhenNoSwapIsNeeded(t *testing.T) {
	f := bgraFrame(RGBA) // claims to be RGBA already
	img := f.ToRGBA(nil)
	// Row 0 pixel 0 was written as 255,0,0,0 and must come out unswapped,
	// alpha included: nothing is corrected for a frame that is already RGBA.
	if got := [4]byte(img.Pix[0:4]); got != [4]byte{255, 0, 0, 0} {
		t.Errorf("pixel (0,0) = %v, want the bytes copied through", got)
	}
}

func TestFrameToRGBAReusesTheRightDestination(t *testing.T) {
	f := bgraFrame(BGRA)
	dst := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if got := f.ToRGBA(dst); got != dst {
		t.Error("ToRGBA allocated a new image instead of reusing one of the right size")
	}
	small := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if got := f.ToRGBA(small); got == small {
		t.Error("ToRGBA reused an image of the wrong size")
	}
}

func TestFrameToRGBARefusesWhatItCannotRead(t *testing.T) {
	var nilFrame *Frame
	if nilFrame.ToRGBA(nil) != nil {
		t.Error("ToRGBA on a nil frame returned an image")
	}
	released := bgraFrame(BGRA)
	released.Release()
	if released.ToRGBA(nil) != nil {
		t.Error("ToRGBA on a released frame returned an image; its pixels are gone")
	}
	if (&Frame{Width: 0, Height: 4}).ToRGBA(nil) != nil {
		t.Error("ToRGBA on a zero-width frame returned an image")
	}
	if (&Frame{Width: 4, Height: 0}).ToRGBA(nil) != nil {
		t.Error("ToRGBA on a zero-height frame returned an image")
	}
}

func TestNewRefusesAnUndecodableFormat(t *testing.T) {
	withSeams(t, func(Config, [][]byte, Options) (handle, error) {
		t.Error("New reached the platform layer with a format it should have refused")
		return nil, nil
	}, nil, nil, nil)
	_, err := New(h264(), Options{Format: RGBA})
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("New(RGBA) = %v, want ErrUnsupportedFormat", err)
	}
	if !strings.Contains(err.Error(), "BGRA") {
		t.Errorf("err = %q, want it to name the format that does work", err)
	}
}

func TestNewRefusesABadConfiguration(t *testing.T) {
	withSeams(t, func(Config, [][]byte, Options) (handle, error) {
		t.Error("New reached the platform layer with a configuration it should have refused")
		return nil, nil
	}, nil, nil, nil)
	if _, err := New(Config{Codec: H264}); !errors.Is(err, ErrParameterSets) {
		t.Errorf("New without parameter sets = %v, want ErrParameterSets", err)
	}
}

func TestNewReportsThePlatformFailure(t *testing.T) {
	boom := errors.New("no decoder")
	withSeams(t, func(Config, [][]byte, Options) (handle, error) { return nil, boom }, nil, nil, nil)
	if _, err := New(h264()); !errors.Is(err, boom) {
		t.Errorf("New = %v, want the platform's own error", err)
	}
}

// fakeSession records what the portable layer handed the platform.
type fakeSession struct {
	cfg     Config
	sets    [][]byte
	opts    Options
	last    Sample
	flushed int
	closed  int
}

// newFake builds a Session over a fake platform and returns both.
func newFake(t *testing.T, cfg Config, opts ...Options) (*Session, *fakeSession) {
	t.Helper()
	f := &fakeSession{}
	withSeams(t,
		func(c Config, sets [][]byte, o Options) (handle, error) {
			f.cfg, f.sets, f.opts = c, sets, o
			return f, nil
		},
		func(h handle, s Sample) ([]*Frame, error) {
			h.(*fakeSession).last = s
			return []*Frame{{PTS: s.PTS}}, nil
		},
		func(h handle) ([]*Frame, error) {
			h.(*fakeSession).flushed++
			return []*Frame{{PTS: time.Second}}, nil
		},
		func(h handle) error {
			h.(*fakeSession).closed++
			return nil
		})
	s, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return s, f
}

func TestSessionCarriesTheResolvedConfiguration(t *testing.T) {
	s, f := newFake(t, h264(), Options{RequireHardware: true})
	if s.Config().NALUnitLengthSize != 4 {
		t.Errorf("Config().NALUnitLengthSize = %d, want the resolved 4", s.Config().NALUnitLengthSize)
	}
	if s.Format() != BGRA {
		t.Errorf("Format() = %v, want BGRA", s.Format())
	}
	if !f.opts.RequireHardware || f.opts.Format != BGRA {
		t.Errorf("the platform was given %+v, want RequireHardware with the resolved format", f.opts)
	}
	if len(f.sets) != 2 {
		t.Errorf("the platform was given %d parameter sets, want 2", len(f.sets))
	}
}

func TestDecodePassesAVCCThrough(t *testing.T) {
	s, f := newFake(t, h264())
	// This sample is the measured trap: 00 00 01 05 is the four-byte AVCC
	// length of a 261-byte unit AND, byte for byte, an Annex B start code. A
	// session built for AVCC must hand it over untouched.
	data := append([]byte{0, 0, 1, 5}, bytes.Repeat([]byte{0xaa}, 261)...)
	frames, err := s.Decode(Sample{Data: data, PTS: 5 * time.Second})
	if err != nil {
		t.Fatalf("Decode = %v", err)
	}
	if len(frames) != 1 || frames[0].PTS != 5*time.Second {
		t.Errorf("Decode returned %v, want one frame carrying the sample's PTS", frames)
	}
	if !bytes.Equal(f.last.Data, data) {
		t.Errorf("the platform was given % x…, want the sample unchanged", f.last.Data[:8])
	}
}

func TestDecodeConvertsAnnexB(t *testing.T) {
	cfg := h264()
	cfg.Bitstream = AnnexB
	s, f := newFake(t, cfg)
	if _, err := s.Decode(Sample{Data: []byte{0, 0, 0, 1, 0x65, 'a', 'b'}}); err != nil {
		t.Fatalf("Decode = %v", err)
	}
	want := []byte{0, 0, 0, 3, 0x65, 'a', 'b'}
	if !bytes.Equal(f.last.Data, want) {
		t.Errorf("the platform was given % x, want the length-prefixed % x", f.last.Data, want)
	}
}

func TestDecodeReportsAnUnconvertibleSample(t *testing.T) {
	cfg := h264()
	cfg.Bitstream = AnnexB
	s, _ := newFake(t, cfg)
	if _, err := s.Decode(Sample{Data: []byte{1, 2, 3, 4, 5}}); !errors.Is(err, ErrSample) {
		t.Errorf("Decode of a sample with no start code = %v, want ErrSample", err)
	}
}

func TestDecodeRefusesAnEmptySample(t *testing.T) {
	s, _ := newFake(t, h264())
	if _, err := s.Decode(Sample{}); !errors.Is(err, ErrSample) {
		t.Errorf("Decode of an empty sample = %v, want ErrSample", err)
	}
}

func TestSessionCloseIsIdempotentAndFinal(t *testing.T) {
	s, f := newFake(t, h264())
	if _, err := s.Flush(); err != nil {
		t.Fatalf("Flush = %v", err)
	}
	if f.flushed != 1 {
		t.Errorf("the platform was flushed %d times, want 1", f.flushed)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if f.closed != 1 {
		t.Errorf("the platform was closed %d times, want 1", f.closed)
	}
	if _, err := s.Decode(Sample{Data: []byte{1}}); !errors.Is(err, ErrClosed) {
		t.Errorf("Decode after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Flush(); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close = %v, want ErrClosed", err)
	}
}

func TestStripStartCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want []byte
	}{
		{"four-byte", []byte{0, 0, 0, 1, 0x67, 9}, []byte{0x67, 9}},
		{"three-byte", []byte{0, 0, 1, 0x67, 9}, []byte{0x67, 9}},
		{"none", []byte{0x67, 9}, []byte{0x67, 9}},
		{"too short to hold one", []byte{0, 0}, []byte{0, 0}},
		{"leading zeros that are not a start code", []byte{0, 0, 0, 0, 5}, []byte{0, 0, 0, 0, 5}},
	} {
		if got := StripStartCode(tc.in); !bytes.Equal(got, tc.want) {
			t.Errorf("%s: StripStartCode(% x) = % x, want % x", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestAnnexBToAVCC(t *testing.T) {
	in := []byte{
		0, 0, 0, 1, 0x67, 'a', 'b', // four-byte start code
		0, 0, 1, 0x68, 'c', // three-byte
		0, 0, 0, 1, 0x65, 'd', 'e', 'f', 0, // trailing zero belongs to nobody
	}
	got, err := AnnexBToAVCC(in, 4)
	if err != nil {
		t.Fatalf("AnnexBToAVCC = %v", err)
	}
	want := []byte{
		0, 0, 0, 3, 0x67, 'a', 'b',
		0, 0, 0, 2, 0x68, 'c',
		0, 0, 0, 4, 0x65, 'd', 'e', 'f',
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AnnexBToAVCC = % x, want % x", got, want)
	}

	// A one-byte prefix says the same thing in less room.
	got, err = AnnexBToAVCC([]byte{0, 0, 1, 0x67, 'a'}, 1)
	if err != nil || !bytes.Equal(got, []byte{2, 0x67, 'a'}) {
		t.Errorf("AnnexBToAVCC(lengthSize 1) = % x, %v", got, err)
	}
	got, err = AnnexBToAVCC([]byte{0, 0, 1, 0x67, 'a'}, 2)
	if err != nil || !bytes.Equal(got, []byte{0, 2, 0x67, 'a'}) {
		t.Errorf("AnnexBToAVCC(lengthSize 2) = % x, %v", got, err)
	}
}

func TestAnnexBToAVCCRefusesWhatItCannotWrite(t *testing.T) {
	if _, err := AnnexBToAVCC([]byte{0, 0, 1, 9}, 3); !errors.Is(err, ErrNALUnitLength) {
		t.Error("AnnexBToAVCC accepted a three-byte length prefix, which VideoToolbox does not")
	}
	if _, err := AnnexBToAVCC([]byte{1, 2, 3, 4}, 4); !errors.Is(err, ErrSample) {
		t.Error("AnnexBToAVCC accepted bytes with no start code in them")
	}
	// Nothing but start codes is nothing at all.
	if _, err := AnnexBToAVCC([]byte{0, 0, 1, 0, 0, 1}, 4); !errors.Is(err, ErrSample) {
		t.Error("AnnexBToAVCC accepted an access unit of no NAL units")
	}
	// A 300-byte unit does not fit a one-byte length, and writing the low
	// byte instead would be a truncation nothing downstream could notice.
	big := append([]byte{0, 0, 1}, bytes.Repeat([]byte{0x41}, 300)...)
	_, err := AnnexBToAVCC(big, 1)
	if !errors.Is(err, ErrSample) || !strings.Contains(err.Error(), "300") {
		t.Errorf("AnnexBToAVCC of a 300-byte unit into one byte = %v, want a refusal naming the size", err)
	}
}

func TestSplitAnnexBHandlesPadding(t *testing.T) {
	// Consecutive start codes, extra zeros, and a unit that ends the buffer.
	units := splitAnnexB([]byte{0, 0, 1, 0, 0, 0, 1, 0x65, 'a', 0, 0, 0, 0, 1, 0x41, 'b'})
	if len(units) != 2 {
		t.Fatalf("splitAnnexB returned %d units, want 2: %v", len(units), units)
	}
	if !bytes.Equal(units[0], []byte{0x65, 'a'}) || !bytes.Equal(units[1], []byte{0x41, 'b'}) {
		t.Errorf("units = % x, want the two real ones with no padding", units)
	}
	if got := splitAnnexB([]byte{9, 9}); got != nil {
		t.Errorf("splitAnnexB of bytes too short for a start code = %v, want nothing", got)
	}
}
