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
// open the host in process instead (ADR 0020, item 2): Open loads a
// configuration with a one_shot trigger, Host.RunInput runs one input as
// run -input does and returns its Report, and Host.Store and
// Host.Namespace read what the runs wrote. SignalContext gives such a
// command the host's signal lifecycle: the first SIGINT or SIGTERM cancels
// its context, so a run shuts down at its next step boundary, and a second
// ends the process.
package standalone
