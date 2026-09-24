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
// rx-python declares its own value (`src/rx/contract.py`). While rx-python
// is paused, a bump here is a row in ../tickets/PARITY-DEBT.md rather than
// a change there. `rx-viewer` reads it from /health and refuses a major it
// does not know.
const ContractVersion = "1.7"
