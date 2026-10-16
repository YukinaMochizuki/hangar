package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestTheFrontDoorNeedsAnACMEEmail: with --cp-domain and no --acme-email
// the edge refuses to start; with the front door off it needs none.
func TestTheFrontDoorNeedsAnACMEEmail(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	err := run(context.Background(), flags{cpDomain: "cp.example.com", domain: "user.example.com"}, log)
	if err == nil || !strings.Contains(err.Error(), "--acme-email") {
		t.Errorf("front door without --acme-email: %v", err)
	}
	if err := checkFlags(flags{cpDomain: "cp.example.com", acmeMail: "ops@example.com"}); err != nil {
		t.Errorf("front door with --acme-email: %v", err)
	}
	if err := checkFlags(flags{listen: "127.0.0.1:8090"}); err != nil {
		t.Errorf("front door off, no --acme-email: %v", err)
	}
}
