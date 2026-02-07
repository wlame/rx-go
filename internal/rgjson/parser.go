package rgjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Parser parses ripgrep JSON output
type Parser struct {
	scanner *bufio.Scanner
}

// NewParser creates a new parser for ripgrep JSON output
func NewParser(r io.Reader) *Parser {
	return &Parser{
		scanner: bufio.NewScanner(r),
	}
}

// Next reads the next event from the stream
// Returns nil when no more events are available
func (p *Parser) Next() (*Event, error) {
	if !p.scanner.Scan() {
		if err := p.scanner.Err(); err != nil {
			return nil, fmt.Errorf("scanner error: %w", err)
		}
		return nil, io.EOF
	}

	line := p.scanner.Bytes()
	var event Event

	if err := json.Unmarshal(line, &event); err != nil {
		return nil, fmt.Errorf("failed to parse JSON event: %w", err)
	}

	return &event, nil
}

// ParseAll reads all events from the stream
func (p *Parser) ParseAll() ([]Event, error) {
	var events []Event

	for {
		event, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		events = append(events, *event)
	}

	return events, nil
}

// ParseMatches reads all match events from the stream
func (p *Parser) ParseMatches() ([]Event, error) {
	var matches []Event

	for {
		event, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if event.Type == EventTypeMatch {
			matches = append(matches, *event)
		}
	}

	return matches, nil
}
