package webapi

// ContractVersion is the version of the HTTP wire contract this backend
// speaks, as MAJOR.MINOR.
//
// It is reported by GET /health and is how a client decides whether it can
// talk to this server at all:
//
//   - Bump the MINOR for an additive change: a new field, a new endpoint,
//     a new optional parameter. Existing clients keep working.
//   - Bump the MAJOR for a breaking change: a renamed or removed field, a
//     changed meaning, a changed status code. Clients that do not know the
//     major must refuse to run.
//
// rx-python declares the same value (`src/rx/contract.py`) and the two must
// be changed together, in the same task, with a changelog entry in both.
// `rx-viewer` reads it from /health.
const ContractVersion = "1.0"
