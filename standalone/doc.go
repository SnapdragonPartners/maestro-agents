// Package standalone is the standalone host (MODE-2 to MODE-7, ADR 0017):
// the small program, shipped with the library, that runs an agent type from
// a declarative JSON configuration.
//
// An agent's executable registers its agent types and calls Main:
//
//	func main() {
//		reg := agent.NewRegistry()
//		if err := reg.Register(myagent.Type()); err != nil {
//			log.Fatal(err)
//		}
//		standalone.Main(standalone.Program{Name: "myagent", Registry: reg})
//	}
//
// The configuration chooses the model provider: any maestro-llms provider
// ID, built with maestro-llms' providers.New. Program.Providers adds or
// replaces a provider for a route the configuration cannot express, such
// as Vertex AI.
//
// The executable then has two commands:
//
//	myagent run -config FILE [-input FILE]
//	myagent config validate -config FILE
//
// A one_shot trigger processes the -input file and exits; a folder trigger
// watches a folder and processes each new file until SIGINT or SIGTERM.
// Each input is one instance and one run, identified by the file's SHA-256,
// so a file is processed once per store. The run's trigger payload is a
// FileTrigger; an agent type reads the input with FileTrigger.Read.
//
// Exit codes: ExitComplete (0), ExitIncomplete (1) and ExitUsage (2).
//
// An agent's own commands, or a harness such as the scoring package, can
// open the host in process instead (ADR 0020, item 2; ADR 0021): Open
// loads and validates any configuration. Host.Watch runs a folder
// configuration's watch in process, as run does (ADR 0022), while the
// caller submits runs beside it.
// Host.RunInput runs one input as run -input does and returns its Report;
// it needs a one_shot trigger (ErrNotOneShot otherwise), and
// Host.TriggerKind says which the configuration has. Host.Submit runs one
// of the program's submitted agent types (Program.Submitted, trigger kind
// SubmittedKind) on a caller's instance, from a payload rather than a
// file, serialized with every other run on that instance (ErrBusy while
// one is unfinished). Host.Store and Host.Namespace read what the runs
// wrote. SignalContext gives such a
// command the host's signal lifecycle: the first SIGINT or SIGTERM cancels
// its context, so a run shuts down at its next step boundary, and a second
// ends the process.
package standalone
