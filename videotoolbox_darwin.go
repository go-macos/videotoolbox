// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package videotoolbox

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// Framework paths. VideoToolbox carries the decompression session; CoreMedia
// the format descriptions, block buffers and sample buffers; CoreVideo the
// pixel buffers. libSystem carries malloc, because the parameter sets a format
// description is built from have to be handed over as C memory.
const (
	frameworkVideoToolbox = "/System/Library/Frameworks/VideoToolbox.framework/VideoToolbox"
	frameworkCoreMedia    = "/System/Library/Frameworks/CoreMedia.framework/CoreMedia"
	frameworkCoreVideo    = "/System/Library/Frameworks/CoreVideo.framework/CoreVideo"
	libSystem             = "/usr/lib/libSystem.B.dylib"
)

// CMBlockBufferFlags: allocate the memory block now rather than on first use,
// which is what makes the block buffer safe to copy into straight away.
const kCMBlockBufferAssureMemoryNowFlag = 1 << 0

// VTDecodeInfoFlags: the decoder dropped this frame rather than decoding it,
// and no image buffer comes with the callback.
const kVTDecodeInfo_FrameDropped = 1 << 1

// cvReadOnly is the CVPixelBufferLockFlags bit for a read-only lock. Locking
// read-only lets the decoder keep the buffer in whatever memory it likes; a
// read-write lock can force a copy out of GPU-visible memory.
const cvReadOnly = 1

// nanosecondTimescale is the timescale every CMTime here is stated in, so a Go
// time.Duration crosses without rounding. It fits an int32 with room to spare.
const nanosecondTimescale = int32(time.Second / time.Nanosecond)

// cmTime is CoreMedia's CMTime: 24 bytes. It is only ever passed BY POINTER
// here, inside a CMSampleTimingInfo — which is deliberate. The decompression
// callback receives two CMTimes by value, and how a 24-byte struct is passed
// differs between arm64 (by reference) and amd64 (on the stack); rather than
// depend on that, the callback is declared with the arguments BEFORE them and
// ignores the rest, which every C calling convention allows. The presentation
// time comes back on the source frame reference instead.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

// cmTimeFlagValid is CMTime's kCMTimeFlags_Valid. A CMTime without it is
// kCMTimeInvalid, which is how "no such time" is stated.
const cmTimeFlagValid = 1

// cmTimeOf states a duration as a CMTime. A zero duration is stated as invalid
// rather than as zero: a demuxer that does not know how long a frame lasts is
// saying nothing, not saying zero.
func cmTimeOf(d time.Duration) cmTime {
	if d == 0 {
		return cmTime{}
	}
	return cmTime{Value: int64(d), Timescale: nanosecondTimescale, Flags: cmTimeFlagValid}
}

// cmSampleTimingInfo is CoreMedia's CMSampleTimingInfo, three CMTimes.
type cmSampleTimingInfo struct {
	Duration              cmTime
	PresentationTimeStamp cmTime
	DecodeTimeStamp       cmTime
}

// vtOutputCallbackRecord is VTDecompressionOutputCallbackRecord: a function
// pointer and the caller's reference. It is passed by POINTER, which is what
// VTDecompressionSessionCreate takes.
type vtOutputCallbackRecord struct {
	callback uintptr
	refCon   uintptr
}

var (
	cmVideoFormatDescriptionCreateFromH264ParameterSets func(
		alloc uintptr, count uint64, ptrs, sizes unsafe.Pointer, headerLength int32, out *uintptr) int32
	cmVideoFormatDescriptionCreateFromHEVCParameterSets func(
		alloc uintptr, count uint64, ptrs, sizes unsafe.Pointer, headerLength int32,
		extensions uintptr, out *uintptr) int32
	cmBlockBufferCreateWithMemoryBlock func(
		structureAllocator, memoryBlock uintptr, blockLength uint64,
		blockAllocator, customBlockSource uintptr, offsetToData, dataLength uint64,
		flags uint32, out *uintptr) int32
	cmBlockBufferReplaceDataBytes func(
		source unsafe.Pointer, destination uintptr, offset, length uint64) int32
	cmSampleBufferCreateReady func(
		alloc, dataBuffer, formatDescription uintptr, numSamples, numTimings int64,
		timings *cmSampleTimingInfo, numSizes int64, sizes *uint64, out *uintptr) int32
	cfRelease func(uintptr)

	vtDecompressionSessionCreate func(
		alloc, formatDescription, specification, destinationAttributes uintptr,
		callback *vtOutputCallbackRecord, out *uintptr) int32
	vtDecompressionSessionDecodeFrame func(
		session, sampleBuffer uintptr, flags uint32, sourceFrameRefCon uintptr, infoOut *uint32) int32
	vtDecompressionSessionWaitForAsynchronousFrames func(session uintptr) int32
	vtDecompressionSessionInvalidate                func(session uintptr)

	cvPixelBufferLockBaseAddress    func(uintptr, uint64) int32
	cvPixelBufferUnlockBaseAddress  func(uintptr, uint64) int32
	cvPixelBufferGetBaseAddress     func(uintptr) unsafe.Pointer
	cvPixelBufferGetBytesPerRow     func(uintptr) uint64
	cvPixelBufferGetWidth           func(uintptr) uint64
	cvPixelBufferGetHeight          func(uintptr) uint64
	cvPixelBufferGetPixelFormatType func(uintptr) uint32
	cvPixelBufferIsPlanar           func(uintptr) bool
	cvBufferRetain                  func(uintptr) uintptr

	cMalloc func(uint64) unsafe.Pointer
	cFree   func(unsafe.Pointer)
)

// outputCallback is the one C function pointer this package ever creates.
// purego never frees a callback and allows a bounded number of them, so making
// one per session would be a leak with a hard ceiling; the session is found
// through the reference the callback carries instead.
var outputCallback uintptr

var (
	loadOnce sync.Once
	loadErr  error
)

// load resolves the frameworks and C entry points once.
func load() error {
	loadOnce.Do(func() { loadErr = doLoad() })
	return loadErr
}

func doLoad() error {
	if err := objc.Load(objc.Foundation, frameworkVideoToolbox,
		frameworkCoreMedia, frameworkCoreVideo); err != nil {
		return err
	}
	vt, err := dlopen(frameworkVideoToolbox)
	if err != nil {
		return err
	}
	cm, err := dlopen(frameworkCoreMedia)
	if err != nil {
		return err
	}
	cv, err := dlopen(frameworkCoreVideo)
	if err != nil {
		return err
	}
	cf, err := dlopen(objc.CoreFoundation)
	if err != nil {
		return err
	}
	sys, err := dlopen(libSystem)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cmVideoFormatDescriptionCreateFromH264ParameterSets, cm,
		"CMVideoFormatDescriptionCreateFromH264ParameterSets")
	purego.RegisterLibFunc(&cmVideoFormatDescriptionCreateFromHEVCParameterSets, cm,
		"CMVideoFormatDescriptionCreateFromHEVCParameterSets")
	purego.RegisterLibFunc(&cmBlockBufferCreateWithMemoryBlock, cm, "CMBlockBufferCreateWithMemoryBlock")
	purego.RegisterLibFunc(&cmBlockBufferReplaceDataBytes, cm, "CMBlockBufferReplaceDataBytes")
	purego.RegisterLibFunc(&cmSampleBufferCreateReady, cm, "CMSampleBufferCreateReady")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&vtDecompressionSessionCreate, vt, "VTDecompressionSessionCreate")
	purego.RegisterLibFunc(&vtDecompressionSessionDecodeFrame, vt, "VTDecompressionSessionDecodeFrame")
	purego.RegisterLibFunc(&vtDecompressionSessionWaitForAsynchronousFrames, vt,
		"VTDecompressionSessionWaitForAsynchronousFrames")
	purego.RegisterLibFunc(&vtDecompressionSessionInvalidate, vt, "VTDecompressionSessionInvalidate")
	purego.RegisterLibFunc(&cvPixelBufferLockBaseAddress, cv, "CVPixelBufferLockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferUnlockBaseAddress, cv, "CVPixelBufferUnlockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBaseAddress, cv, "CVPixelBufferGetBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBytesPerRow, cv, "CVPixelBufferGetBytesPerRow")
	purego.RegisterLibFunc(&cvPixelBufferGetWidth, cv, "CVPixelBufferGetWidth")
	purego.RegisterLibFunc(&cvPixelBufferGetHeight, cv, "CVPixelBufferGetHeight")
	purego.RegisterLibFunc(&cvPixelBufferGetPixelFormatType, cv, "CVPixelBufferGetPixelFormatType")
	purego.RegisterLibFunc(&cvPixelBufferIsPlanar, cv, "CVPixelBufferIsPlanar")
	purego.RegisterLibFunc(&cvBufferRetain, cv, "CVBufferRetain")
	purego.RegisterLibFunc(&cMalloc, sys, "malloc")
	purego.RegisterLibFunc(&cFree, sys, "free")
	outputCallback = purego.NewCallback(decompressionOutput)
	return nil
}

// dlopen is a seam so a test can force doLoad's failure path.
var dlopen = func(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// ---------------------------------------------------------------------------
// The session registry. The decompression callback is a C function pointer and
// cannot close over anything, so it carries an integer key and looks the
// session up. An integer, not a Go pointer: handing C a pointer into the Go
// heap and expecting it back later is exactly what the cgo pointer rules
// forbid.
// ---------------------------------------------------------------------------

var registry struct {
	sync.Mutex
	next uintptr
	byID map[uintptr]*darwinSession
}

func registerSession(d *darwinSession) {
	registry.Lock()
	defer registry.Unlock()
	if registry.byID == nil {
		registry.byID = map[uintptr]*darwinSession{}
	}
	registry.next++
	d.id = registry.next
	registry.byID[d.id] = d
}

func unregisterSession(d *darwinSession) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.byID, d.id)
}

func lookupSession(id uintptr) *darwinSession {
	registry.Lock()
	defer registry.Unlock()
	return registry.byID[id]
}

// emitted is one callback's worth of output, kept until the Go side turns it
// into a Frame.
type emitted struct {
	imageBuffer uintptr
	sourceRef   uintptr
	status      int32
	infoFlags   uint32
}

// decompressionOutput is VTDecompressionOutputCallback.
//
// The real C signature ends with two CMTimes passed by value, which are NOT
// declared here: a callee is free to ignore trailing arguments under both the
// arm64 and the amd64 C calling conventions, and a 24-byte struct is passed
// differently by each of them. The presentation time comes back on
// sourceFrameRefCon, which the caller set, so nothing is lost.
func decompressionOutput(refCon, sourceRef uintptr, st int32, infoFlags uint32, imageBuffer uintptr) {
	d := lookupSession(refCon)
	if d == nil {
		// The session was closed while a frame was in flight. Nothing owns
		// the buffer; VideoToolbox releases it when this returns.
		return
	}
	if imageBuffer != 0 {
		// The buffer belongs to VideoToolbox and dies when this callback
		// returns, so it is retained for as long as the Frame lives.
		imageBuffer = cvBufferRetain(imageBuffer)
	}
	d.mu.Lock()
	d.out = append(d.out, emitted{imageBuffer: imageBuffer, sourceRef: sourceRef, status: st, infoFlags: infoFlags})
	d.mu.Unlock()
}

// darwinSession is one VTDecompressionSession and what it needs to stay alive.
type darwinSession struct {
	id      uintptr
	session uintptr
	format  uintptr // CMVideoFormatDescriptionRef
	attrs   objc.ID // destination image buffer attributes, retained
	spec    objc.ID // video decoder specification, retained; 0 when none
	pixel   PixelFormat

	mu       sync.Mutex
	out      []emitted
	sequence uintptr
	inflight map[uintptr]time.Duration
}

func init() {
	newSession = darwinNew
	decodeSample = darwinDecode
	flushSession = darwinFlush
	closeSession = darwinClose
}

// darwinNew builds the format description and the decompression session.
func darwinNew(cfg Config, sets [][]byte, o Options) (handle, error) {
	if err := load(); err != nil {
		return nil, err
	}
	format, err := formatDescription(cfg, sets)
	if err != nil {
		return nil, err
	}
	d := &darwinSession{format: format, pixel: o.Format, inflight: map[uintptr]time.Duration{}}
	objc.AutoreleasePool(func() {
		// The attributes and the specification are autoreleased dictionaries;
		// VideoToolbox holds onto them for the session's lifetime, so they are
		// retained here and released in Close.
		d.attrs = retained(dictionary("PixelFormatType",
			objc.ClassID("NSNumber").Send(objc.Sel("numberWithUnsignedInt:"), uint32(o.Format))))
		if o.RequireHardware {
			d.spec = retained(dictionary("RequireHardwareAcceleratedVideoDecoder",
				objc.ClassID("NSNumber").Send(objc.Sel("numberWithBool:"), true)))
		}
	})
	if d.attrs == 0 {
		d.releaseObjects()
		return nil, fmt.Errorf("videotoolbox: could not build the destination pixel buffer attributes")
	}
	registerSession(d)
	record := vtOutputCallbackRecord{callback: outputCallback, refCon: d.id}
	st := vtDecompressionSessionCreate(0, format, uintptr(d.spec), uintptr(d.attrs), &record, &d.session)
	if st != 0 {
		unregisterSession(d)
		d.releaseObjects()
		return nil, status("VTDecompressionSessionCreate", st)
	}
	return d, nil
}

// retained sends retain to an object and hands it back, or 0 for nil.
func retained(id objc.ID) objc.ID {
	if id == 0 {
		return 0
	}
	return id.Send(objc.Sel("retain"))
}

// dictionary builds a one-entry NSDictionary, which is toll-free bridged to the
// CFDictionary these APIs take. Building the key as an NSString rather than
// reading the exported CF constant avoids a Dlsym and the uintptr-to-pointer
// conversion go vet would rightly flag; the strings ARE the constants' values.
func dictionary(key string, value objc.ID) objc.ID {
	if value == 0 {
		return 0
	}
	return objc.ClassID("NSDictionary").Send(
		objc.Sel("dictionaryWithObject:forKey:"), value, objc.NSString(key))
}

// formatDescription builds the CMVideoFormatDescription the decoder is set up
// from. The parameter sets are copied into C memory first: they are handed over
// as an array of pointers, and an array of pointers into the Go heap is what
// the cgo rules forbid outright.
func formatDescription(cfg Config, sets [][]byte) (uintptr, error) {
	c, err := copySets(sets)
	if err != nil {
		return 0, err
	}
	// The format description copies the parameter sets, so the C memory is
	// only needed for the length of the call.
	defer c.free()
	var (
		desc uintptr
		st   int32
		op   string
	)
	switch cfg.Codec {
	case HEVC:
		op = "CMVideoFormatDescriptionCreateFromHEVCParameterSets"
		st = cmVideoFormatDescriptionCreateFromHEVCParameterSets(0, uint64(len(sets)),
			c.ptrs, c.sizes, int32(cfg.NALUnitLengthSize), 0, &desc)
	default:
		op = "CMVideoFormatDescriptionCreateFromH264ParameterSets"
		st = cmVideoFormatDescriptionCreateFromH264ParameterSets(0, uint64(len(sets)),
			c.ptrs, c.sizes, int32(cfg.NALUnitLengthSize), &desc)
	}
	if st != 0 {
		return 0, status(op, st)
	}
	if desc == 0 {
		return 0, fmt.Errorf("videotoolbox: %s returned no format description", op)
	}
	return desc, nil
}

// cSets is the parameter sets in C memory: the bytes, an array of pointers to
// them, and an array of their sizes.
type cSets struct {
	ptrs, sizes unsafe.Pointer
	blocks      []unsafe.Pointer
}

func (c *cSets) free() {
	for _, b := range c.blocks {
		cFree(b)
	}
	if c.ptrs != nil {
		cFree(c.ptrs)
	}
	if c.sizes != nil {
		cFree(c.sizes)
	}
}

func copySets(sets [][]byte) (*cSets, error) {
	const word = uint64(unsafe.Sizeof(uintptr(0)))
	c := &cSets{}
	c.ptrs = cMalloc(uint64(len(sets)) * word)
	c.sizes = cMalloc(uint64(len(sets)) * word)
	if c.ptrs == nil || c.sizes == nil {
		c.free()
		return nil, fmt.Errorf("videotoolbox: out of memory copying %d parameter sets", len(sets))
	}
	ptrs := unsafe.Slice((*uintptr)(c.ptrs), len(sets))
	sizes := unsafe.Slice((*uint64)(c.sizes), len(sets))
	for i, set := range sets {
		block := cMalloc(uint64(len(set)))
		if block == nil {
			c.free()
			return nil, fmt.Errorf("videotoolbox: out of memory copying parameter set %d", i)
		}
		c.blocks = append(c.blocks, block)
		copy(unsafe.Slice((*byte)(block), len(set)), set)
		ptrs[i] = uintptr(block)
		sizes[i] = uint64(len(set))
	}
	return c, nil
}

// darwinDecode submits one access unit and collects what came back.
//
// The decode is made synchronous on purpose: VideoToolbox is asked to decode
// and then waited on, so a caller gets its frames from the call that submitted
// them. Decoding asynchronously would be faster on paper and would hand the
// caller a queue to manage; a pull API that returns nothing most of the time is
// worse than one that returns a frame.
func darwinDecode(h handle, s Sample) ([]*Frame, error) {
	d, ok := h.(*darwinSession)
	if !ok || d == nil {
		return nil, ErrClosed
	}
	block, err := d.blockBuffer(s.Data)
	if err != nil {
		return nil, err
	}
	sbuf, err := d.sampleBuffer(block, s)
	// The sample buffer retains the block buffer, so this reference is done
	// with either way.
	cfRelease(block)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.sequence++
	ref := d.sequence
	d.inflight[ref] = s.PTS
	d.mu.Unlock()

	var info uint32
	st := vtDecompressionSessionDecodeFrame(d.session, sbuf, 0, ref, &info)
	cfRelease(sbuf)
	if st != 0 {
		d.forget(ref)
		return nil, status("VTDecompressionSessionDecodeFrame", st)
	}
	if st := vtDecompressionSessionWaitForAsynchronousFrames(d.session); st != 0 {
		d.forget(ref)
		return nil, status("VTDecompressionSessionWaitForAsynchronousFrames", st)
	}
	return d.collect()
}

// forget drops an in-flight sample whose frame will never arrive.
func (d *darwinSession) forget(ref uintptr) {
	d.mu.Lock()
	delete(d.inflight, ref)
	d.mu.Unlock()
}

// blockBuffer copies an access unit into a CoreMedia block buffer.
//
// The copy is deliberate. CMBlockBufferCreateWithMemoryBlock can be told to
// wrap memory it does not own, but the sample buffer outlives this call, and
// the memory would be a Go slice — which the runtime is entitled to move out
// from under it. One memcpy per frame is the price of not lying to the garbage
// collector.
func (d *darwinSession) blockBuffer(data []byte) (uintptr, error) {
	var block uintptr
	st := cmBlockBufferCreateWithMemoryBlock(0, 0, uint64(len(data)), 0, 0, 0,
		uint64(len(data)), kCMBlockBufferAssureMemoryNowFlag, &block)
	if st != 0 {
		return 0, status("CMBlockBufferCreateWithMemoryBlock", st)
	}
	if st := cmBlockBufferReplaceDataBytes(unsafe.Pointer(&data[0]), block, 0, uint64(len(data))); st != 0 {
		cfRelease(block)
		return 0, status("CMBlockBufferReplaceDataBytes", st)
	}
	return block, nil
}

// sampleBuffer wraps a block buffer as the one-sample CMSampleBuffer
// VTDecompressionSessionDecodeFrame takes.
func (d *darwinSession) sampleBuffer(block uintptr, s Sample) (uintptr, error) {
	timing := cmSampleTimingInfo{
		Duration:              cmTimeOf(s.Duration),
		PresentationTimeStamp: cmTimeOf(s.PTS),
		// The decode timestamp is left invalid: a demuxer states presentation
		// time, and VideoToolbox does not need decode time to decode one
		// access unit at a time.
	}
	sizes := [1]uint64{uint64(len(s.Data))}
	var sbuf uintptr
	st := cmSampleBufferCreateReady(0, block, d.format, 1, 1, &timing, 1, &sizes[0], &sbuf)
	if st != 0 {
		return 0, status("CMSampleBufferCreateReady", st)
	}
	return sbuf, nil
}

// collect turns what the callback recorded into Frames.
func (d *darwinSession) collect() ([]*Frame, error) {
	d.mu.Lock()
	out := d.out
	d.out = nil
	d.mu.Unlock()

	var (
		frames []*Frame
		failed error
	)
	for _, e := range out {
		switch {
		case e.status != 0:
			if failed == nil {
				failed = status("VTDecompressionOutputCallback", e.status)
			}
		case e.infoFlags&kVTDecodeInfo_FrameDropped != 0, e.imageBuffer == 0:
			// A dropped frame is not a failure: the decoder was told the
			// frame was not needed, or had nothing to show for it.
		default:
			f, err := d.wrap(e)
			if err != nil {
				if failed == nil {
					failed = err
				}
				continue
			}
			frames = append(frames, f)
			continue
		}
		if e.imageBuffer != 0 {
			cfRelease(e.imageBuffer)
		}
	}
	d.mu.Lock()
	// Every frame submitted so far has now been accounted for: the decode
	// waited for the decoder to finish. Anything left is a frame the decoder
	// silently dropped, and keeping it would grow the map for the life of the
	// session.
	clear(d.inflight)
	d.mu.Unlock()
	if failed != nil {
		ReleaseAll(frames)
		return nil, failed
	}
	return frames, nil
}

// wrap locks a decoded pixel buffer and describes it as a Frame, without
// copying a pixel.
func (d *darwinSession) wrap(e emitted) (*Frame, error) {
	if planar := cvPixelBufferIsPlanar(e.imageBuffer); planar {
		// This cannot happen while BGRA is the only format asked for, and it
		// is checked anyway because the failure it guards is silent: a planar
		// buffer answers NULL to CVPixelBufferGetBaseAddress, since its bytes
		// live in per-plane allocations.
		return nil, fmt.Errorf("videotoolbox: the decoder produced a planar %v buffer, "+
			"whose bytes are only reachable through CVPixelBufferGetBaseAddressOfPlane",
			PixelFormat(cvPixelBufferGetPixelFormatType(e.imageBuffer)))
	}
	if got := PixelFormat(cvPixelBufferGetPixelFormatType(e.imageBuffer)); got != d.pixel {
		return nil, fmt.Errorf("%w: asked for %v, the decoder produced %v", ErrUnsupportedFormat, d.pixel, got)
	}
	if st := cvPixelBufferLockBaseAddress(e.imageBuffer, cvReadOnly); st != 0 {
		return nil, status("CVPixelBufferLockBaseAddress", st)
	}
	base := cvPixelBufferGetBaseAddress(e.imageBuffer)
	w := int(cvPixelBufferGetWidth(e.imageBuffer))
	h := int(cvPixelBufferGetHeight(e.imageBuffer))
	stride := int(cvPixelBufferGetBytesPerRow(e.imageBuffer))
	if base == nil || w <= 0 || h <= 0 || stride <= 0 {
		cvPixelBufferUnlockBaseAddress(e.imageBuffer, cvReadOnly)
		return nil, fmt.Errorf("videotoolbox: empty pixel buffer %dx%d stride %d", w, h, stride)
	}
	d.mu.Lock()
	pts := d.inflight[e.sourceRef]
	d.mu.Unlock()

	buffer := e.imageBuffer
	return &Frame{
		Width:  w,
		Height: h,
		Stride: stride,
		Format: d.pixel,
		PTS:    pts,
		Pix:    unsafe.Slice((*byte)(base), stride*h),
		release: func() {
			cvPixelBufferUnlockBaseAddress(buffer, cvReadOnly)
			cfRelease(buffer)
		},
	}, nil
}

// darwinFlush waits for whatever is still inside the decoder.
func darwinFlush(h handle) ([]*Frame, error) {
	d, ok := h.(*darwinSession)
	if !ok || d == nil {
		return nil, ErrClosed
	}
	if st := vtDecompressionSessionWaitForAsynchronousFrames(d.session); st != 0 {
		return nil, status("VTDecompressionSessionWaitForAsynchronousFrames", st)
	}
	return d.collect()
}

// darwinClose invalidates the session and gives every retained object back.
func darwinClose(h handle) error {
	d, ok := h.(*darwinSession)
	if !ok || d == nil {
		return nil
	}
	if d.session != 0 {
		vtDecompressionSessionInvalidate(d.session)
		cfRelease(d.session)
		d.session = 0
	}
	unregisterSession(d)
	// Anything the callback recorded and nobody collected is released here,
	// or it leaks a pixel buffer per uncollected frame.
	d.mu.Lock()
	for _, e := range d.out {
		if e.imageBuffer != 0 {
			cfRelease(e.imageBuffer)
		}
	}
	d.out = nil
	d.mu.Unlock()
	d.releaseObjects()
	return nil
}

// releaseObjects gives back the format description and the two dictionaries.
func (d *darwinSession) releaseObjects() {
	if d.format != 0 {
		cfRelease(d.format)
		d.format = 0
	}
	for _, id := range []*objc.ID{&d.attrs, &d.spec} {
		if *id != 0 {
			id.Send(objc.Sel("release"))
			*id = 0
		}
	}
}
