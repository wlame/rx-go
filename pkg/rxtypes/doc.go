// Package rxtypes defines the JSON wire types shared between the rx-go
// server, CLI, and external consumers such as the rx-viewer SPA.
//
// Every struct in this package is designed to marshal to byte-compatible
// JSON with the rx-python FastAPI output. That has two important
// consequences:
//
//  1. Field declaration order matters. Go's encoding/json walks a struct's
//     fields in declaration order, so preserving Python's Pydantic field
//     order is how we keep key order identical in the emitted JSON.
//
//  2. Optional fields use pointer types (e.g. *int, *string) with an
//     explicit json tag — no omitempty — to emit "null" instead of
//     dropping the key. This matches FastAPI's default behavior
//     (exclude_none=False). A small set of fields DO use omitempty;
//     each one is called out at the struct where it appears.
//
// This package depends only on the standard library ("time", "encoding/json",
// "fmt", "strconv", "strings"). It has no imports from internal/ — cycle
// avoidance is deliberate.
package rxtypes
