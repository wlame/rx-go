package main

import (
	"reflect"
	"testing"
)

// TestPreprocessArgs covers the "default subcommand is trace" logic.
// It's pure, so no process-level side effects or cobra orchestration
// are needed.
func TestPreprocessArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty args → untouched",
			in:   []string{},
			want: []string{},
		},
		{
			name: "--help passes through",
			in:   []string{"--help"},
			want: []string{"--help"},
		},
		{
			name: "-h passes through",
			in:   []string{"-h"},
			want: []string{"-h"},
		},
		{
			name: "--version passes through",
			in:   []string{"--version"},
			want: []string{"--version"},
		},
		{
			name: "trace subcommand passes through",
			in:   []string{"trace", "pattern", "file.log"},
			want: []string{"trace", "pattern", "file.log"},
		},
		{
			name: "index subcommand passes through",
			in:   []string{"index", "/var/log/a.log"},
			want: []string{"index", "/var/log/a.log"},
		},
		{
			name: "serve passes through",
			in:   []string{"serve", "--port=8080"},
			want: []string{"serve", "--port=8080"},
		},
		{
			name: "compress passes through",
			in:   []string{"compress", "/var/log/a.log"},
			want: []string{"compress", "/var/log/a.log"},
		},
		{
			name: "samples passes through",
			in:   []string{"samples", "/var/log/a.log"},
			want: []string{"samples", "/var/log/a.log"},
		},
		{
			name: "help passes through",
			in:   []string{"help"},
			want: []string{"help"},
		},
		{
			name: "completion passes through",
			in:   []string{"completion", "zsh"},
			want: []string{"completion", "zsh"},
		},
		{
			name: "positional → trace prepended",
			in:   []string{"error", "/var/log/a.log"},
			want: []string{"trace", "error", "/var/log/a.log"},
		},
		{
			name: "regex-looking positional → trace prepended",
			in:   []string{"error.*failed", "/var/log/a.log"},
			want: []string{"trace", "error.*failed", "/var/log/a.log"},
		},
		{
			name: "check (dropped command) → trace prepended",
			in:   []string{"check", "regex"},
			want: []string{"trace", "check", "regex"},
		},
		{
			name: "flag-first (--json) → trace prepended",
			in:   []string{"--json", "error", "file.log"},
			want: []string{"trace", "--json", "error", "file.log"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("\n got:  %v\n want: %v", got, tc.want)
			}
		})
	}
}

// TestPreprocessArgs_TruthTable is the borrowed-from-another-rx-go parity
// table. It pins down the full truth table of routing decisions so that
// the planned structural refactor (map → []string + shouldRouteToTrace
// predicate) preserves behavior exactly. This test MUST pass both before
// and after the refactor.
func TestPreprocessArgs_TruthTable(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"bare pattern", []string{"error", "/var/log"}, []string{"trace", "error", "/var/log"}},
		{"subcommand trace", []string{"trace", "pat"}, []string{"trace", "pat"}},
		{"subcommand index", []string{"index", "/var/log"}, []string{"index", "/var/log"}},
		{"subcommand samples", []string{"samples", "/var/log"}, []string{"samples", "/var/log"}},
		{"subcommand compress", []string{"compress", "/var/log"}, []string{"compress", "/var/log"}},
		{"subcommand serve", []string{"serve", "--port=7777"}, []string{"serve", "--port=7777"}},
		{"help long", []string{"--help"}, []string{"--help"}},
		{"help short", []string{"-h"}, []string{"-h"}},
		{"version long", []string{"--version"}, []string{"--version"}},
		{"cobra help subcommand", []string{"help"}, []string{"help"}},
		{"cobra completion subcommand", []string{"completion", "zsh"}, []string{"completion", "zsh"}},
		{"flag then pattern", []string{"--json", "error", "/var/log"}, []string{"trace", "--json", "error", "/var/log"}},
		{"flag then subcommand", []string{"--json", "index", "/var/log"}, []string{"--json", "index", "/var/log"}},
		{"subcommand then flag", []string{"index", "--json"}, []string{"index", "--json"}},
		{"only flag", []string{"--json"}, []string{"trace", "--json"}},
		{"short flag then pattern", []string{"-j", "err", "/var/log"}, []string{"trace", "-j", "err", "/var/log"}},
		{"regex positional", []string{"error.*failed", "a.log"}, []string{"trace", "error.*failed", "a.log"}},
		{"unknown first word (dropped command)", []string{"check", "pattern"}, []string{"trace", "check", "pattern"}},
		{"multiple leading flags then pattern", []string{"--json", "--no-color", "err", "a.log"}, []string{"trace", "--json", "--no-color", "err", "a.log"}},
		{"multiple leading flags then subcommand", []string{"--json", "--no-color", "serve"}, []string{"--json", "--no-color", "serve"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preprocessArgs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("preprocessArgs(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
