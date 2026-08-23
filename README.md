# go-macos/videotoolbox

Hardware H.264 and HEVC decoding on macOS from pure Go — `CGO_ENABLED=0`, via
[purego](https://github.com/ebitengine/purego). You bring the coded frames; this
brings the decoder.

```go
cfg, _ := reader.TrackConfig(track.ID)          // go-avkit/avkit/container
codec, _ := videotoolbox.CodecFor(cfg.Codec)    // "avc1" -> H264

s, _ := videotoolbox.New(videotoolbox.Config{
        Codec: codec, VPS: cfg.VPS, SPS: cfg.SPS, PPS: cfg.PPS,
})
defer s.Close()

for _, sample := range samples {
        frames, err := s.Decode(videotoolbox.Sample{Data: sample.Data, PTS: pts})
        for _, f := range frames {
                // f.Pix aliases the decoder's buffer: Stride bytes per row, BGRA
                use(f)
                f.Release()
        }
}
```

## Why this exists

[`go-macos/avfoundation`](https://github.com/go-macos/avfoundation) already
decodes a video file end to end, demuxing included, and it is the right tool
when it works. It does not work on Matroska. **AVFoundation does not demux MKV or
WebM**: measured on two real files, `AVURLAsset` reports *no video track at all*,
whatever the file holds.

```
$ avprobe screencasts_defguard-screencast.mkv
avprobe: avfoundation: file has no video track: screencasts_defguard-screencast.mkv
```

A great deal of 3D and immersive material ships as MKV, so the only way through
is to demux it ourselves and hand the coded frames to the hardware decoder
directly. [`go-avkit/avkit/container`](https://github.com/go-avkit/avkit) does
the demuxing — MP4, Matroska/WebM and MPEG-TS, in pure Go, parameter sets
included. This package is the other half: `VTDecompressionSession`, reached
without cgo.

```
$ vtprobe screencasts_defguard-screencast.mkv
  matroska, 54.700 s, 1 tracks (read in 3ms, demuxed in 3ms)
  video track 1: avc1 1920x1080, timescale 1000
  3279 samples, h264, 0 VPS / 1 SPS / 1 PPS
  frame 0: 1920x1080 stride=7680 pts=34ms
           wrote frame00.png
```

## What it covers

| | |
|---|---|
| Codecs | H.264 (`avc1`, `avc3`) and HEVC (`hvc1`, `hev1`), set up from parameter sets |
| Input | one access unit at a time, [AVCC](#the-bitstream-form-is-stated-not-sniffed) length prefixes (1, 2 or 4 bytes) or Annex B start codes |
| Output | zero-copy `Frame`, BGRA, with the PTS of the sample it came from |
| Order | frames come out in **decoding** order; a player sorts by `Frame.PTS` |
| Platforms | darwin arm64 and amd64; every other platform builds and answers `ErrUnsupported` |

Not here: audio, seeking (a caller starts at a sync sample itself, as `vtprobe
-from` shows), and multi-plane pixel formats.

## Three measured limits

**BGRA only.** The planar formats the hardware natively prefers — NV12 and
friends — return `NULL` from `CVPixelBufferGetBaseAddress`, because their bytes
live in per-plane allocations reachable only through
`CVPixelBufferGetBaseAddressOfPlane`. A single-plane `Frame` cannot describe
those, so this asks for `kCVPixelBufferPixelFormatType_32BGRA` and refuses any
other request up front, with a reason. `Frame.ToRGBA` converts for the callers
that need it.

**Use `Stride`, not `Width*4`.** Decoders pad rows. Measured here, 1280-wide
frames come back with a 5120-byte stride and 1920-wide ones with 7680, but that
is not a promise. Indexing by width shears the picture progressively down the
frame, which looks like a decode bug and is not one.

<a name="the-bitstream-form-is-stated-not-sniffed"></a>
**The bitstream form is stated, not sniffed.** Sniffing was tried, and it is not
sound. On a plain H.264 MP4 that decodes perfectly, sample 205 begins:

```
00 00 01 05 41 9a ef 34 …
```

Those first four bytes are the AVCC length of a 261-byte NAL unit. They are also,
byte for byte, an Annex B start code. Any test that reads the leading bytes calls
that sample Annex B, converts it, and hands the decoder rubbish —
`kVTVideoDecoderBadDataErr`, 204 good frames into the file. A per-track guess is
no better, only luckier: it is the same test run once. So `Config.Bitstream`
states it, and the default is `AVCC`, which is what MP4 and Matroska both hold.

## Frames are not copied

A `Frame` holds a `CVPixelBuffer` locked and its pixels stay valid until
`Release()`. At 4K that avoids ~33 MB of copying per frame. Every frame you
receive must be released, once — holding many unreleased frames stalls the
decoder, which is waiting for its own buffers back. `ReleaseAll` does a batch.

What *is* copied is the coded frame going in: one memcpy per access unit into a
`CMBlockBuffer`. The alternative is to hand CoreMedia a pointer into the Go heap
and hope the sample buffer dies before the garbage collector notices, which is
not an alternative.

## Measured

M4 Max, `CGO_ENABLED=0`, decoding through the public API of this package and of
`go-avkit/avkit/container`:

| file | | frames | rate |
|---|---|---|---|
| 4-minute presentation | MP4, H.264 720p | 7 740 / 7 740 | 965 fps, 32× real time |
| screencast | **MKV**, H.264 1080p | 3 279 / 3 279 | 537 fps, 9× real time |
| feature film, 1 h 32 | **MKV**, H.264 720p, 2.0 GB | 133 389 / 133 389 | 805 fps, 34× real time |

The first ten frames of the MP4 are **byte-for-byte identical** to the same ten
decoded by AVFoundation, which is the control this was built against: a decoder
that is wrong is wrong everywhere, and a Matroska path that cannot reproduce a
known-good MP4 result has not proved anything.

`avfoundation` reaches 2165 fps on that same MP4. The difference is this
package's shape, not the decoder's: it waits for each frame before returning it,
so the caller gets its picture from the call that submitted the sample rather
than from a queue it has to manage.

## `cmd/vtprobe`

```
vtprobe movie.mkv                 # info and the first 3 frames as PNG
vtprobe -n 10 -out /tmp movie.mp4
vtprobe -from 25m movie.mkv       # start at the sync sample nearest 25 minutes
vtprobe -all movie.mkv            # decode the whole track, report the rate
vtprobe -hw movie.mkv             # refuse a software-decoder fallback
```

It reads the file whole, because the demuxer takes a byte slice. That is the
tool's limit, not the package's.

## Testing

The portable layer is at **100% statement coverage** behind platform seams, and
CI gates on that file rather than on the total: the purego bindings need a real
decoder, and a total-coverage gate would either be a lie or force a video file
into the repository.

The bindings are covered three ways. A **real** `VTDecompressionSession` is
built on every CI run from an eleven-byte SPS/PPS pair — format description,
callback record, decode, flush, teardown, all exercised with no media anywhere
near the runner. Failure paths (parameter sets that describe nothing, samples
that overrun their own length prefix, a session used after close) run
everywhere. And the end-to-end decode is opt-in:

```
VIDEOTOOLBOX_TEST_FILE=/path/to/movie.mkv go test -race ./...
```

Licence: BSD-3-Clause.
