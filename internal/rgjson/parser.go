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
// RgMessage values. It uses a bufio.Scanner with a configurable buffer size to
// handle long lines.
type Parser struct {
	scanner *bufio.Scanner
}

// NewParser creates a Parser that reads from r. maxLineSizeKB controls the scanner
// buffer ceiling in kilobytes. Pass 0 to use DefaultMaxLineSizeKB.
func NewParser(r io.Reader, maxLineSizeKB int) *Parser {
	if maxLineSizeKB <= 0 {
		maxLineSizeKB = DefaultMaxLineSizeKB
	}
	bufSize := maxLineSizeKB * 1024

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufSize), bufSize)

	return &Parser{scanner: scanner}
}

// Next reads the next rg JSON event from the stream. It returns (message, nil)
// on success, (nil, nil) when the stream is exhausted, and (nil, err) on failure.
// Malformed lines are silently skipped (matching Python's behavior).
func (p *Parser) Next() (*RgMessage, error) {
	for p.scanner.Scan() {
		line := p.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		msg, err := parseLine(line)
		if err != nil {
			// Skip malformed lines — ripgrep occasionally emits partial writes
			// or non-JSON status messages.
			continue
		}
		return msg, nil
	}

	if err := p.scanner.Err(); err != nil {
		return nil, fmt.Errorf("rgjson scanner: %w", err)
	}
	return nil, nil // stream exhausted
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
