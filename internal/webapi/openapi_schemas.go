package webapi

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Schemas huma cannot derive from the Go types
// ============================================================================
//
// huma builds the OpenAPI schema of a response by reflecting over its Go
// type. Two wire types do not reflect into what they put on the wire:
//
//   - rxtypes.LineIndexEntry is a struct that marshals itself as a JSON
//     array ([line, offset] or [line, offset, frame]), so reflection
//     would describe an object with Go field names.
//   - rxtypes.TaskResult is an `any` that holds one of three result
//     structs, or nil, so reflection would describe "anything".
//
// pkg/rxtypes depends on the standard library only, so the schemas are
// written here instead. huma's registry lets a type be replaced by an
// alias type for schema purposes (RegisterTypeAlias); each alias below
// implements huma.SchemaProvider, the interface huma calls instead of
// reflecting. The aliases are registered before any operation, because
// huma builds the schemas when an operation is registered.

// registerWireSchemas installs the schema aliases on api's registry.
func registerWireSchemas(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	registry.RegisterTypeAlias(reflect.TypeOf(rxtypes.LineIndexEntry{}), reflect.TypeOf(lineIndexEntrySchema{}))
	// TaskResult is an interface type, so reflect.TypeOf on a value of
	// it would see the dynamic value instead; taking the pointer type's
	// element names the interface type itself.
	registry.RegisterTypeAlias(reflect.TypeOf((*rxtypes.TaskResult)(nil)).Elem(), reflect.TypeOf(taskResultSchema{}))
}

// lineIndexEntrySchemaName is the component the entries of every
// line_index refer to.
const lineIndexEntrySchemaName = "LineIndexEntry"

// lineIndexEntrySchema stands in for rxtypes.LineIndexEntry in the
// registry. It has no fields; only its Schema method matters.
type lineIndexEntrySchema struct{}

// Schema implements huma.SchemaProvider. It registers the named
// component on first use and returns a reference to it, so every
// line_index in the document points at one definition.
//
// The component is oneOf two tuples: [line_number, byte_offset] for a
// plain or gzip index, and [line_number, byte_offset, frame_index] for a
// seekable-zstd one. Each tuple is an array of integers of a fixed
// length, which every validator checks (huma's included); prefixItems,
// carried as an extension because huma.Schema has no field for it, names
// and bounds each position for validators and generators that read it.
// The two lengths make the branches exclusive, as oneOf requires.
func (lineIndexEntrySchema) Schema(r huma.Registry) *huma.Schema {
	if _, done := r.Map()[lineIndexEntrySchemaName]; !done {
		component := &huma.Schema{
			Description: "A line-index checkpoint: the 1-based line number and the byte offset " +
				"where that line starts in the file's text. An entry of a seekable-zstd index " +
				"has a third element, the 0-based index of the frame holding the line.",
			OneOf: []*huma.Schema{
				lineIndexTuple(lineNumberItem(), byteOffsetItem()),
				lineIndexTuple(lineNumberItem(), byteOffsetItem(), frameIndexItem()),
			},
		}
		component.PrecomputeMessages()
		r.Map()[lineIndexEntrySchemaName] = component
	}
	return &huma.Schema{Ref: schemaRefPrefix + lineIndexEntrySchemaName}
}

// schemaRefPrefix is where huma's default registry keeps named schemas.
const schemaRefPrefix = "#/components/schemas/"

// lineIndexTuple is an array schema of exactly len(positions) integers.
func lineIndexTuple(positions ...*huma.Schema) *huma.Schema {
	length := len(positions)
	minimum := 0.0
	tuple := &huma.Schema{
		Type:       huma.TypeArray,
		Items:      &huma.Schema{Type: huma.TypeInteger, Format: "int64", Minimum: &minimum},
		MinItems:   &length,
		MaxItems:   &length,
		Extensions: map[string]any{"prefixItems": positions},
	}
	tuple.Items.PrecomputeMessages()
	tuple.PrecomputeMessages()
	return tuple
}

func lineNumberItem() *huma.Schema {
	return integerItem(1, "Line number, 1-based.")
}

func byteOffsetItem() *huma.Schema {
	return integerItem(0, "Byte offset where the line starts, in the file's text "+
		"(the decompressed stream for a compressed file).")
}

func frameIndexItem() *huma.Schema {
	return integerItem(0, "Index of the seekable-zstd frame holding the line, 0-based.")
}

// integerItem is an int64 schema with a lower bound and a description.
func integerItem(minimum float64, description string) *huma.Schema {
	return &huma.Schema{Type: huma.TypeInteger, Format: "int64", Minimum: &minimum, Description: description}
}

// taskResultSchema stands in for rxtypes.TaskResult in the registry.
type taskResultSchema struct{}

// Schema implements huma.SchemaProvider: the result of a task is an
// IndexTaskResult, a CompressTaskResult, a ChainIndexTaskResult, or
// null until the task completes.
//
// It is anyOf rather than oneOf because huma's validator accepts any
// value for a {"type": "null"} branch, so with oneOf a real result would
// match two branches and fail. The result objects cannot be confused
// anyway: each forbids the properties only the others have.
func (taskResultSchema) Schema(r huma.Registry) *huma.Schema {
	schema := &huma.Schema{
		Description: "The task's result once it completes: IndexTaskResult for an index task, " +
			"CompressTaskResult for a compress task, ChainIndexTaskResult for a chain_index task. " +
			"Null until then.",
		AnyOf: []*huma.Schema{
			r.Schema(reflect.TypeOf(rxtypes.IndexTaskResult{}), true, ""),
			r.Schema(reflect.TypeOf(rxtypes.CompressTaskResult{}), true, ""),
			r.Schema(reflect.TypeOf(rxtypes.ChainIndexTaskResult{}), true, ""),
			{Type: "null"},
		},
	}
	return schema
}
