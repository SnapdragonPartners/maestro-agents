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
// This package runs code states. Model states (a tool loop through
// maestro-llms) arrive next, with the write-ahead model turn, tool
// permissions, attempt markers and results.
package agent
