package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

func main() {
	config := flag.String("config", "/etc/lidza-agent/config.json", "existing source agent configuration; no listeners are opened")
	app := flag.String("app", "", "optional source application ID")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plans, err := agent.InspectMigrationPlans(ctx, *config, *app)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(plans); err != nil {
		fmt.Fprintln(os.Stderr, "could not write migration review")
		os.Exit(1)
	}
}
