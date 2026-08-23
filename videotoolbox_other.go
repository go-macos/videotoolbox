// Copyright (c) the go-macos/videotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package videotoolbox

// On every non-darwin platform the seams answer [ErrUnsupported] rather than
// being nil, so a consumer that cross-compiles for Linux or Windows gets a clean
// error from the same API instead of a nil-func panic.
func init() {
	newSession = func(Config, [][]byte, Options) (handle, error) { return nil, ErrUnsupported }
	decodeSample = func(handle, Sample) ([]*Frame, error) { return nil, ErrUnsupported }
	flushSession = func(handle) ([]*Frame, error) { return nil, ErrUnsupported }
	closeSession = func(handle) error { return nil }
}
