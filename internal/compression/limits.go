package compression

import "errors"

// WindowLimit is the most memory rx lets one decoder hold for the text
// it has already decoded: 128 MiB. It bounds the window of a zstd frame,
// a seekable zstd frame that is decoded whole, and the dictionary of an
// xz block. 128 MiB is the window `zstd --long` and `zstd --ultra -22`
// write and the most the reference zstd tool decodes without an
// explicit `--long=N` or `--memory=`, and above the 64 MiB dictionary
// `xz -9` writes.
//
// A file declares these sizes itself, so without a limit a file of a
// few bytes could make one reader reserve gigabytes.
const WindowLimit = 128 << 20

// ErrTooLargeToDecode is wrapped by every refusal to decode a file
// because it declares more than a decoder's limit: ErrWindowTooLarge
// for zstd, ErrDictionaryTooLarge for xz.
var ErrTooLargeToDecode = errors.New("decompressing it needs more memory than rx allows")
