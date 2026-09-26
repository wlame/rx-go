package clicommand

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// TestCompressRequestDefaults_MatchCLIFlagDefaults pins the defaults of the
// POST /v1/compress body to the defaults of `rx compress`. A request that
// leaves a field out must compress the way the CLI does without the flag,
// so the cli_command a task reports reproduces what the task did.
//
// The HTTP defaults live in the `default:"..."` struct tags huma reads
// (and publishes in the OpenAPI document); the CLI defaults live in the
// cobra flag definitions. This test is what keeps the two in step.
func TestCompressRequestDefaults_MatchCLIFlagDefaults(t *testing.T) {
	cmd := NewCompressCommand(io.Discard)
	bodyType := reflect.TypeOf(rxtypes.CompressRequest{})

	cases := []struct {
		jsonName string
		flagName string
	}{
		{"frame_size", "frame-size"},
		{"compression_level", "level"},
		{"build_index", "build-index"},
		{"force", "force"},
	}
	for _, tc := range cases {
		t.Run(tc.jsonName, func(t *testing.T) {
			flag := cmd.Flags().Lookup(tc.flagName)
			if flag == nil {
				t.Fatalf("rx compress has no --%s flag", tc.flagName)
			}
			field, ok := fieldByJSONName(bodyType, tc.jsonName)
			if !ok {
				t.Fatalf("CompressRequest has no %q field", tc.jsonName)
			}
			got, hasDefault := field.Tag.Lookup("default")
			if !hasDefault {
				t.Fatalf("CompressRequest.%s has no default tag; the CLI default is %q",
					field.Name, flag.DefValue)
			}
			if got != flag.DefValue {
				t.Errorf("CompressRequest.%s default = %q, rx compress --%s default = %q",
					field.Name, got, tc.flagName, flag.DefValue)
			}
		})
	}
}

// fieldByJSONName finds the struct field whose json tag names jsonName.
func fieldByJSONName(t reflect.Type, jsonName string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == jsonName {
			return field, true
		}
	}
	return reflect.StructField{}, false
}
