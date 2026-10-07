// Command upstream-probe opens a live PostgreSQL database in upstream-schema
// mode and exercises every read path the Go service uses. It is a cutover
// diagnostic: the session is forced read-only, so no write can reach the
// database even if a bug slips in.
//
// Usage:
//
//	HINDSIGHT_GO_DATABASE_URL=postgres://... upstream-probe
//
// The DSN is never printed.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/storepg"
)

func main() {
	dsn := os.Getenv("HINDSIGHT_GO_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("UPSTREAM_PROBE_DSN")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "upstream-probe: set HINDSIGHT_GO_DATABASE_URL")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s, err := storepg.New(ctx, dsn, storepg.WithUpstreamSchema(), storepg.WithReadOnlySession())
	if err != nil {
		fmt.Fprintf(os.Stderr, "upstream-probe: connect failed: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	banks, err := s.BankIDs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upstream-probe: list banks failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("banks=%d\n", len(banks))

	failures := 0
	report := func(bank, what string, err error) {
		if err != nil {
			failures++
			fmt.Printf("bank=%s %s FAILED: %v\n", bank, what, err)
		}
	}

	for _, bank := range banks {
		if _, err := s.GetBank(ctx, bank); err != nil {
			report(bank, "get_bank", err)
		}
		if _, err := s.BankMission(ctx, bank); err != nil {
			report(bank, "bank_mission", err)
		}
		models, err := s.ListMentalModels(ctx, bank)
		report(bank, "list_mental_models", err)
		directives, err := s.ListDirectives(ctx, bank)
		report(bank, "list_directives", err)
		units, unitTotal, err := s.ListMemories(ctx, bank, 5, 0)
		report(bank, "list_memories", err)
		docs, docTotal, err := s.ListDocuments(ctx, bank, 5, 0)
		report(bank, "list_documents", err)
		tags, err := s.ListTags(ctx, bank)
		report(bank, "list_tags", err)
		facts, err := s.UnconsolidatedFacts(ctx, bank, 1)
		report(bank, "unconsolidated_facts", err)

		fmt.Printf("bank=%s mental_models=%d directives=%d memories=%d/%d documents=%d/%d tags=%d sample_units=%d unconsolidated_sample=%d\n",
			bank, len(models), len(directives), len(units), unitTotal, len(docs), docTotal, len(tags), len(units), len(facts))
	}

	if failures > 0 {
		fmt.Printf("read-only probe FAILED: %d query group(s)\n", failures)
		os.Exit(1)
	}
	fmt.Println("read-only probe OK")
}
