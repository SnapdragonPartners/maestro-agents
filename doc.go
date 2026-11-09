// Package agents is the foundation library for building Maestro agents.
//
// An agent built on this module runs either as a standalone executable or
// under an orchestrator (Maestro, or Morris's orchestration layer). LLM access
// goes through github.com/SnapdragonPartners/maestro-llms.
//
// The module is being built toward its MVP. Agent types and the run
// controller are in the agent package, and the durable records and the
// store interface in the store package; see the README and docs/adr for the
// current state and the decisions behind it.
package agents
