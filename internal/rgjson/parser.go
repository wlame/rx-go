package rgjson

import (
	"bufio"
	"fmt"
	"io"

	json "github.com/goccy/go-json"
)

// DefaultMaxLineSizeKB is the fallback scanner buffer size when no config is provided.
const DefaultMaxLineSizeKB = 8

// Parser reads rg --json output line by line from an io.Reader and yields typed
// RgMessage values. It uses a bufio.Reader to handle arbitrarily long lines
// without the permanent failure mode of bufio.Scanner.
type Parser struct {
	reader  *bufio.Reader
	bufSize int
}

// NewParser creates a Parser that reads from r. maxLineSizeKB controls the soft
// limit for line size in kilobytes — lines exceeding this are skipped rather than
// causing a fatal error. Pass 0 to use DefaultMaxLineSizeKB.
func NewParser(r io.Reader, maxLineSizeKB int) *Parser {
	if maxLineSizeKB <= 0 {
		maxLineSizeKB = DefaultMaxLineSizeKB
	}
	// Use a generous read buffer — rg JSON lines for matches on very long
	// lines (e.g., minified JSON, SQL queries) can easily exceed 8KB.
	// We use 1MB as the hard limit to avoid unbounded memory usage.
	const hardMaxBytes = 1024 * 1024
	bufSize := maxLineSizeKB * 1024
	if bufSize < hardMaxBytes {
		bufSize = hardMaxBytes
	}

	return &Parser{
		reader:  bufio.NewReaderSize(r, 64*1024),
		bufSize: bufSize,
	}
}

// Next reads the next rg JSON event from the stream. It returns (message, nil)
// on success, (nil, nil) when the stream is exhausted, and (nil, err) on failure.
// Malformed or oversized lines are silently skipped (matching Python's behavior).
func (p *Parser) Next() (*RgMessage, error) {
	for {
		line, err := p.readLine()
		if err != nil {
			if err == io.EOF {
				return nil, nil // stream exhausted
			}
			return nil, fmt.Errorf("rgjson reader: %w", err)
		}

		if len(line) == 0 {
			continue
		}

		msg, parseErr := parseLine(line)
		if parseErr != nil {
			// Skip malformed lines — ripgrep occasionally emits partial writes
			// or non-JSON status messages.
			continue
		}
		return msg, nil
	}
}

// readLine reads a complete line from the buffered reader, assembling fragments
// if the line exceeds the internal buffer. Lines exceeding bufSize are discarded
// to prevent unbounded memory growth, but reading continues (no permanent failure).
func (p *Parser) readLine() ([]byte, error) {
	var full []byte
	for {
		fragment, isPrefix, err := p.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if full == nil && !isPrefix {
			// Common fast path: entire line fit in one read.
			return fragment, nil
		}
		full = append(full, fragment...)
		if !isPrefix {
			break
		}
		// Line is longer than the buffer — keep assembling, but bail if it
		// exceeds our hard limit to avoid unbounded memory usage.
		if len(full) > p.bufSize {
			// Drain the remainder of this oversized line so we don't get
			// stuck reading its tail as the next "line".
			for isPrefix {
				_, isPrefix, err = p.reader.ReadLine()
				if err != nil {
					return nil, err
				}
			}
			// Return empty so the caller skips this line.
			return nil, nil
		}
	}
	return full, nil
}

// ParseAll reads all remaining events from the stream and returns them in order.
// Malformed lines are silently skipped.
func (p *Parser) ParseAll() ([]RgMessage, error) {
	var messages []RgMessage
	for {
		msg, err := p.Next()
		if err != nil {
			return messages, err
		}
		if msg == nil {
			return messages, nil
		}
		messages = append(messages, *msg)
	}
}

// parseLine decodes a single JSON line into an RgMessage, populating the correct
// typed field based on the "type" discriminator.
func parseLine(line []byte) (*RgMessage, error) {
	// First pass: peek at the "type" field to determine which payload struct to use.
	var envelope struct {
		Type MessageType            `json:"type"`
		Data json.RawMessage        `json:"data"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}

	msg := &RgMessage{Type: envelope.Type}

	switch envelope.Type {
	case TypeBegin:
		var d RgBegin
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, fmt.Errorf("unmarshal begin data: %w", err)
		}
		msg.Begin = &d

	case TypeMatch:
		var d RgMatch
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, fmt.Errorf("unmarshal match data: %w", err)
		}
		msg.Match = &d

	case TypeContext:
		var d RgContext
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, fmt.Errorf("unmarshal context data: %w", err)
		}
		msg.Context = &d

	case TypeEnd:
		var d RgEnd
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, fmt.Errorf("unmarshal end data: %w", err)
		}
		msg.End = &d

	case TypeSummary:
		var d RgSummary
		if err := json.Unmarshal(envelope.Data, &d); err != nil {
			return nil, fmt.Errorf("unmarshal summary data: %w", err)
		}
		msg.Summary = &d

	default:
		// Unknown event type — skip silently.
		return nil, fmt.Errorf("unknown event type: %q", envelope.Type)
	}

	return msg, nil
}
