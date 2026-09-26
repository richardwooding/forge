// Package grpcapi holds the generated protobuf and gRPC code for forge's
// ToolService, and exists so that clients can import it.
//
// It is part of forge's own module rather than a nested one, which is a change
// from the original plan and worth explaining. The plan put the gRPC surface in
// a nested module to keep grpc and protobuf out of the core. That does not work
// here for two independent reasons. A nested module cannot import the parent's
// internal packages, so the server implementation could not live there; and the
// generated types have to be importable by clients, so they cannot live in
// internal either. Nesting would also have bought nothing in practice: cmd/forge
// links the server, so grpc is a real dependency of the forge binary whichever
// module declares it.
//
// The guest SDK is still a separate module, where the argument does hold: a
// tool's wasm binary must not carry forge's dependencies, and nothing in the
// SDK needs forge's internals.
package grpcapi
