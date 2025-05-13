package main

import (
	"os"
	//"fmt"
	"log"
	"context"
	"strings"
	"github.com/spf13/cobra"

	"singul/pkg"
	"github.com/shuffle/shuffle-shared"
)

// Singul -> Shuffle-shared?
// OR
// Shuffle-shared -> Singul?
func runSingul(args []string, flags map[string]string) {
	log.Printf("Running Singul with args: %v and flags: %v\n", args, flags)

	ctx := context.Background()
	value := shuffle.CategoryAction{
		Label: args[0],
		AppName: args[1],

		SkipWorkflow: true,
	}

	parsedFields := []shuffle.Valuereplace{}
	for key, value := range flags {
		if strings.HasPrefix(key, "--") {
			key = strings.TrimPrefix(key, "--")
		} else if strings.HasPrefix(key, "-") {
			key = strings.TrimPrefix(key, "-")
		}

		parsedFields = append(parsedFields, shuffle.Valuereplace{
			Key:   key,
			Value: value,
		})
	}

	value.Fields = parsedFields

	// Make a fake ResponseWrite that can actaully receive data
	data, err := singul.RunAction(ctx, value)
	if err != nil {
		log.Printf("[ERROR] Failed running action: %v", err)
	}

	log.Printf("API OUTPUT: %s", string(data))
	if err != nil { 
		log.Printf("ERROR: %s", err.Error())
	}
}

func rootCmdRun(cmd *cobra.Command, args []string) {
	parsedArgs := []string{}

    parsedFlags := map[string]string{}
	for i := 0; i < len(args); i++ {
		log.Printf("ARGS: %#v", args[i])
		if args[i][0] == '-' {
			// handle both -f val, --flag=val and --flag val
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

	for _, parsedFlag := range parsedFlags {
		if parsedFlag == "--help" || parsedFlag == "--h" {
			log.Println("\n\nPlease provide at least two arguments.")
			log.Printf("  Usage: singul <command> <api> [<args>]")
			log.Printf("Example: singul list_tickets jira --max_tickets=10")
			return
		}
	}

	if len(parsedArgs) < 2 {
		log.Println("\n\nPlease provide at least two arguments.")
		log.Printf("  Usage: singul <command> <api> [<args>]")
		log.Printf("Example: singul list_tickets jira --max_tickets=10")
		return
	}

	// Send request to singul.io/apps/{appname}
	// This is an API key that is public for Algolia
	// It has restrictions per IP address as to avoid 
	// Use the Algolia API to search for apps with the name, before asking for the public app from shuffler.io
	os.Setenv("ALGOLIA_PUBLICKEY", "14bfd695f2152664bb16ca8e8c3e8281")
	if len(os.Getenv("SHUFFLE_BACKEND")) == 0 {
		os.Setenv("SHUFFLE_BACKEND", "https://shuffler.io")
		os.Setenv("SHUFFLE_CLOUDRUN_URL", "https://shuffler.io")
	}

	// Ensures Singul runs in "standalone mode"
	os.Setenv("STANDALONE", "true")
	if len(os.Getenv("FILE_LOCATION")) == 0 {

		shuffleLocation := os.Getenv("SHUFFLE_FILE_LOCATION")
		if len(shuffleLocation) > 0 {
			os.Setenv("FILE_LOCATION", shuffleLocation)
		} else {
			os.Setenv("FILE_LOCATION", "./files")
		}
	}

	if strings.ToLower(parsedArgs[0]) == "authenticate" {
		singul.AuthenticateAppCli(parsedArgs[1])
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
	Use:   "Singul CLI",
	Short: "A CLI that accepts arbitrary arguments",
	Args:  cobra.ArbitraryArgs, 
	DisableFlagParsing: true,
	Run: rootCmdRun,
}

func main() {
	/*
	translateData := `{
	  "fields": {
		"project": {
		  "key": "SHUF"
		},
		"summary": "{{summary}}",
		"issuetype": {
		  "name": "{{issuetype[]}}"
		}
	  }
	}`

	data := []shuffle.Valuereplace{
		shuffle.Valuereplace{
			Key: "body",
			Value: translateData,
		},
	}
	resp := shuffle.TranslateBadFieldFormats(data)
	log.Printf("RESP: %#v", resp[0].Value)
	os.Exit(3)
	*/


	// Register subcommands to the math command
	if err := rootCmd.Execute(); err != nil {
        log.Printf("%#v", err)
        os.Exit(1)
    }
}
