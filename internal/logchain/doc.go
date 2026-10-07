// Package logchain finds log chains: the files of one rotated log in
// one directory (`syslog`, `syslog.1`, `syslog.2.gz`,
// `syslog-20261001-1790812801.gz`), which rx reads and searches as one
// text.
//
// Membership comes from names only. A table of name templates
// (Templates) says which names are rotated parts and which chain each
// belongs to; Group applies it to one directory listing, and Resolve
// finds the one chain a handle names. Nothing here reads a part's text
// beyond the text check every listing makes (filekind), and only for
// names that can belong to a chain.
//
// Words used in this package:
//
//   - chain: the files of one rotated log in one directory.
//   - part: one file of a chain.
//   - active part: the part whose name has no number or date (`syslog`).
//     It may be absent. Every other part is frozen.
//   - chain name: the active part's file name, whether it exists or not.
//   - handle: the chain's directory joined with its name, which is the
//     active file's path.
//   - key: the number or date in a part's name. It gives the
//     provisional order, oldest first, and finds missing numbers. The
//     real order comes from the parts' timestamps, later.
//
// In Go code of other packages "chain" alone also means an HTTP
// middleware chain; this package's name says which one is meant.
package logchain
