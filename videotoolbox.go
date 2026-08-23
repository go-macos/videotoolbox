// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package videotoolbox decodes H.264 and HEVC elementary streams on macOS
// through VideoToolbox, with no cgo.
//
// It exists because of a measured hole. [github.com/go-macos/avfoundation]
// decodes a file end to end — demux included — but AVFoundation does not demux
// Matroska: given an MKV or a WebM it reports no video track at all, whatever
// the file holds. Much of the 3D and immersive material worth decoding ships as
// MKV, so the only way through is to demux it ourselves and hand the coded
// frames to the hardware decoder directly. That is what this package is: the
// second half of that path, with [github.com/go-avkit/avkit/container] the
// first.
//
// The model is a push-pull: build a [Session] from the track's parameter sets,
// hand it one coded frame at a time with [Session.Decode], and take back the
// frames the decoder emitted for it. Everything goes through
// github.com/ebitengine/purego, so a consumer still builds with
// CGO_ENABLED=0.
//
// # What a sample must look like
//
// A [Sample] holds ONE access unit — one coded picture with the non-picture
// units that belong to it. The form its NAL units are in is STATED, in
// [Config.Bitstream], and never sniffed: [AVCC] length prefixes, which is what
// an MP4 sample and a Matroska block both hold and so what container.Reader
// hands back for either, or [AnnexB] start codes, which is what an MPEG-TS
// payload and a raw encoder output hold.
//
// Sniffing was tried and it is not sound. Measured on a plain H.264 MP4, sample
// 205 begins 00 00 01 05 — which is the four-byte AVCC length of a 261-byte NAL
// unit, and is also, byte for byte, an Annex B start code. Any test that reads
// the leading bytes calls that sample Annex B, converts it, and hands the
// decoder rubbish; VideoToolbox answers kVTVideoDecoderBadDataErr 204 good
// frames into a file that decodes perfectly. A per-track guess is no better,
// only luckier: it is the same test run once. The caller knows which form its
// demuxer produces, so the caller states it.
//
// The parameter sets in [Config] are RAW NAL units — no start code, no length
// prefix — which is the form container.Reader's TrackConfig states them in. A
// start code is stripped if one is there anyway.
//
// # Frames
//
// Frames are NOT copied. A [Frame] holds a CVPixelBuffer locked, and its pixels
// stay valid until [Frame.Release] — which the caller must call, once, for every
// frame it receives.
//
// Frames come out in DECODING order, not display order: a stream with B-frames
// emits them out of presentation order, and each frame carries the PTS of the
// sample it came from. A player must reorder by [Frame.PTS]; this package will
// not hold frames back to do it for you, because a decoder that buffers is a
// decoder that adds latency without being asked.
package videotoolbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"time"
)

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by every entry point on non-darwin platforms.
	ErrUnsupported = errors.New("videotoolbox: unsupported on this platform (darwin only)")
	// ErrClosed is returned when a Session is used after [Session.Close].
	ErrClosed = errors.New("videotoolbox: session is closed")
	// ErrUnsupportedCodec is returned by [New] for a codec this package does
	// not build a format description for.
	ErrUnsupportedCodec = errors.New("videotoolbox: unsupported codec")
	// ErrUnsupportedFormat is returned by [New] for a pixel format the decoder
	// will not produce. See [Options.Format].
	ErrUnsupportedFormat = errors.New("videotoolbox: the decoder will not produce that pixel format")
	// ErrParameterSets is returned by [New] when the configuration does not
	// carry the parameter sets its codec needs.
	ErrParameterSets = errors.New("videotoolbox: incomplete parameter sets")
	// ErrNALUnitLength is returned for a length prefix size VideoToolbox does
	// not accept.
	ErrNALUnitLength = errors.New("videotoolbox: invalid NAL unit length prefix size")
	// ErrSample is returned when a sample cannot be submitted as given.
	ErrSample = errors.New("videotoolbox: invalid sample")
)

// Codec names the bitstream a session decodes.
type Codec uint8

// The codecs VideoToolbox will build a format description for from parameter
// sets alone, which is what a demuxed track gives us.
const (
	// H264 is ITU-T H.264 / MPEG-4 AVC, described by its SPS and PPS.
	H264 Codec = iota + 1
	// HEVC is ITU-T H.265, described by its VPS, SPS and PPS.
	HEVC
)

// String names the codec.
func (c Codec) String() string {
	switch c {
	case H264:
		return "h264"
	case HEVC:
		return "hevc"
	default:
		return fmt.Sprintf("Codec(%d)", uint8(c))
	}
}

// CodecFor maps the sample entry names container.Reader reports — "avc1",
// "avc3", "hvc1", "hev1" — onto a [Codec]. It reports false for anything else,
// which is how a caller finds out that a track it demuxed is not one this
// package decodes before it builds a session.
func CodecFor(sampleEntry string) (Codec, bool) {
	switch sampleEntry {
	case "avc1", "avc3":
		return H264, true
	case "hvc1", "hev1":
		return HEVC, true
	}
	return 0, false
}

// Bitstream is how a sample separates its NAL units.
type Bitstream uint8

const (
	// AVCC separates NAL units with a big-endian length prefix of
	// [Config.NALUnitLengthSize] bytes. It is the zero value because it is
	// what an MP4 sample and a Matroska block hold, and so what a demuxed
	// track almost always is. It is also the only form VideoToolbox takes.
	AVCC Bitstream = iota
	// AnnexB separates NAL units with 00 00 01 start codes, as an MPEG-TS
	// payload and a raw encoder output do. Samples in this form are rewritten
	// as AVCC before they are submitted, which allocates.
	AnnexB
)

// String names the bitstream form.
func (b Bitstream) String() string {
	switch b {
	case AVCC:
		return "avcc"
	case AnnexB:
		return "annexb"
	default:
		return fmt.Sprintf("Bitstream(%d)", uint8(b))
	}
}

// PixelFormat is a CoreVideo pixel format, which is a four-character code.
type PixelFormat uint32

// The formats this package can ask the decoder for.
const (
	// BGRA is 32-bit BGRA, 8 bits per channel, one plane. It is the only
	// format this package asks for, and the reason is measured rather than
	// chosen: the planar formats the hardware natively prefers (NV12 and
	// friends) return NULL from CVPixelBufferGetBaseAddress, because their
	// bytes live in per-plane allocations reachable only through
	// CVPixelBufferGetBaseAddressOfPlane. A single-plane [Frame] cannot
	// describe those, so asking for one would hand back an empty picture.
	BGRA PixelFormat = 0x42475241 // 'BGRA'
	// RGBA is 32-bit RGBA. It DESCRIBES a frame but is not accepted as a
	// decode request: measured on macOS, a decompression session asked for it
	// fails to produce usable buffers. Use [Frame.ToRGBA] instead.
	RGBA PixelFormat = 0x52474241 // 'RGBA'
)

// decodable lists the formats a session will ask for. It is a list of ONE,
// for the reason spelled out on [BGRA].
var decodable = map[PixelFormat]bool{BGRA: true}

// String renders the format as its four-character code.
func (f PixelFormat) String() string {
	b := [4]byte{byte(f >> 24), byte(f >> 16), byte(f >> 8), byte(f)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("PixelFormat(%#08x)", uint32(f))
		}
	}
	return string(b[:])
}

// StatusError carries an OSStatus from VideoToolbox or CoreMedia, naming the
// call that returned it. VideoToolbox says everything through these codes, and
// an unexplained -12909 is a long walk from the mistake that caused it.
type StatusError struct {
	// Op is the C function that failed.
	Op string
	// Status is the OSStatus it returned.
	Status int32
}

// osStatusNames are the VideoToolbox and CoreMedia statuses worth spelling out.
var osStatusNames = map[int32]string{
	-12902: "kVTParameterErr: a parameter is wrong",
	-12903: "kVTInvalidSessionErr: the session is no longer usable",
	-12905: "kVTAllocationFailedErr",
	-12906: "kVTPixelTransferNotSupportedErr: no path to the requested pixel format",
	-12907: "kVTCouldNotFindVideoDecoderErr: no decoder for this format",
	-12909: "kVTVideoDecoderBadDataErr: the decoder rejected the coded frame",
	-12910: "kVTVideoDecoderUnsupportedDataFormatErr",
	-12911: "kVTVideoDecoderMalfunctionErr",
	-12912: "kVTVideoDecoderNotAvailableNowErr",
	-12916: "kVTFormatDescriptionChangeNotSupportedErr",
	-12917: "kVTInsufficientSourceColorDataErr",
	-12949: "kVTVideoDecoderReferenceMissingErr: this frame depends on one that was never decoded",
	-12714: "kCMFormatDescriptionError_InvalidParameter: the parameter sets do not describe a stream",
	-12782: "kCMFormatDescriptionBridgeError_InvalidParameter",
	-12730: "kCMBlockBufferBadCustomBlockSourceErr",
	-12731: "kCMBlockBufferBadOffsetParameterErr",
	-12732: "kCMBlockBufferBadLengthParameterErr",
}

func (e *StatusError) Error() string {
	if name, ok := osStatusNames[e.Status]; ok {
		return fmt.Sprintf("videotoolbox: %s: %s (%d)", e.Op, name, e.Status)
	}
	return fmt.Sprintf("videotoolbox: %s: OSStatus %d", e.Op, e.Status)
}

// status wraps an OSStatus, or reports nil for noErr.
func status(op string, s int32) error {
	if s == 0 {
		return nil
	}
	return &StatusError{Op: op, Status: s}
}

// Config describes the track a session decodes, the way a demuxer states it.
//
// container.Reader's TrackConfig hands back everything here but the codec:
// SPS, PPS and VPS are its fields of the same name, raw NAL units without a
// start code or a length prefix.
type Config struct {
	// Codec is the bitstream in the samples. It is taken as stated and never
	// inferred: the two NAL-based codecs spell a unit's type in different
	// bits, so reading one as the other turns a picture into a parameter set.
	Codec Codec
	// Bitstream is how a sample separates its NAL units. The zero value is
	// [AVCC], which is what an MP4 and a Matroska file both hold. It is taken
	// as stated and never sniffed; the package documentation says why, with
	// the measured counter-example.
	Bitstream Bitstream
	// VPS, SPS and PPS are the track's parameter sets as raw NAL units. HEVC
	// needs all three; H.264 needs SPS and PPS and ignores VPS.
	VPS, SPS, PPS [][]byte
	// NALUnitLengthSize is how many bytes of big-endian length precede each
	// NAL unit in a sample. Zero means 4, which is what an MP4 and a Matroska
	// block both use; VideoToolbox accepts 1, 2 and 4 and nothing else.
	NALUnitLengthSize int
	// Width and Height are the coded frame size, carried for the caller's
	// benefit: the decoder reads its own from the SPS.
	Width, Height int
}

// parameterSets is the ordered list a format description is built from, with
// the start codes stripped. The order matters for HEVC: VPS, then SPS, then
// PPS, which is the order a decoder sets itself up in.
func (c Config) parameterSets() ([][]byte, error) {
	var sets [][]byte
	if c.Codec == HEVC {
		if len(c.VPS) == 0 {
			return nil, fmt.Errorf("%w: hevc needs a VPS and the configuration carries none", ErrParameterSets)
		}
		sets = appendSets(sets, c.VPS)
	}
	if len(c.SPS) == 0 || len(c.PPS) == 0 {
		return nil, fmt.Errorf("%w: %s carries %d SPS and %d PPS, and both are needed",
			ErrParameterSets, c.Codec, len(c.SPS), len(c.PPS))
	}
	sets = appendSets(sets, c.SPS)
	sets = appendSets(sets, c.PPS)
	for i, set := range sets {
		if len(set) == 0 {
			return nil, fmt.Errorf("%w: parameter set %d is empty", ErrParameterSets, i)
		}
	}
	return sets, nil
}

// appendSets adds parameter sets with any start code taken off.
func appendSets(dst [][]byte, sets [][]byte) [][]byte {
	for _, set := range sets {
		dst = append(dst, StripStartCode(set))
	}
	return dst
}

// lengthSize resolves the length prefix size, defaulting to 4.
func (c Config) lengthSize() (int, error) {
	n := c.NALUnitLengthSize
	if n == 0 {
		n = 4
	}
	switch n {
	case 1, 2, 4:
		return n, nil
	}
	return 0, fmt.Errorf("%w: %d, and VideoToolbox accepts 1, 2 or 4", ErrNALUnitLength, n)
}

// validate resolves a configuration into the form a session is built from.
func (c Config) validate() (Config, [][]byte, error) {
	switch c.Codec {
	case H264, HEVC:
	default:
		return Config{}, nil, fmt.Errorf("%w: %v", ErrUnsupportedCodec, c.Codec)
	}
	switch c.Bitstream {
	case AVCC, AnnexB:
	default:
		return Config{}, nil, fmt.Errorf("%w: %v", ErrSample, c.Bitstream)
	}
	n, err := c.lengthSize()
	if err != nil {
		return Config{}, nil, err
	}
	sets, err := c.parameterSets()
	if err != nil {
		return Config{}, nil, err
	}
	c.NALUnitLengthSize = n
	return c, sets, nil
}

// Options parametrise [New]. The zero value asks for BGRA from whichever
// decoder VideoToolbox picks, which on Apple silicon is the hardware one.
type Options struct {
	// Format is the pixel format to decode into. Zero means [BGRA], which is
	// also the only accepted value.
	Format PixelFormat
	// RequireHardware refuses a session that would fall back to the software
	// decoder, rather than decoding slowly and saying nothing.
	RequireHardware bool
}

// format resolves the requested format, defaulting to BGRA.
func (o Options) format() PixelFormat {
	if o.Format == 0 {
		return BGRA
	}
	return o.Format
}

// Sample is one coded access unit handed to the decoder.
type Sample struct {
	// Data is the access unit, in the form [Config.Bitstream] states:
	// length-prefixed by default, start-code separated when the session was
	// built for [AnnexB].
	Data []byte
	// PTS is when this picture should be shown, relative to the start of the
	// track. It travels through the decoder and comes back on the [Frame].
	PTS time.Duration
	// Duration is how long the picture lasts; zero if the demuxer does not
	// say. Nothing here depends on it, but VideoToolbox is told.
	Duration time.Duration
}

// Frame is one decoded frame. Its pixels alias a CVPixelBuffer and are valid
// until [Frame.Release] — which the caller must call, once, for every frame it
// receives. Holding many unreleased frames stalls the decoder, which is waiting
// for its own buffers back.
type Frame struct {
	// Width and Height are this frame's dimensions in pixels.
	Width, Height int
	// Stride is the number of bytes per row, which is USUALLY more than
	// Width*4: the decoder pads rows for alignment. Measured here, a
	// 1280-wide frame comes back with a 5120-byte stride — which happens to
	// be Width*4 — and a 1920-wide one does not. Index by Stride.
	Stride int
	// Format is the pixel layout, always [BGRA] today.
	Format PixelFormat
	// PTS is the presentation timestamp of the sample this frame was decoded
	// from. Frames arrive in decoding order, so a stream with B-frames hands
	// these back out of order and a player must sort by them.
	PTS time.Duration
	// Pix is the frame's bytes, Stride*Height of them.
	Pix []byte

	released bool
	release  func()
}

// Release hands the buffer back to the decoder. It is safe to call more than
// once, and a released Frame's Pix must not be read.
func (f *Frame) Release() {
	if f == nil || f.released {
		return
	}
	f.released = true
	f.Pix = nil
	if f.release != nil {
		f.release()
	}
}

// Released reports whether the frame's buffer has been handed back.
func (f *Frame) Released() bool { return f == nil || f.released }

// ToRGBA copies the frame into an *image.RGBA, converting from BGRA if needed.
//
// dst is reused when it is exactly the right size, so a render loop can hold one
// image and not allocate per frame; pass nil, or an image of the wrong size, to
// get a fresh one. It returns nil for a released frame.
func (f *Frame) ToRGBA(dst *image.RGBA) *image.RGBA {
	if f == nil || f.released || f.Width <= 0 || f.Height <= 0 {
		return nil
	}
	if dst == nil || dst.Rect.Dx() != f.Width || dst.Rect.Dy() != f.Height {
		dst = image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	}
	swap := f.Format == BGRA
	for y := 0; y < f.Height; y++ {
		srcRow := f.Pix[y*f.Stride : y*f.Stride+f.Width*4]
		dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+f.Width*4]
		if !swap {
			copy(dstRow, srcRow)
			continue
		}
		for x := 0; x < f.Width*4; x += 4 {
			// BGRA -> RGBA. Alpha is forced opaque: a decoded video frame has
			// no meaningful alpha, and the decoder leaves the byte at zero,
			// which would render the whole picture invisible.
			dstRow[x+0] = srcRow[x+2]
			dstRow[x+1] = srcRow[x+1]
			dstRow[x+2] = srcRow[x+0]
			dstRow[x+3] = 0xff
		}
	}
	return dst
}

// ReleaseAll releases every frame in a batch, which is what a caller that has
// finished with the result of one [Session.Decode] wants.
func ReleaseAll(frames []*Frame) {
	for _, f := range frames {
		f.Release()
	}
}

// Session is a VideoToolbox decompression session: one track's decoder, fed a
// coded frame at a time.
//
// It is NOT safe for concurrent use.
type Session struct {
	cfg    Config
	format PixelFormat
	closed bool

	// h is the platform handle; nil on an unsupported platform.
	h handle
}

// Config returns the configuration the session was built from, with the
// defaults resolved.
func (s *Session) Config() Config { return s.cfg }

// Format returns the pixel format frames are decoded into.
func (s *Session) Format() PixelFormat { return s.format }

// New builds a decompression session for a track.
func New(cfg Config, opts ...Options) (*Session, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	format := o.format()
	if !decodable[format] {
		// Refuse here, with a reason. Passing an unsupported format through
		// would surface as an opaque OSStatus from inside VideoToolbox, or
		// worse as frames with no readable bytes.
		return nil, fmt.Errorf("%w: %v (only %v is decodable)", ErrUnsupportedFormat, format, BGRA)
	}
	resolved, sets, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	o.Format = format
	h, err := newSession(resolved, sets, o)
	if err != nil {
		return nil, err
	}
	return &Session{cfg: resolved, format: format, h: h}, nil
}

// Decode submits one coded frame and returns the frames the decoder emitted for
// it — usually one, none while the decoder is filling its reference buffers, and
// occasionally more.
//
// The caller must [Frame.Release] every frame it receives; [ReleaseAll] does a
// batch.
func (s *Session) Decode(sample Sample) ([]*Frame, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if len(sample.Data) == 0 {
		return nil, fmt.Errorf("%w: the sample carries no bytes", ErrSample)
	}
	if s.cfg.Bitstream == AnnexB {
		converted, err := AnnexBToAVCC(sample.Data, s.cfg.NALUnitLengthSize)
		if err != nil {
			return nil, err
		}
		sample.Data = converted
	}
	return decodeSample(s.h, sample)
}

// Flush waits for every frame still inside the decoder and returns them. A
// caller that has submitted its last sample must call it, or lose whatever the
// decoder was still holding.
func (s *Session) Flush() ([]*Frame, error) {
	if s.closed {
		return nil, ErrClosed
	}
	return flushSession(s.h)
}

// Close tears the session down. Frames already handed out stay valid until they
// are individually released.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return closeSession(s.h)
}

// ---------------------------------------------------------------------------
// Bitstream forms.
//
// Two ways of separating NAL units are in the wild, and a decoder told the
// wrong one reads a length as a NAL header and rejects the frame. An MP4 sample
// and a Matroska block are length-prefixed; an MPEG-TS payload and anything
// that came off a hardware encoder's raw output are Annex B.
// ---------------------------------------------------------------------------

// StripStartCode returns a NAL unit without its Annex B start code, and
// unchanged when it has none. Parameter sets travel both ways in the wild: an
// avcC record holds them bare, an elementary stream holds them separated.
func StripStartCode(nalu []byte) []byte {
	switch {
	case len(nalu) >= 4 && nalu[0] == 0 && nalu[1] == 0 && nalu[2] == 0 && nalu[3] == 1:
		return nalu[4:]
	case len(nalu) >= 3 && nalu[0] == 0 && nalu[1] == 0 && nalu[2] == 1:
		return nalu[3:]
	}
	return nalu
}

// AnnexBToAVCC rewrites a start-code separated access unit as a length-prefixed
// one, with lengthSize bytes of big-endian length before each NAL unit — the
// form a VideoToolbox format description states and the decoder expects.
//
// It allocates: the two forms are not the same size, so this cannot be done in
// place. A caller decoding an MP4 or a Matroska file never reaches it.
func AnnexBToAVCC(data []byte, lengthSize int) ([]byte, error) {
	switch lengthSize {
	case 1, 2, 4:
	default:
		return nil, fmt.Errorf("%w: %d, and VideoToolbox accepts 1, 2 or 4", ErrNALUnitLength, lengthSize)
	}
	units := splitAnnexB(data)
	if len(units) == 0 {
		return nil, fmt.Errorf("%w: no start code separates a NAL unit here", ErrSample)
	}
	max := uint64(1)<<(8*lengthSize) - 1
	total := 0
	for _, u := range units {
		if uint64(len(u)) > max {
			return nil, fmt.Errorf("%w: a NAL unit of %d bytes does not fit a %d-byte length prefix",
				ErrSample, len(u), lengthSize)
		}
		total += lengthSize + len(u)
	}
	out := make([]byte, 0, total)
	var prefix [4]byte
	for _, u := range units {
		binary.BigEndian.PutUint32(prefix[:], uint32(len(u)))
		out = append(out, prefix[4-lengthSize:]...)
		out = append(out, u...)
	}
	return out, nil
}

// splitAnnexB cuts an access unit at its start codes.
//
// A unit runs from the end of its start code to the start of the next one, with
// trailing zero bytes taken off: a four-byte start code is a three-byte one
// with a zero in front, and a stream is free to pad with more, so those zeros
// belong to neither unit. Empty units are dropped — consecutive start codes
// produce them, and a NAL unit of no bytes is not one.
func splitAnnexB(data []byte) [][]byte {
	var units [][]byte
	for i := nextStartCode(data, 0); i >= 0; {
		start := i + 3
		next := nextStartCode(data, start)
		end := len(data)
		if next >= 0 {
			end = next
		}
		for end > start && data[end-1] == 0 {
			end--
		}
		if end > start {
			units = append(units, data[start:end])
		}
		i = next
	}
	return units
}

// nextStartCode is the index of the next 00 00 01 at or after from, or -1.
func nextStartCode(data []byte, from int) int {
	for i := from; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Platform seams. The darwin build assigns the real VideoToolbox
// implementations in an init(); every other platform assigns unsupported stubs.
// Keeping the portable logic above them lets this file be exercised without a
// Mac, and lets a test drive Session through fakes.
// ---------------------------------------------------------------------------

// handle is the platform's decoder state, opaque to the portable layer.
type handle any

var (
	// newSession builds the decompression session for a validated
	// configuration and its ordered parameter sets.
	newSession func(cfg Config, sets [][]byte, o Options) (handle, error)
	// decodeSample submits one access unit and returns what came out.
	decodeSample func(h handle, s Sample) ([]*Frame, error)
	// flushSession drains the decoder.
	flushSession func(h handle) ([]*Frame, error)
	// closeSession tears the decoder down.
	closeSession func(h handle) error
)
