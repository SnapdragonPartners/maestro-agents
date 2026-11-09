// Package agent defines agent types and runs their activations.
//
// An agent type ([Type]) is a state machine whose state data is the agent
// type's own Go type (STM-1 to STM-5). A host registers its agent types in a
// [Registry] (MAN-3), claims an instance through the store's control plane,
// and passes the claim to [Controller.Activate], which runs the instance's
// run from its last step boundary (ADR 0003, ADR 0007).
//
// Every durable write an activation makes is one fenced, atomic commit
// (ADR 0005): the execution record's entries (ADR 0004), the checkpoint at
// every step boundary, a code state's object operations with its
// transition, and the run's ending with the release of the instance.
// Activate returns an [Outcome] only when the commit of it is confirmed;
// otherwise it returns an error and the host rereads the run record before
// acting.
//
// A model state (ADR 0008) runs a tool loop through maestro-llms' toolloop
// with the host's model client (LLM-1). Every attempt is recorded before it
// is sent and every turn before its calls execute; every tool call passes
// the host's policy hook (TOOL-3), and its permission, attempt and result
// are recorded in that order (STM-6). A tool's result can carry a
// transition (STM-3), and an atomic local transaction's object operations
// commit with its result (ADR 0005). An activation continuing mid-turn
// resolves each recorded call by STM-6's partition without asking the model
// again.
package agent
