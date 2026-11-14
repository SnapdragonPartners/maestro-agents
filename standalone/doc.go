// Package standalone is the standalone host (MODE-2 to MODE-7, ADR 0017):
// the small program, shipped with the library, that runs an agent type from
// a declarative JSON configuration.
//
// An agent's executable registers its agent types, lists the model
// providers it offers, and calls Main:
//
//	func main() {
//		reg := agent.NewRegistry()
//		if err := reg.Register(myagent.Type()); err != nil {
//			log.Fatal(err)
//		}
//		standalone.Main(standalone.Program{
//			Name:      "myagent",
//			Registry:  reg,
//			Providers: map[llms.ProviderID]standalone.Provider{llms.ProviderAnthropic: newAnthropic},
//		})
//	}
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
package standalone
