package rxtypes

// TimeRangeResponse is the time range of one file: the body of
// GET /v1/time-range and of each file `rx time-range --json` prints.
//
// FirstMs and LastMs are UTC instants in milliseconds since the Unix
// epoch, so the ranges of several files share one axis: a file whose
// timestamps carry no zone has its wall clock read in RX_LOG_TZ, as
// SamplesResponse.LineTimestamps reads it, and under a request's
// file_tz every one is the wall clock its line writes, read in that
// zone. DisplayZone and Example say
// how the file itself writes its times, so a client can show an
// instant the way the file's lines show it.
//
// When no timestamp format is recognized, Format and every member after
// it but Source and CLICommand are null.
type TimeRangeResponse struct {
	Path string `json:"path" doc:"The file, as the request named it."`
	// Format is the timestamp family of the file's lines.
	Format *string `json:"format" doc:"The timestamp format of the lines: iso, clf, ctime, syslog, slash, dotted or epoch; null when none is recognized in the first mebibyte of the text, and then has_zone, day_first, display_zone, example, first_ms and last_ms are null too."`
	// HasZone says whether the file's timestamps carry zones.
	HasZone *bool `json:"has_zone" doc:"Whether most timestamps carry a zone."`
	// DayFirst is the day/month order of a slash date, nil for every
	// other family.
	DayFirst *bool `json:"day_first" doc:"For the slash format, whether the day comes before the month; null for every other format."`
	// DisplayZone is the zone the file's own lines show their times in.
	DisplayZone *string `json:"display_zone" doc:"The zone to show this file's times in so that they read as its lines do: RX_LOG_TZ (UTC, an IANA name or ±HH:MM) for a file whose timestamps carry no zone; the offset of the first timestamp (±HH:MM) for one whose timestamps do; null when that offset is unknown. The request's file_tz, whatever the file, when it names one."`
	// Example is the first timestamp as its line writes it.
	Example *string `json:"example" doc:"The first timestamp as its line writes it, such as 2025-12-10 07:00:04.574: printable ASCII, any other byte written as \\xHH, at most 64 bytes; null when no line has a timestamp."`
	// FirstMs and LastMs are the first and the last line, in file
	// order, with a timestamp of their own.
	FirstMs *int64 `json:"first_ms" doc:"The timestamp of the first line that has one, as a UTC instant in ms; null when unknown. Under file_tz, the wall clock the line writes read in that zone."`
	LastMs  *int64 `json:"last_ms" doc:"The timestamp of the last line that has one, as a UTC instant in ms; null when unknown: source none, or no timestamped line within the last 16 MiB of the text. Under file_tz, the wall clock the line writes read in that zone."`
	// Source says how the range was found.
	Source     string `json:"source" enum:"index,scan,none" doc:"How the range was found: index (the file's line index; nothing of the file read, except under file_tz the last timestamped line of a file whose timestamps carry zones whose offset changes too often for the index to record), scan (the head of the text and a read back from its end, at most 16 MiB), none (a gzip, bzip2, xz or plain zstd file without an index, or under file_tz one whose timestamps carry zones and whose index records no offsets: first_ms and last_ms are null)."`
	CLICommand string `json:"cli_command" doc:"The rx command that gives this answer."`
}
