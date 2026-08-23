// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package videotoolbox

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-avkit/avkit/container"
)

// minimalH264 is a valid H.264 baseline SPS and PPS pair describing a 16x16
// stream. It is configuration, not media: seven bytes and four bytes of
// parameter set, which is what lets a real VTDecompressionSession be built on a
// CI runner that has no video file anywhere on it.
func minimalH264() Config {
	return Config{
		Codec: H264,
		SPS:   [][]byte{{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}},
		PPS:   [][]byte{{0x68, 0xce, 0x38, 0x80}},
	}
}

func TestCMTimeOf(t *testing.T) {
	// A duration of zero means the demuxer said nothing, and kCMTimeInvalid
	// is how CoreMedia spells that. Stating a valid zero instead would tell
	// the decoder the frame lasts no time at all.
	if got := cmTimeOf(0); got != (cmTime{}) {
		t.Errorf("cmTimeOf(0) = %+v, want the invalid CMTime", got)
	}
	got := cmTimeOf(1500 * time.Millisecond)
	want := cmTime{Value: 1_500_000_000, Timescale: 1_000_000_000, Flags: cmTimeFlagValid}
	if got != want {
		t.Errorf("cmTimeOf(1.5s) = %+v, want %+v", got, want)
	}
	// The nanosecond timescale must fit an int32, or every timestamp is
	// silently wrong.
	if nanosecondTimescale != 1_000_000_000 {
		t.Errorf("nanosecondTimescale = %d, want 1e9", nanosecondTimescale)
	}
}

func TestDoLoadReportsADlopenFailure(t *testing.T) {
	// The real load has to have happened first, or this test leaves the
	// package half-wired for the ones that follow.
	if err := load(); err != nil {
		t.Fatalf("load() = %v", err)
	}
	boom := errors.New("no such library")
	saved := dlopen
	t.Cleanup(func() { dlopen = saved })
	dlopen = func(string) (uintptr, error) { return 0, boom }
	if err := doLoad(); !errors.Is(err, boom) {
		t.Errorf("doLoad with a failing dlopen = %v, want the dlopen error", err)
	}
}

func TestNewRefusesParameterSetsThatDescribeNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"h264 junk", Config{Codec: H264, SPS: [][]byte{{0x67, 0xff, 0xff, 0xff}}, PPS: [][]byte{{0x68, 0xce}}}},
		{"hevc junk", Config{Codec: HEVC, VPS: [][]byte{{0x40, 0x01}},
			SPS: [][]byte{{0x42, 0x01}}, PPS: [][]byte{{0x44, 0x01}}}},
	} {
		s, err := New(tc.cfg)
		if err == nil {
			s.Close()
			t.Errorf("%s: New accepted parameter sets that describe no stream", tc.name)
			continue
		}
		var se *StatusError
		if !errors.As(err, &se) {
			t.Errorf("%s: New = %v, want a *StatusError naming the CoreMedia call", tc.name, err)
		}
	}
}

// TestRealSessionRejectsRubbish builds a REAL VTDecompressionSession — format
// description, callback record, decode, flush and teardown — and feeds it bytes
// that are not a picture. It needs no media, so it is the darwin path's cover on
// a CI runner: everything but the decode of an actual frame runs here.
func TestRealSessionRejectsRubbish(t *testing.T) {
	s, err := New(minimalH264())
	if err != nil {
		t.Fatalf("New(minimal h264) = %v", err)
	}
	defer s.Close()

	// A NAL unit of the right shape whose payload is nonsense: the session
	// takes the sample buffer and the decoder turns it down.
	_, err = s.Decode(Sample{Data: []byte{0, 0, 0, 4, 0x41, 0x9a, 0x00, 0x00}, PTS: time.Second})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Decode of rubbish = %v, want a *StatusError from the decoder", err)
	}
	if se.Status != -12909 {
		t.Logf("decoder answered %v (any refusal is acceptable; -12909 is the usual one)", se)
	}
	frames, err := s.Flush()
	if err != nil {
		t.Errorf("Flush = %v", err)
	}
	ReleaseAll(frames)
}

func TestRealSessionRefusesASampleThatIsNotOne(t *testing.T) {
	s, err := New(minimalH264())
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer s.Close()
	// A four-byte length prefix that claims more bytes than the sample holds.
	if _, err := s.Decode(Sample{Data: []byte{0xff, 0xff, 0xff, 0xff, 0x41}}); err == nil {
		t.Error("Decode accepted a sample whose length prefix overruns it")
	}
}

func TestSessionRegistry(t *testing.T) {
	d := &darwinSession{}
	registerSession(d)
	if d.id == 0 {
		t.Fatal("registerSession left the id at zero, which is the no-reference value")
	}
	if got := lookupSession(d.id); got != d {
		t.Errorf("lookupSession(%d) = %p, want %p", d.id, got, d)
	}
	unregisterSession(d)
	if got := lookupSession(d.id); got != nil {
		t.Errorf("lookupSession after unregister = %p, want nil", got)
	}
	// A callback for a session that has gone must be a no-op, not a panic:
	// closing while a frame is in flight is a normal shutdown.
	decompressionOutput(d.id, 1, 0, 0, 0)
}

func TestCollectClassifiesWhatTheCallbackSaw(t *testing.T) {
	d := &darwinSession{inflight: map[uintptr]time.Duration{1: time.Second}}
	// A dropped frame carries no image buffer and is not a failure.
	d.out = []emitted{{sourceRef: 1, infoFlags: kVTDecodeInfo_FrameDropped}, {sourceRef: 1}}
	frames, err := d.collect()
	if err != nil || len(frames) != 0 {
		t.Errorf("collect of two dropped frames = %v, %v; want no frames and no error", frames, err)
	}
	if len(d.inflight) != 0 {
		t.Errorf("collect left %d samples in flight, want none", len(d.inflight))
	}

	// A failed frame is reported, and the first failure is the one kept.
	d.out = []emitted{{status: -12909}, {status: -12903}}
	if _, err := d.collect(); err == nil {
		t.Error("collect of a failed frame returned no error")
	} else {
		var se *StatusError
		if !errors.As(err, &se) || se.Status != -12909 {
			t.Errorf("collect = %v, want the FIRST failure (-12909)", err)
		}
	}
}

func TestPlatformSeamsRefuseAForeignHandle(t *testing.T) {
	if _, err := darwinDecode(nil, Sample{Data: []byte{1}}); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinDecode(nil) = %v, want ErrClosed", err)
	}
	if _, err := darwinFlush("not a session"); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinFlush(junk) = %v, want ErrClosed", err)
	}
	if err := darwinClose(nil); err != nil {
		t.Errorf("darwinClose(nil) = %v, want nil", err)
	}
}

func TestRetainedAndDictionaryHandleNil(t *testing.T) {
	if got := retained(0); got != 0 {
		t.Errorf("retained(0) = %v, want 0", got)
	}
	if got := dictionary("key", 0); got != 0 {
		t.Errorf("dictionary with a nil value = %v, want 0", got)
	}
}

// TestLiveDecode is the end-to-end proof, and it needs a real file — which a CI
// runner does not have, so it is opt-in. Point VIDEOTOOLBOX_TEST_FILE at an MKV
// and this demuxes it with go-avkit and decodes it here, which is the whole
// reason the package exists.
//
//	VIDEOTOOLBOX_TEST_FILE=/path/to/movie.mkv go test -run Live ./...
func TestLiveDecode(t *testing.T) {
	path := os.Getenv("VIDEOTOOLBOX_TEST_FILE")
	if path == "" {
		t.Skip("set VIDEOTOOLBOX_TEST_FILE to a video file to run the live decode test")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := container.NewReader(data)
	if err != nil {
		t.Fatalf("demux %s: %v", path, err)
	}
	tracks := r.File().VideoTracks()
	if len(tracks) == 0 {
		t.Fatalf("%s holds no video track", path)
	}
	track := tracks[0]
	cfg, err := r.TrackConfig(track.ID)
	if err != nil {
		t.Fatalf("track config: %v", err)
	}
	samples, err := r.Samples(track.ID)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	codec, ok := CodecFor(cfg.Codec)
	if !ok {
		t.Skipf("%s is a %s track, which this package does not decode", path, cfg.Codec)
	}
	s, err := New(Config{Codec: codec, VPS: cfg.VPS, SPS: cfg.SPS, PPS: cfg.PPS})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer s.Close()

	// Twenty samples is enough to get past the first key frame and out of the
	// decoder's warm-up, and short enough to stay a test.
	const want = 20
	decoded := 0
	for i, sample := range samples {
		if decoded >= want {
			break
		}
		frames, err := s.Decode(Sample{Data: sample.Data, PTS: time.Duration(i) * time.Millisecond})
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		for _, f := range frames {
			// Everything asserted here is checkable from outside the package:
			// the size the container states, and a stride that can hold it.
			if f.Width != track.Width || f.Height != track.Height {
				t.Errorf("frame %d is %dx%d, but the container states %dx%d",
					decoded, f.Width, f.Height, track.Width, track.Height)
			}
			if f.Stride < f.Width*4 {
				t.Errorf("frame %d has stride %d, too small for %d BGRA pixels",
					decoded, f.Stride, f.Width)
			}
			if len(f.Pix) != f.Stride*f.Height {
				t.Errorf("frame %d has %d bytes, want stride*height = %d",
					decoded, len(f.Pix), f.Stride*f.Height)
			}
			if f.Format != BGRA {
				t.Errorf("frame %d came back as %v, want BGRA", decoded, f.Format)
			}
			decoded++
		}
		ReleaseAll(frames)
	}
	if decoded == 0 {
		t.Fatalf("%s: %d samples went in and no frame came out", path, len(samples))
	}
	t.Logf("%s: decoded %d frames of %dx%d", path, decoded, track.Width, track.Height)
}
