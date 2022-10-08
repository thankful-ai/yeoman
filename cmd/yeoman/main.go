package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		switch {
		case errors.Is(err, emptyArgError{}):
			usage()
		default:
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(1)
	}
}

func run() error {
	minMax := flag.String("n", "1", "min and max of servers (e.g. 3:5)")
	containerName := flag.String("c", "", "container name")
	flag.Parse()

	arg, tail := parseArg(flag.Args())
	switch arg {
	case "init":
		return nil
	case "service":
		return service(tail, serviceOpts{
			minMax:        *minMax,
			containerName: *containerName,
		})
	case "deploy":
		return nil
	case "status":
		return nil
	case "version":
		fmt.Println("v0.0.0-alpha")
		return nil
	case "", "help":
		return emptyArgError{}
	default:
		return badArgError(arg)
	}
}

type serviceOpts struct {
	// minMax in the form of "3:5".
	minMax        string
	containerName string
}

func service(args []string, opts serviceOpts) error {
	arg, _ := parseArg(args)
	switch arg {
	case "create":
		// Notifies the service over API.
	case "", "help":
		return emptyServiceArgError{}
	default:
		return badArgError(arg)
	}
	return nil
}

// parseArg splits the arguments into a head and tail.
func parseArg(args []string) (string, []string) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return args[0], args[1:]
	}
}

type config struct {
	ip string
}

func parseConfig() (config, error) {
}

type emptyArgError struct{}

func (e emptyArgError) Error() string { return "empty arg" }

type emptyServiceArgError struct{}

func (e emptyServiceArgError) Error() string {
	return "usage: yeoman service [create|update|destroy] ..."
}

type badArgError string

func (e badArgError) Error() string {
	return fmt.Sprintf("unknown argument: %s", string(e))
}

func usage() {
	fmt.Println(`usage: yeoman [init|service|deploy|status|version] ...`)
}
