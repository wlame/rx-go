# History

`rx` was originally written in Python — see
[`wlame/rx-python`](https://github.com/wlame/rx-python) for the original
implementation.

It was later reimplemented in Go to reduce runtime overhead (no
interpreter, no process-pool startup cost, no per-decompressor
subprocesses) and to ship as a single static binary with no runtime
dependencies beyond `ripgrep`.
