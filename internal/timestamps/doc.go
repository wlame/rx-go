// Package timestamps recognizes the timestamp at the start of a log line.
//
// The package is pure: it reads no file, no environment variable and no
// clock. The caller passes in every outside fact (a file's modification
// time, the zones a query is read in), so the same bytes always give the
// same answer, whichever process computes it. That is what lets an index
// built yesterday and a cold scan today agree on every value.
//
// # The pieces
//
//   - A [Family] is one way of writing a date and time (ISO 8601, the
//     Apache access-log form, syslog and so on). Each family is a
//     hand-written byte matcher: no regular expression and no time.Parse
//     run per line.
//   - A [Format] is what [Detect] decides for one file from the first
//     [SampleBytes] of its text: the family, whether the timestamp sits at
//     the start of the line, the day/month order of a slash date and
//     whether most timestamps carry a zone. A Format serializes to JSON so
//     an index can store it.
//   - A [Parser] is built once per file from its Format and modification
//     time. [Parser.Own] returns the timestamp written on one line. It
//     looks at no more than [WindowBytes] of the line and allocates
//     nothing, because it runs on every line of every index build.
//     [Parser.Locate] also says where the timestamp is written, and
//     [Parser.Text] returns it as written, escaped for display.
//   - [ParseQuery] reads a timestamp query such as `2026-10-06T12:34`,
//     `14:33:12..14:35` or `..1759754096`. Some endpoints cannot become a
//     number from the text alone (a time with no date needs the file's
//     date; a time with no zone needs a zone), so [Resolve] finishes the
//     job with the context the caller supplies.
//
// # Values
//
// A timestamp is an int64 count of milliseconds since the Unix epoch.
// Digits past the millisecond are dropped. Every value of one file is
// in one frame, which its [Format] decides. In a file whose timestamps
// carry a zone (`Z`, `+02:00`, `UTC`, …) a value is the UTC instant it
// names, and a line without a zone is read as UTC. In a file whose
// timestamps carry none, a value is the wall-clock reading as if it were
// UTC, the "file frame", and a line that does carry a zone keeps the
// wall clock it shows. The zone such a file was written in is only known
// to the caller, which applies it when it compares a query with the file
// (see [ResolveContext]). A query that carries a zone is always the UTC
// instant it names.
package timestamps
