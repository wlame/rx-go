package rxtypes

// ChainEntry is one log chain of a directory listing: the files of one
// rotated log (`syslog`, `syslog.1`, `syslog.2.gz`, …), found from their
// names alone. It is an element of ChainsResponse.Chains.
//
// Nothing here is read from the parts' text but whether each part is
// text and how it is compressed; the time order, the checks and the
// line numbers of a chain come from describing it.
type ChainEntry struct {
	// Path is the chain's handle: its directory joined with its name.
	// It is the active file's path, whether that file exists or not.
	Path string `json:"path" doc:"The chain's handle: its directory joined with its name, which is the active file's path whether that file exists or not. The other /v1/logs routes take it as path."`
	// Name is the active file's name.
	Name string `json:"name" doc:"The chain's name: the name of its active file (the one without a number or date), such as syslog or app.log."`
	// Parts are the file names in the provisional order, oldest first.
	Parts []string `json:"parts" nullable:"false" doc:"The names of the chain's files in the directory, in the provisional order, oldest first: by the number or date in each name in its rotation scheme's direction, then by modification time; the active file last. One file in several encodings is listed once, as the encoding rx reads. Empty for a chain of more than 10,000 parts (too_many_parts)."`
	// HasActive says whether the active file exists.
	HasActive bool `json:"has_active" doc:"Whether the active file (the one named like the chain) exists and is a part."`
	// Missing names the absent numbers of a numbered chain.
	Missing []string `json:"missing" nullable:"false" doc:"The names an absent part would have, for each number missing between the lowest expected number (0 when a .0 part exists, else 1) and the highest present one, lowest first, without a compression suffix; at most 100 of them (missing_count gives how many are missing). Named only when no more numbers are missing than parts with a number are present. Empty for a chain whose parts are all dated. A four-digit number from 1970 to 2100 where a rotation number goes (report.2023) is a year, a date, and is never missing. Empty for a chain of more than 10,000 parts."`
	// MissingCount is how many numbered parts are missing.
	MissingCount int `json:"missing_count" doc:"How many numbered parts are missing, whether missing names them all or stops at 100; 0 when missing names none."`
	// Size is the sum of the parts' file sizes.
	Size int64 `json:"size" doc:"The sum of the sizes in bytes of the files listed in parts, as stored (compressed for a compressed part)."`
	// CompressionFormats are the distinct formats among the parts.
	CompressionFormats []string `json:"compression_formats" nullable:"false" doc:"The distinct compression formats of the parts, sorted: gzip, bz2, xz, zstd (seekable zstd included). A plain part adds none."`
	// IsIndexed says whether every frozen part has a current line index.
	IsIndexed bool `json:"is_indexed" doc:"Whether every part but the active file has a current line index built from the file the listing found (the same inode and device), as is_indexed of /v1/tree tells for one file. False for a chain of more than 10,000 parts, whose indexes are not looked at."`
	// TooManyParts says whether the chain has more parts than are read
	// as one text.
	TooManyParts bool `json:"too_many_parts" doc:"Whether the chain has more than 10,000 parts. Such a chain is not read as one text: parts and missing are empty, missing_count is 0, is_indexed is false, and GET /v1/logs/chain answers it invalid with the reason too_many_parts."`
}

// ChainsResponse is the body of GET /v1/logs/chains and of each
// directory `rx logs list --json` prints: the log chains of one
// directory, sorted by name, case-insensitive.
type ChainsResponse struct {
	// Path is the listed directory, absolute.
	Path string `json:"path" doc:"The listed directory, as an absolute path."`
	// Chains are the directory's chains.
	Chains []ChainEntry `json:"chains" nullable:"false" doc:"The directory's log chains, sorted by name, case-insensitive. Empty when it has none."`
}

// The states of a log chain: ChainResponse.State.
const (
	// ChainStatePending: some frozen part has no current line index, or
	// the active part's first timestamp is not known yet. Each part can
	// be read on its own; global line numbers and times wait.
	ChainStatePending = "pending"
	// ChainStateReady: every part is known and every check passed. The
	// chain reads as one text.
	ChainStateReady = "ready"
	// ChainStateInvalid: a check failed; the reasons say which.
	ChainStateInvalid = "invalid"
)

// The reasons a log chain is invalid: ChainReason.Code.
const (
	// ChainReasonNoTimestamps: a part holds lines and no timestamp rx
	// recognizes, so its place in time is not known.
	ChainReasonNoTimestamps = "no_timestamps"
	// ChainReasonOverlap: a part's highest timestamp is after the next
	// part's first by more than RX_CHAIN_OVERLAP_SECONDS.
	ChainReasonOverlap = "overlap"
	// ChainReasonActiveNotLast: the active file starts before a frozen
	// part, which should be older.
	ChainReasonActiveNotLast = "active_not_last"
	// ChainReasonUnreadable: a part cannot be read.
	ChainReasonUnreadable = "unreadable"
	// ChainReasonTooManyParts: the chain has more parts than are read as
	// one text (10,000).
	ChainReasonTooManyParts = "too_many_parts"
)

// ChainResponse is the description of one log chain: the body of
// GET /v1/logs/chain and of each chain `rx logs show --json` prints.
//
// Its parts are in the chain's order: by their first timestamps once
// every part is known, in the provisional order of their names before.
// Times are UTC instants in milliseconds since the Unix epoch, read
// the way GET /v1/time-range reads one file (file_tz included), so the
// parts of a chain share one axis.
type ChainResponse struct {
	Path            string             `json:"path" doc:"The chain's handle: its directory joined with its name, which is the active file's path whether that file exists or not."`
	Name            string             `json:"name" doc:"The chain's name: the name of its active file."`
	State           string             `json:"state" enum:"pending,ready,invalid" doc:"pending: some frozen part has no current line index, or the active file's first timestamp is not known yet; each part can be read on its own, and global line numbers and times wait. ready: every part is known and every check passed; the chain reads as one text. invalid: a check failed, and reasons says which."`
	Reasons         []ChainReason      `json:"reasons" nullable:"false" doc:"Why the chain is invalid, one entry per failed check; empty unless state is invalid."`
	Fingerprint     string             `json:"fingerprint" doc:"16 hex digits that change when the chain's files change: a frozen part renamed, compressed, deleted, added or written to, or the active file replaced. The active file growing does not change it. Send it back as fingerprint to learn, by a 409, that the files changed."`
	Parts           []ChainPart        `json:"parts" nullable:"false" doc:"The chain's parts in its order: by first timestamp once the chain is ready (an empty part keeps its place among the others), by the number or date in their names before (as GET /v1/logs/chains lists them). One file in several encodings is one part, the encoding rx reads. Empty for a chain of more than 10,000 parts, which is invalid with the reason too_many_parts."`
	Missing         []string           `json:"missing" nullable:"false" doc:"The names absent numbered parts would have, as GET /v1/logs/chains gives them: at most 100, lowest first."`
	MissingCount    int                `json:"missing_count" doc:"How many numbered parts are missing, as GET /v1/logs/chains counts them."`
	Gaps            []ChainGap         `json:"gaps" nullable:"false" doc:"The stretches of time no part covers, in order, in a ready chain of four parts with lines or more: where the time from a part's highest timestamp to the next part's first is more than 1.5 times the median distance between the first timestamps of neighboring parts. Empty otherwise."`
	FirstMs         *int64             `json:"first_ms" doc:"The chain's first timestamp: the first of its first part with lines, as a UTC instant in ms. Null unless the chain is ready."`
	LastMs          *int64             `json:"last_ms" doc:"The chain's last timestamp: the last of its last part with lines, as a UTC instant in ms. Null unless the chain is ready, and when that part's last timestamp is not known (an active file whose last timestamped line is more than 16 MiB from its end)."`
	FrozenLineCount *int64             `json:"frozen_line_count" doc:"The lines of every part but the active file. Null unless the chain is ready."`
	LineCount       *int64             `json:"line_count" doc:"The chain's lines, the active file's included, when its count is known (from its current line index, or read by rx logs show). Null unless the chain is ready."`
	IndexBuild      *SamplesIndexBuild `json:"index_build" doc:"The task building the indexes the chain needs, when one runs or has just ended; null otherwise."`
	CLICommand      string             `json:"cli_command" doc:"The rx command that gives this answer."`
}

// ChainPart is one part of a described chain: an element of
// ChainResponse.Parts.
type ChainPart struct {
	Name              string             `json:"name" doc:"The part's file name in the chain's directory."`
	Path              string             `json:"path" doc:"The part's path: the chain's directory joined with name. The single-file routes take it."`
	IsActive          bool               `json:"is_active" doc:"Whether this is the active file, the one named like the chain, which may grow."`
	Key               *string            `json:"key" doc:"The number or date in the part's name as the name writes it (3, 20261001-1790812801, 2026-10-01.3); null for the active file."`
	CompressionFormat *string            `json:"compression_format" doc:"How the part is compressed, by its bytes: gzip, bz2, xz or zstd (seekable zstd included); null for a plain file."`
	Size              int64              `json:"size" doc:"The part's size in bytes as stored, from the listing."`
	ModifiedAt        string             `json:"modified_at" format:"date-time" doc:"The part's modification time, RFC 3339 in UTC with six fractional digits, from the listing."`
	IsIndexed         bool               `json:"is_indexed" doc:"Whether a current line index of the part is stored."`
	LineCount         *int64             `json:"line_count" doc:"The part's lines, as rx samples numbers them (a last line without a newline is a line); null when not known: a frozen part without a current index, or the active file without one, unless rx logs show read it."`
	FirstMs           *int64             `json:"first_ms" doc:"The part's first timestamp (of the first line, in file order, that has one), as a UTC instant in ms; null when not known or when the part has none."`
	LastMs            *int64             `json:"last_ms" doc:"The part's last timestamp (of the last line, in file order, that has one), as a UTC instant in ms; null when not known or when the part has none."`
	MaxMs             *int64             `json:"max_ms" doc:"The part's highest timestamp, as a UTC instant in ms, from its line index (stored, or read by rx logs show); null without one (the active file usually) and for a part without timestamps. An upper bound when max_is_bound is true."`
	MaxIsBound        bool               `json:"max_is_bound" doc:"Whether max_ms is an upper bound of the part's highest timestamp rather than the timestamp: under file_tz, in a part whose lines write several zone offsets, where the line with the latest wall clock is not known from the index."`
	GlobalStart       *int64             `json:"global_start" doc:"The global line number of the part's first line: 1 plus the lines of the parts before it, so line L of the part is global line global_start + L - 1. An empty part's is the next part's. Null unless the chain is ready."`
	TimeFormat        *SamplesTimeFormat `json:"time_format" doc:"The part's timestamp format, as GET /v1/samples gives it; null when not known or when the part has none."`
	Duplicates        []string           `json:"duplicates" nullable:"false" doc:"The names of the part's other encodings (the same generation compressed another way), which rx does not read."`
}

// ChainReason is one reason a chain is invalid: an element of
// ChainResponse.Reasons.
type ChainReason struct {
	Code      string   `json:"code" enum:"no_timestamps,overlap,active_not_last,unreadable,too_many_parts" doc:"The check that failed. no_timestamps: a part has lines and no timestamp rx recognizes. overlap: a part's highest timestamp is after the next part's first by more than RX_CHAIN_OVERLAP_SECONDS. active_not_last: the active file starts before a frozen part. unreadable: a part cannot be read. too_many_parts: the chain has more than 10,000 parts."`
	Parts     []string `json:"parts" nullable:"false" doc:"The names of the parts the check names, in the chain's order: the part for no_timestamps and unreadable, the two neighbors for overlap, the active file and the parts after it for active_not_last; empty for too_many_parts."`
	Message   string   `json:"message" doc:"The reason in words."`
	OverlapMs *int64   `json:"overlap_ms" doc:"For overlap, how far in ms the first part's highest timestamp is after the second's first timestamp; null for the other codes."`
}

// ChainGap is a stretch of time no part of a chain covers: an element of
// ChainResponse.Gaps.
type ChainGap struct {
	After  string `json:"after" doc:"The part before the gap."`
	Before string `json:"before" doc:"The part after the gap."`
	FromMs int64  `json:"from_ms" doc:"Where the gap starts: the highest timestamp of the part before it, as a UTC instant in ms."`
	ToMs   int64  `json:"to_ms" doc:"Where the gap ends: the first timestamp of the part after it, as a UTC instant in ms."`
}

// ChainTimeRange is the time range of one log chain, as
// `rx logs time-range --json` prints it: the chain's first and last
// timestamp, and how its first part with timestamps writes them.
type ChainTimeRange struct {
	Path        string  `json:"path" doc:"The chain's handle."`
	Name        string  `json:"name" doc:"The chain's name."`
	State       string  `json:"state" enum:"pending,ready,invalid" doc:"The chain's state, as its description gives it."`
	Format      *string `json:"format" doc:"The timestamp format of the chain's first part with timestamps; null when no part has one."`
	FirstMs     *int64  `json:"first_ms" doc:"The chain's first timestamp as a UTC instant in ms; null unless the chain is ready."`
	LastMs      *int64  `json:"last_ms" doc:"The chain's last timestamp as a UTC instant in ms; null unless the chain is ready, and when it is not known."`
	DisplayZone string  `json:"display_zone" doc:"The zone the times are shown in: the file zone when one is given, else the zone the first part with timestamps is read in (RX_LOG_TZ for timestamps without a zone, UTC for timestamps with one)."`
	CLICommand  string  `json:"cli_command" doc:"The rx command that gives this answer."`
}
