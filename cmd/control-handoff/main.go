package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agim/lidza-deploy/internal/control"
)

func main() {
	source := flag.String("source", "", "absolute retained control data directory")
	mapping := flag.String("mapping", "", "private JSON file with original origin and all agent endpoint mappings")
	output := flag.String("output", "", "new offline staging directory; no service is started")
	expected := flag.String("expected-sha256", "", "source fingerprint from a fresh reviewed snapshot")
	flag.Parse()
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	f, err := os.Open(*mapping)
	if err != nil {
		fail(fmt.Errorf("mapping file unavailable"))
	}
	defer f.Close()
	var request control.HandoffRequest
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(fmt.Errorf("invalid mapping file"))
	}
	var review control.HandoffReview
	if *output == "" {
		review, err = control.ReviewControlHandoff(*source, request)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		review, err = control.StageControlHandoff(ctx, *source, *output, *expected, request)
	}
	if err != nil {
		fail(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(review); err != nil {
		fail(err)
	}
}
