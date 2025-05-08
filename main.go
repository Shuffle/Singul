package main

import (
	"os"
	//"fmt"
	"log"
	"github.com/spf13/cobra"
)

func runSingul(args []string, flags map[string]string) {
	log.Printf("Running Singul with args: %v and flags: %v\n", args, flags)
}

func rootCmdRun(cmd *cobra.Command, args []string) {
	parsedArgs := []string{}
    parsedFlags := map[string]string{}

	for i := 0; i < len(args); i++ {
		if args[i][0] == '-' {
			// handle both -f val and --flag=val
			if i+1 < len(args) && args[i+1][0] != '-' && !containsEqual(args[i]) {
				parsedFlags[args[i]] = args[i+1]
				i++
			} else if containsEqual(args[i]) {
				parts := splitEqual(args[i])
				parsedFlags[parts[0]] = parts[1]
			} else {
				parsedFlags[args[i]] = ""
			}
		} else {
			parsedArgs = append(parsedArgs, args[i])
		}
	}

	if len(parsedArgs) < 2 {
		log.Println("\n\nPlease provide at least two arguments.")
		log.Printf("  Usage: singul <command> <api> [<args>]")
		log.Printf("Example: singul list_tickets jira --max_tickets=10")
		return
	}

	runSingul(parsedArgs, parsedFlags)
}

// Utility helpers
func containsEqual(s string) bool {
    for i := 1; i < len(s); i++ {
        if s[i] == '=' {
            return true
        }
    }
    return false
}

func splitEqual(s string) [2]string {
    for i := 1; i < len(s); i++ {
        if s[i] == '=' {
            return [2]string{s[:i], s[i+1:]}
        }
    }
    return [2]string{s, ""}
}

var rootCmd = &cobra.Command{
	Use:   "mycli",
	Short: "A CLI that accepts arbitrary arguments",
	Args:  cobra.ArbitraryArgs, 
	DisableFlagParsing: true,
	Run: rootCmdRun,
}

func main() {
	// Register subcommands to the math command
	if err := rootCmd.Execute(); err != nil {
        log.Printf("%#v", err)
        os.Exit(1)
    }
}
