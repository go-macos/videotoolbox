// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command vtprobe demuxes a video file and decodes its first frames through
// VideoToolbox, writing them as PNGs.
//
// It is this package's dogfood and its end-to-end proof: a Matroska file goes
// in, pictures come out, and nothing in between is AVFoundation — which cannot
// read Matroska at all. Demuxing is github.com/go-avkit/avkit/container's;
// decoding is this package's; everything it does goes through the public API of
// both.
//
//	vtprobe movie.mkv                 # info and the first 3 frames as PNG
//	vtprobe -n 10 -out /tmp movie.mp4
//	vtprobe -all movie.mkv            # decode the whole track, report the rate
//	vtprobe -from 2m movie.mkv        # start at the sync sample nearest 2 minutes
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-macos/videotoolbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "vtprobe:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		count = flag.Int("n", 3, "how many decoded frames to write")
		write = flag.Bool("png", true, "write each decoded frame as a PNG")
		outes = flag.String("out", ".", "directory to write PNGs into")
		all   = flag.Bool("all", false, "decode the whole track and report the rate (writes nothing)")
		hw    = flag.Bool("hw", false, "refuse a session that would fall back to the software decoder")
		from  = flag.Duration("from", 0, "start at the last sync sample at or before this offset")
	)
	flag.Parse()
	if flag.NArg() != 1 {
		return errors.New("usage: vtprobe [flags] <file>")
	}
	path := flag.Arg(0)

	// The demuxer reads from a byte slice, so the file is read whole. That is
	// fine for anything that fits in memory and is the honest limit of this
	// tool, not of the package.
	start := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	read := time.Since(start)

	start = time.Now()
	r, err := container.NewReader(data)
	if err != nil {
		return fmt.Errorf("demux: %w", err)
	}
	demux := time.Since(start)

	file := r.File()
	video := file.VideoTracks()
	if len(video) == 0 {
		return fmt.Errorf("%s holds no video track", path)
	}
	track := video[0]
	fmt.Printf("%s\n", path)
	fmt.Printf("  %s, %.3f s, %d tracks (read in %v, demuxed in %v)\n",
		file.Format, file.DurationSeconds(), len(file.Tracks),
		read.Round(time.Millisecond), demux.Round(time.Millisecond))
	fmt.Printf("  video track %d: %s %dx%d, timescale %d\n",
		track.ID, track.Codec, track.Width, track.Height, track.Timescale)

	cfg, err := r.TrackConfig(track.ID)
	if err != nil {
		return fmt.Errorf("track config: %w", err)
	}
	samples, err := r.Samples(track.ID)
	if err != nil {
		return fmt.Errorf("samples: %w", err)
	}
	codec, ok := videotoolbox.CodecFor(cfg.Codec)
	if !ok {
		return fmt.Errorf("%q is not a codec this decodes (h.264 and hevc are)", cfg.Codec)
	}
	fmt.Printf("  %d samples, %s, %d VPS / %d SPS / %d PPS\n",
		len(samples), codec, len(cfg.VPS), len(cfg.SPS), len(cfg.PPS))

	// Decoding may only begin at a sync sample: every other one refers to
	// pictures a decoder starting here has never seen, and VideoToolbox says
	// so with kVTVideoDecoderReferenceMissingErr rather than guessing.
	first, dts := seek(samples, track.Timescale, *from)
	if first > 0 {
		fmt.Printf("  starting at sync sample %d, %v in\n",
			first, scale(dts, track.Timescale).Round(time.Millisecond))
		samples = samples[first:]
	}

	session, err := videotoolbox.New(videotoolbox.Config{
		Codec:  codec,
		VPS:    cfg.VPS,
		SPS:    cfg.SPS,
		PPS:    cfg.PPS,
		Width:  track.Width,
		Height: track.Height,
	}, videotoolbox.Options{RequireHardware: *hw})
	if err != nil {
		return err
	}
	defer session.Close()

	if *all {
		return decodeAll(session, samples, track.Timescale, dts, file.DurationSeconds())
	}
	return decodeFirst(session, samples, track.Timescale, dts, *count, *write, *outes)
}

// seek is the index of the last sync sample at or before offset, and the
// decoding time it sits at. A decoder cannot start anywhere else: every other
// sample refers to pictures it has never seen.
func seek(samples []container.Sample, timescale uint32, offset time.Duration) (int, int64) {
	var (
		dts   int64
		found int
		at    int64
	)
	for i, s := range samples {
		if scale(dts, timescale) > offset {
			break
		}
		if s.Sync {
			found, at = i, dts
		}
		dts += int64(s.Duration)
	}
	return found, at
}

// scale turns a count of timescale units into a duration.
func scale(units int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	return time.Duration(float64(units) / float64(timescale) * float64(time.Second))
}

// submit hands one demuxed sample to the decoder, at the presentation time the
// container states for it.
func submit(s *videotoolbox.Session, sample container.Sample, dts int64, timescale uint32) ([]*videotoolbox.Frame, error) {
	return s.Decode(videotoolbox.Sample{
		Data:     sample.Data,
		PTS:      scale(dts+int64(sample.CompositionOffset), timescale),
		Duration: scale(int64(sample.Duration), timescale),
	})
}

// maxReorder is how many frames a stream may hold back before the picture that
// belongs earliest arrives: H.264 and HEVC both cap max_num_reorder_frames at
// 16. Decoding that many frames past the ones asked for is what makes the FIRST
// n pictures the first n, rather than the first n the decoder happened to emit.
const maxReorder = 16

// decodeFirst decodes the track's first n frames, in presentation order, and
// writes them.
//
// It counts frames coming OUT, not samples going in — a decoder is entitled to
// hold frames back — and it decodes [maxReorder] frames further than asked,
// because in a stream with B-frames the picture shown first is not the picture
// decoded first. Stopping at the n-th frame emitted would quietly skip the
// opening of the film.
func decodeFirst(s *videotoolbox.Session, samples []container.Sample, timescale uint32, dts int64,
	n int, write bool, dir string) error {
	var out []*videotoolbox.Frame
	defer func() { videotoolbox.ReleaseAll(out) }()
	for i, sample := range samples {
		frames, err := submit(s, sample, dts, timescale)
		if err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
		dts += int64(sample.Duration)
		out = append(out, frames...)
		if len(out) >= n+maxReorder {
			break
		}
	}
	if len(out) < n+maxReorder {
		frames, err := s.Flush()
		if err != nil {
			return err
		}
		out = append(out, frames...)
	}
	if len(out) == 0 {
		return errors.New("the decoder produced no frame at all")
	}
	// Frames come out in decoding order; a viewer wants them in presentation
	// order, and so does anyone looking at the PNGs.
	sort.SliceStable(out, func(i, j int) bool { return out[i].PTS < out[j].PTS })
	if len(out) > n {
		videotoolbox.ReleaseAll(out[n:])
		out = out[:n]
	}
	for i, f := range out {
		fmt.Printf("  frame %d: %dx%d stride=%d pts=%v\n",
			i, f.Width, f.Height, f.Stride, f.PTS.Round(time.Microsecond))
		if !write {
			continue
		}
		name := filepath.Join(dir, fmt.Sprintf("frame%02d.png", i))
		if err := writePNG(name, f); err != nil {
			return err
		}
		fmt.Printf("           wrote %s\n", name)
	}
	return nil
}

// decodeAll decodes the whole track and reports how fast it went.
func decodeAll(s *videotoolbox.Session, samples []container.Sample, timescale uint32, dts int64,
	seconds float64) error {
	var (
		decoded int
		lastPTS time.Duration
		start   = time.Now()
	)
	for i, sample := range samples {
		frames, err := submit(s, sample, dts, timescale)
		if err != nil {
			return fmt.Errorf("sample %d of %d: %w", i, len(samples), err)
		}
		dts += int64(sample.Duration)
		for _, f := range frames {
			if f.PTS > lastPTS {
				lastPTS = f.PTS
			}
			decoded++
		}
		videotoolbox.ReleaseAll(frames)
	}
	frames, err := s.Flush()
	if err != nil {
		return err
	}
	decoded += len(frames)
	videotoolbox.ReleaseAll(frames)

	el := time.Since(start)
	fmt.Printf("  decoded %d of %d samples in %v (%.1f fps, %.1fx real time)\n",
		decoded, len(samples), el.Round(time.Millisecond),
		float64(decoded)/el.Seconds(), lastPTS.Seconds()/el.Seconds())
	fmt.Printf("  last PTS %v vs track duration %.3f s\n", lastPTS.Round(time.Millisecond), seconds)
	if decoded != len(samples) {
		return fmt.Errorf("%d samples went in and %d frames came out", len(samples), decoded)
	}
	return nil
}

func writePNG(name string, f *videotoolbox.Frame) error {
	img := f.ToRGBA(nil)
	if img == nil {
		return errors.New("frame released before it could be converted")
	}
	out, err := os.Create(name)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, img)
}
