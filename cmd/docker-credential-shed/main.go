// docker-credential-shed is the guest-side Docker credential helper that runs
// inside shed microVMs. Docker execs this binary on demand to resolve registry
// credentials. It translates credential helper protocol operations into message
// bus requests to the host agent.
//
// This is a one-shot CLI, not a daemon. Docker execs it per operation:
//
//	echo "us-docker.pkg.dev" | docker-credential-shed get
//	docker-credential-shed list
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	sdk "github.com/charliek/shed/sdk"

	"github.com/charliek/shed/internal/ext/dockercred"
	"github.com/charliek/shed/internal/version"
)

const requestTimeout = 5 * time.Second

// credentialsNotFoundMessage is the exact string Docker's credential-helper
// protocol recognizes on a helper's stdout (with a non-zero exit) as "no
// credentials for this registry", which makes Docker proceed with an anonymous
// pull instead of aborting. It must match
// github.com/docker/docker-credential-helpers/credentials.errCredentialsNotFoundMessage.
const credentialsNotFoundMessage = "credentials not found in native keychain"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "docker-credential-shed %s\n", version.Info())
		fmt.Fprintf(os.Stderr, "usage: docker-credential-shed <get|list|store|erase>\n")
		os.Exit(1)
	}

	publishURL := sdk.DefaultPublishURL
	if v := os.Getenv("SHED_PUBLISH_URL"); v != "" {
		publishURL = v
	}

	command := os.Args[1]

	switch command {
	case "get":
		doGet(publishURL)
	case "list":
		doList(publishURL)
	case "store", "erase":
		fmt.Fprintf(os.Stderr, "docker-credential-shed: %s not supported (read-only credential broker)\n", command)
		os.Exit(1)
	case "version":
		fmt.Fprintf(os.Stderr, "docker-credential-shed %s\n", version.FullInfo())
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "docker-credential-shed: unknown command %q\n", command)
		os.Exit(1)
	}
}

func doGet(publishURL string) {
	os.Exit(runGet(publishURL, os.Stdin, os.Stdout, os.Stderr))
}

// runGet executes the credential-helper "get" operation and returns the process
// exit code. It is split out from doGet so the Docker not-found contract can be
// unit-tested without spawning a process.
func runGet(publishURL string, stdin io.Reader, stdout, stderr io.Writer) int {
	input, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "docker-credential-shed: reading stdin: %s\n", err)
		return 1
	}

	serverURL := strings.TrimSpace(string(input))
	if serverURL == "" {
		fmt.Fprintf(stderr, "docker-credential-shed: empty server URL\n")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	helper := dockercred.New(dockercred.WithPublishURL(publishURL))
	resp, err := helper.Get(ctx, serverURL)
	if err != nil {
		// When the host broker has no credential to serve, speak Docker's
		// protocol: print the not-found message to stdout and exit non-zero, so
		// Docker falls back to an anonymous pull (the correct outcome for public
		// images). Anything else is a genuine fault → stderr, no stdout.
		if errors.Is(err, dockercred.ErrCredentialsNotFound) {
			fmt.Fprintln(stdout, credentialsNotFoundMessage)
			return 1
		}
		fmt.Fprintf(stderr, "docker-credential-shed: %s\n", err)
		return 1
	}

	// Docker expects PascalCase JSON fields from credential helpers
	out := struct {
		ServerURL string `json:"ServerURL"`
		Username  string `json:"Username"`
		Secret    string `json:"Secret"`
	}{
		ServerURL: resp.ServerURL,
		Username:  resp.Username,
		Secret:    resp.Secret,
	}

	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "docker-credential-shed: encoding response: %s\n", err)
		return 1
	}
	return 0
}

func doList(publishURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	helper := dockercred.New(dockercred.WithPublishURL(publishURL))
	registries, err := helper.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker-credential-shed: %s\n", err)
		os.Exit(1)
	}

	if err := json.NewEncoder(os.Stdout).Encode(registries); err != nil {
		fmt.Fprintf(os.Stderr, "docker-credential-shed: encoding response: %s\n", err)
		os.Exit(1)
	}
}
